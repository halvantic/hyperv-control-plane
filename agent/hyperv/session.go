package hyperv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

/* One PowerShell process for a pass, instead of one per call.

   Measured on HVNEW06, 2026-09-30, in one fresh powershell.exe: the nine reads
   EnsureVM makes cost 1.85s the first time each runs in a process and 0.39s the
   second (Get-VM 523ms then 31ms, Get-VHD 406ms then 69ms). Launch itself is
   only 0.38s. So a process per VM spent ~1.8s warming up before doing any work,
   fourteen times a pass: most of EnsureVM's 43s.

   A session keeps one process alive across the calls of a pass. Callers are
   unchanged: each script is still sent on its own, when its turn comes, so the
   reconciler's per-VM checks (a job holding the VM, a drain, a host powering
   off) still run immediately before each VM is touched. Only the process and
   the modules loaded in it are shared. A single script covering every VM would
   have been simpler and would have run a VM's reconcile up to twenty seconds
   after the check that said it was free.

   Opt-in per call (runPooled), because only scripts known to be safe in a
   shared process may use it. The server runs each script as a child scope, so
   variables do not carry from one call to the next, but a script that calls
   exit would end the session, and one using $script: or Add-Type would leave
   state behind for the next. EnsureVM and GetVMState use none of those.

   Anything going wrong with the session itself (it will not start, it dies, a
   write fails before the script is sent) falls back to a fresh process, which
   is exactly what ran before. A script that FAILS is not a session problem: its
   error comes back the way a fresh process would have reported it. */

type sessionKey struct{}

// Session opens a shared PowerShell process for the calls made under the
// returned context, started on first use. end stops it; call it when the pass
// is done.
//
// Inside a context that already carries one, it joins that session and end
// does nothing: the runner opens one for the whole cycle, and ReconcileVMs,
// which also opens one so it stands on its own, then shares it rather than
// paying the warm-up a second time.
func (p *PowerShell) Session(ctx context.Context) (context.Context, func()) {
	if s, ok := ctx.Value(sessionKey{}).(*psSession); ok && s.owner == p {
		return ctx, func() {}
	}
	s := &psSession{owner: p, ctx: ctx}
	return context.WithValue(ctx, sessionKey{}, s), s.close
}

// runPooled runs a script in the context's session when there is one, and in a
// fresh process otherwise. Only for scripts that are safe to share a process:
// see above.
func (p *PowerShell) runPooled(ctx context.Context, script string) ([]byte, error) {
	if s, ok := ctx.Value(sessionKey{}).(*psSession); ok && s.owner == p {
		start := time.Now()
		if out, handled, err := s.call(ctx, script); handled {
			p.timed(start)
			return out, err
		}
	}
	return p.run(ctx, script)
}

// sessionServer is the loop the session process runs. Each request is one line
// on stdin: an id and the script, base64 so a multi-line script cannot be
// mistaken for the end of the request. Each response is one line on stdout,
// tagged with the same id; anything else a script writes straight to the host
// (Write-Host, a warning that escaped) is on lines of its own, which the reader
// skips.
//
// The script runs as a child scope (& on a new scriptblock), so what it assigns
// is gone when it returns. The loop's own variables carry a prefix no script
// here uses, because a child scope can still READ its parent's.
//
// Warnings and the verbose, debug and information streams are discarded: a
// fresh process wrote them to stdout ahead of the JSON, where decodeJSON
// skipped them, so nothing ever read them.
const sessionServer = `$ErrorActionPreference = 'Continue'
while ($true) {
  $__bLine = [Console]::In.ReadLine()
  if ($null -eq $__bLine) { break }
  $__bSp = $__bLine.IndexOf(' ')
  $__bId = $__bLine.Substring(0, $__bSp)
  $__bRes = @{ ok = $true; out = ''; err = '' }
  try {
    $__bSrc = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($__bLine.Substring($__bSp + 1)))
    $__bOut = @(& ([scriptblock]::Create($__bSrc)) 3>$null 4>$null 5>$null 6>$null)
    $__bRes.out = ($__bOut | ForEach-Object { if ($_ -is [string]) { $_ } else { ($_ | Out-String -Width 4096).TrimEnd() } }) -join "` + "`" + `n"
  } catch {
    $__bRes.ok = $false
    $__bRes.err = ($_ | Out-String)
  }
  $__bJson = [pscustomobject]$__bRes | ConvertTo-Json -Compress
  [Console]::Out.WriteLine('BALLAST-SESSION ' + $__bId + ' ' + [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($__bJson)))
  [Console]::Out.Flush()
}
`

type psSession struct {
	owner *PowerShell
	ctx   context.Context // the pass; the process dies with it

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr bytes.Buffer
	seq    int
	dead   bool
}

type sessionResponse struct {
	OK  bool   `json:"ok"`
	Out string `json:"out"`
	Err string `json:"err"`
}

func (s *psSession) start() error {
	cmd := exec.CommandContext(s.ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", sessionServer)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd, s.stdin = cmd, stdin
	// A reader of its own, so a call can give up on a cancelled context rather
	// than sit in a blocking read. Lines can be long (a full VM reading, base64),
	// so bufio.Reader rather than a Scanner with its token limit.
	s.lines = make(chan string, 64)
	go func() {
		defer close(s.lines)
		r := bufio.NewReader(stdout)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				s.lines <- strings.TrimRight(line, "\r\n")
			}
			if err != nil {
				return
			}
		}
	}()
	return nil
}

// call runs one script. handled is false only when the script was never sent,
// so the caller can run it in a fresh process instead without running it twice.
func (s *psSession) call(ctx context.Context, script string) (out []byte, handled bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return nil, false, nil
	}
	if s.cmd == nil {
		if serr := s.start(); serr != nil {
			s.dead = true
			s.owner.log.Warn("powershell session did not start; using a process per call", "err", serr)
			return nil, false, nil
		}
	}
	s.seq++
	id := strconv.Itoa(s.seq)
	start := time.Now()
	req := id + " " + base64.StdEncoding.EncodeToString([]byte(script)) + "\n"
	if _, werr := io.WriteString(s.stdin, req); werr != nil {
		s.kill()
		s.owner.log.Warn("powershell session stopped taking requests; using a process per call", "err", werr)
		return nil, false, nil
	}
	prefix := "BALLAST-SESSION " + id + " "
	for {
		select {
		case line, open := <-s.lines:
			if !open {
				// The process ended mid-script. The script may have run partly, so it
				// is reported as this call's failure, as a crashed process was, and
				// not run again here.
				s.kill()
				detail, tidied := psFailureDetailTidied(ctx, start, "", s.stderr.String())
				return nil, true, wrapPSError(errSessionEnded, detail, tidied)
			}
			if !strings.HasPrefix(line, prefix) {
				continue // host output the script wrote around its result
			}
			out, err := decodeSessionResponse(ctx, start, strings.TrimPrefix(line, prefix))
			return out, true, err
		case <-ctx.Done():
			// Same as a fresh process killed on cancellation: the process goes, and
			// the error says it was cancelled rather than that it failed.
			s.kill()
			detail, tidied := psFailureDetailTidied(ctx, start, "", "")
			return nil, true, wrapPSError(ctx.Err(), detail, tidied)
		}
	}
}

var (
	errSessionEnded  = errors.New("the powershell session ended during the call")
	errScriptStopped = errors.New("exit status 1")
)

// decodeSessionResponse turns a response line into what execPowerShell would
// have returned for the same script: its output on success, and on failure an
// error built from the error record by the same tidying a fresh process's
// stderr gets.
func decodeSessionResponse(ctx context.Context, start time.Time, payload string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("powershell session: unreadable response: %w", err)
	}
	var resp sessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("powershell session: unreadable response: %w", err)
	}
	if resp.OK {
		return []byte(resp.Out), nil
	}
	detail, tidied := psFailureDetailTidied(ctx, start, resp.Out, resp.Err)
	return []byte(resp.Out), wrapPSError(errScriptStopped, detail, tidied)
}

// kill ends the process and marks the session unusable. Caller holds mu.
func (s *psSession) kill() {
	s.dead = true
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		go func(c *exec.Cmd) { _ = c.Wait() }(s.cmd)
	}
	s.cmd = nil
}

// close ends the session: stdin closes, the loop sees end of input and exits.
// A process that does not go within a few seconds is killed.
func (s *psSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		s.dead = true
		return
	}
	_ = s.stdin.Close()
	done := make(chan struct{})
	go func(c *exec.Cmd) { _ = c.Wait(); close(done) }(s.cmd)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
	}
	s.cmd = nil
	s.dead = true
}

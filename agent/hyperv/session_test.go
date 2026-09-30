package hyperv

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* The session has to be invisible: a script run in it must succeed, fail and
   report exactly as it would in a fresh process, or the saving is paid for with
   a console that says something different. These run real powershell.exe, both
   ways, and compare. */

func needPowerShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("needs powershell.exe")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not on PATH")
	}
}

func newTestSession(t *testing.T) (*psSession, context.Context) {
	t.Helper()
	needPowerShell(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s := &psSession{owner: NewPowerShell(nil), ctx: ctx}
	t.Cleanup(func() { s.close(); cancel() })
	return s, ctx
}

func sessionRun(t *testing.T, s *psSession, ctx context.Context, script string) ([]byte, error) {
	t.Helper()
	out, handled, err := s.call(ctx, script)
	if !handled {
		t.Fatalf("the session did not take the script")
	}
	return out, err
}

// The error an operator reads must not depend on which path ran the script.
func TestSessionErrorsReadAsAFreshProcessWould(t *testing.T) {
	s, ctx := newTestSession(t)
	for name, script := range map[string]string{
		"a thrown sentence": `$ErrorActionPreference = 'Stop'
throw ('the LUN already contains a ReFS volume; use "Wipe and adopt" to reuse it')`,
		"a cmdlet error": `$ErrorActionPreference = 'Stop'
Get-Item -LiteralPath 'Z:\ballast\definitely\not\here' -ErrorAction Stop`,
		"a long message that wraps": `$ErrorActionPreference = 'Stop'
throw ('refusing to create C:\ClusterStorage\Volume1\Some Long Folder Name\Another Level\web01-data-disk.vhdx: its directory cannot be read or created from this host (Access is denied) - the volume may be offline, still rebuilding, or owned by another node. Not creating a new disk over a path that cannot be read')`,
		"a throw after output": `$ErrorActionPreference = 'Stop'
'NOTE some progress'
throw 'failed part-way'`,
	} {
		t.Run(name, func(t *testing.T) {
			_, fresh := execPowerShell(ctx, script)
			_, pooled := sessionRun(t, s, ctx, script)
			if fresh == nil || pooled == nil {
				t.Fatalf("both must fail: fresh=%v session=%v", fresh, pooled)
			}
			if fresh.Error() != pooled.Error() {
				t.Fatalf("the session reports a different error\nfresh:   %q\nsession: %q", fresh.Error(), pooled.Error())
			}
		})
	}
}

// A successful script's output decodes to the same result both ways, with the
// NOTE lines and warnings a fresh process wrote ahead of the JSON.
func TestSessionOutputDecodesAsAFreshProcessWould(t *testing.T) {
	s, ctx := newTestSession(t)
	script := `$ErrorActionPreference = 'Stop'
Write-Warning 'switch not found on this host; net0 left disconnected'
Write-Output 'NOTE net0 is not connected to a switch'
[pscustomobject]@{ created = $false; changed = $true; pendingPowerOff = $false; pendingDetail = 'memory; processor count' } | ConvertTo-Json -Compress`
	type result struct {
		Created, Changed, PendingPowerOff bool
		PendingDetail                     string
	}
	var fresh, pooled result
	fo, ferr := execPowerShell(ctx, script)
	po, perr := sessionRun(t, s, ctx, script)
	if ferr != nil || perr != nil {
		t.Fatalf("fresh=%v session=%v", ferr, perr)
	}
	if err := decodeJSON(fo, &fresh); err != nil {
		t.Fatal(err)
	}
	if err := decodeJSON(po, &pooled); err != nil {
		t.Fatalf("session output does not decode: %v\n%s", err, po)
	}
	if fresh != pooled || !pooled.Changed || pooled.PendingDetail != "memory; processor count" {
		t.Fatalf("results differ\nfresh:   %+v\nsession: %+v", fresh, pooled)
	}
}

// One VM's variables must not become the next VM's. A child scope can still
// read its parent's, which is why the server's own names are prefixed.
func TestSessionDoesNotCarryVariablesBetweenCalls(t *testing.T) {
	s, ctx := newTestSession(t)
	if _, err := sessionRun(t, s, ctx, `$vp = 'first VM'; $changed = $true; 'ok'`); err != nil {
		t.Fatal(err)
	}
	out, err := sessionRun(t, s, ctx, `'[' + $vp + '|' + $changed + ']'`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "[|]" {
		t.Fatalf("a variable from the previous call is visible: %s", got)
	}
}

// A script that fails is that script's failure, not the session's: the next
// VM is still served.
func TestSessionSurvivesAFailedScript(t *testing.T) {
	s, ctx := newTestSession(t)
	if _, err := sessionRun(t, s, ctx, `$ErrorActionPreference = 'Stop'; throw 'one VM failed'`); err == nil {
		t.Fatal("the failure must be reported")
	}
	out, err := sessionRun(t, s, ctx, `'{"next":true}'`)
	if err != nil || !strings.Contains(string(out), `"next":true`) {
		t.Fatalf("the session must serve the next call after a failure: out=%q err=%v", out, err)
	}
}

// Output written straight to the host lands on stdout around the response,
// and must not be taken for it.
func TestSessionIgnoresHostOutputAroundTheResult(t *testing.T) {
	s, ctx := newTestSession(t)
	out, err := sessionRun(t, s, ctx, `Write-Host 'BALLAST-SESSION 1 not-a-response'; Write-Host 'noise'; '{"a":1}'`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != `{"a":1}` {
		t.Fatalf("host output leaked into the result: %q", out)
	}
}

// A cancelled call reads as cancelled, as a killed process did, and the session
// is then out of service: the next call is not handled, so the caller runs it
// in a fresh process.
func TestSessionCancellationReportsItAndHandsBack(t *testing.T) {
	s, ctx := newTestSession(t)
	if _, err := sessionRun(t, s, ctx, `'warm'`); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	begun := time.Now()
	_, handled, err := s.call(cctx, `Start-Sleep -Seconds 30; 'late'`)
	if !handled || err == nil {
		t.Fatalf("a cancelled call must be reported: handled=%v err=%v", handled, err)
	}
	if time.Since(begun) > 10*time.Second {
		t.Fatalf("cancellation took %s; the call must not wait out the script", time.Since(begun))
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "time limit") {
		t.Fatalf("the error must say the call was cancelled, got %v", err)
	}
	if _, handled, _ := s.call(ctx, `'after'`); handled {
		t.Fatal("a session whose process was killed must hand calls back")
	}
}

// Closed means closed: nothing is sent to a session after its pass ends.
func TestSessionClosedHandsBack(t *testing.T) {
	s, ctx := newTestSession(t)
	if _, err := sessionRun(t, s, ctx, `'warm'`); err != nil {
		t.Fatal(err)
	}
	s.close()
	if _, handled, _ := s.call(ctx, `'after'`); handled {
		t.Fatal("a closed session must hand calls back")
	}
}

// The runner opens a session for the cycle and ReconcileVMs opens one too; the
// inner one must join the outer rather than start a second process, and ending
// it must not end the cycle's.
func TestANestedSessionJoinsTheOuterOne(t *testing.T) {
	p := NewPowerShell(nil)
	outer, endOuter := p.Session(context.Background())
	defer endOuter()
	inner, endInner := p.Session(outer)
	if inner != outer {
		t.Fatal("a session opened inside another must join it")
	}
	endInner()
	if s := outer.Value(sessionKey{}).(*psSession); s.dead {
		t.Fatal("ending the inner session ended the cycle's")
	}
}

// Without a session in the context, runPooled is exactly run.
func TestRunPooledWithoutASessionUsesAFreshProcess(t *testing.T) {
	f := &fakeRunner{}
	p := newTestPS(f)
	if _, err := p.runPooled(context.Background(), "'x'"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("runPooled must fall through to run, got %d runs", len(f.calls))
	}
}

// The real EnsureVM script, generated as it ships, against stubbed Hyper-V
// cmdlets: it must give the same result in a session as in a fresh process,
// and twice in a row in the same session.
func TestEnsureVMScriptRunsTheSameInASession(t *testing.T) {
	s, ctx := newTestSession(t)
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			ProcessorCount:       2,
			MemoryStartupBytes:   4294967296,
			AutomaticStartAction: types.VMStartIfWasRunning,
			Disks:                []types.VMDiskSpec{{Path: `C:\vms\Web01\web01.vhdx`}},
			NetworkAdapters:      []types.VMNetworkAdapterSpec{{Name: "net0", SwitchName: "sw", VLANID: 41}},
		},
	}
	stubs := `
function Get-VM { [CmdletBinding()] param($Name) [pscustomobject]@{ Name = 'Web01'; Id = 'id-1'; State = 'Running'; AutomaticStartAction = 'Nothing' } }
function Set-VM { [CmdletBinding()] param($Name, $AutomaticStartAction) }
function Get-VMProcessor { [CmdletBinding()] param($VMName) [pscustomobject]@{ Count = 2; ExposeVirtualizationExtensions = $false } }
function Get-VMMemory { [CmdletBinding()] param($VMName) [pscustomobject]@{ DynamicMemoryEnabled = $false; Startup = 4294967296 } }
function Get-VMFirmware { [CmdletBinding()] param($VMName) [pscustomobject]@{ SecureBoot = 'On'; SecureBootTemplate = 'MicrosoftWindows'; BootOrder = @() } }
function Get-VMSecurity { [CmdletBinding()] param($VMName) [pscustomobject]@{ TpmEnabled = $false } }
function Get-VMHardDiskDrive { [CmdletBinding()] param($VMName) [pscustomobject]@{ Path = 'C:\vms\Web01\web01.vhdx' } }
function Get-VHD { [CmdletBinding()] param($Path) [pscustomobject]@{ Path = $Path; ParentPath = $null; Size = 1 } }
function Get-VMNetworkAdapter { [CmdletBinding()] param($VMName, $Name) [pscustomobject]@{ Name = 'net0'; Id = 'id-net0'; SwitchName = 'sw'; MacAddressSpoofing = 'Off' } }
function Get-VMNetworkAdapterVlan { [CmdletBinding()] param($VMName, $VMNetworkAdapterName, $VMNetworkAdapter) [pscustomobject]@{ OperationMode = 'Access'; AccessVlanId = 41; ParentAdapter = [pscustomobject]@{ Id = 'id-net0' } } }
function Set-VMNetworkAdapterVlan { [CmdletBinding()] param($VMName, $VMNetworkAdapterName, [switch]$Access, $VlanId, [switch]$Untagged) }
function Set-VMNetworkAdapter { [CmdletBinding()] param($VMNetworkAdapter, $VMName, $Name, $MacAddressSpoofing, $StaticMacAddress) }
function Get-VMDvdDrive { [CmdletBinding()] param($VMName) }
`
	script := stubs + newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)

	fo, ferr := execPowerShell(ctx, script)
	if ferr != nil {
		t.Fatalf("the script fails in a fresh process: %v", ferr)
	}
	for i := 0; i < 2; i++ {
		po, perr := sessionRun(t, s, ctx, script)
		if perr != nil {
			t.Fatalf("run %d fails in the session: %v", i+1, perr)
		}
		var fresh, pooled map[string]any
		if err := decodeJSON(fo, &fresh); err != nil {
			t.Fatal(err)
		}
		if err := decodeJSON(po, &pooled); err != nil {
			t.Fatalf("run %d: %v\n%s", i+1, err, po)
		}
		for _, k := range []string{"created", "changed", "pendingPowerOff", "pendingDetail"} {
			if fresh[k] != pooled[k] {
				t.Fatalf("run %d: %s differs: fresh %v, session %v", i+1, k, fresh[k], pooled[k])
			}
		}
		if pooled["created"] != false || pooled["pendingPowerOff"] != false {
			t.Fatalf("a matching running VM must be neither created nor pending: %v", pooled)
		}
		// Every section marks its time, so a slow reconcile can name its part.
		ms, ok := pooled["ms"].(map[string]any)
		if !ok {
			t.Fatalf("run %d: no section timings in the result: %v", i+1, pooled)
		}
		for _, sec := range ensureSections {
			if _, ok := ms[sec]; !ok {
				t.Errorf("run %d: section %q did not mark its time: %v", i+1, sec, ms)
			}
		}
	}
}

// Held to the claim in session.go: the pooled scripts use nothing that behaves
// differently in a shared process. exit would end the session; $script:,
// $global: and Add-Type would leave state for the next call; Write-Host and
// [Console] write around the response instead of into it.
func TestPooledScriptsAreSafeToShareAProcess(t *testing.T) {
	vm := nestedVM(true)
	vm.Spec.ProcessorCount = 4
	vm.Spec.TPM = true
	vm.Spec.BootOrder = []string{"DVD", "Drive"}
	vm.Spec.VideoResolution = "1920x1080"
	vm.Spec.Disks = []types.VMDiskSpec{{Path: `C:\vms\a.vhdx`, SizeBytes: 1 << 30}}
	vm.Spec.ISOPath = `C:\iso\a.iso`
	vm.Spec.Placement.ClusterName = "cl1"
	vm.Status.VMID = "id-1"

	f := &fakeRunner{}
	p := newTestPS(f)
	_, _ = p.GetVMState(context.Background(), "Web01")
	_, _ = p.ListObservedVMs(context.Background())
	if len(f.calls) != 2 {
		t.Fatalf("expected GetVMState and ListObservedVMs to run one script each, got %d", len(f.calls))
	}
	scripts := map[string]string{
		"EnsureVM gen 2":  p.ensureVMScript(vm, 2),
		"EnsureVM gen 1":  p.ensureVMScript(vm, 1),
		"GetVMState":      f.calls[0],
		"ListObservedVMs": f.calls[1],
		"GetVMLiveStates": vmLiveScript([]string{"Web01", "Linux"}),
	}
	for name, s := range scripts {
		for _, l := range strings.Split(psCode(s), "\n") {
			line := strings.TrimSpace(l)
			for _, bad := range []string{"$script:", "$global:", "Add-Type", "Write-Host", "[Console]"} {
				if strings.Contains(line, bad) {
					t.Errorf("%s uses %s, which does not behave the same in a shared process: %s", name, bad, line)
				}
			}
			for _, w := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ';' || r == '{' || r == '}' || r == '(' }) {
				if strings.EqualFold(w, "exit") {
					t.Errorf("%s calls exit, which would end the shared process: %s", name, line)
				}
			}
		}
	}
}

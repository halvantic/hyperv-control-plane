package hyperv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"log/slog"

	"github.com/joshua-fourie/ballast/api/types"
)

// ProgressFunc receives a human-readable progress note from a long-running
// operation (e.g. "live migration 42%"). A nil ProgressFunc is a no-op.
type ProgressFunc func(note string)

func (f ProgressFunc) emit(note string) {
	if f != nil {
		f(note)
	}
}

// PowerShell is the v1 host implementation of Interface. It shells out to the
// Windows PowerShell management modules (Hyper-V, NetAdapter, NetTCPIP) per the
// stack decision in CLAUDE.md: every operation is an explicit, documented
// cmdlet sequence, easy to target, with hot paths to be migrated to direct
// WMI/CIM later.
//
// Each operation is split into a read script (observe actual state) and, only
// when needed, an act script (create or adjust). The decision between them is
// pure Go (see plan* functions) so it is unit-tested without a host; the
// scripts themselves require a real Hyper-V host to validate.
type PowerShell struct {
	run          runFunc
	runStream    runStreamFunc
	runStreamEnv runStreamEnvFunc
	log          *slog.Logger
	// timings records how long each exported method spent in PowerShell, so a
	// slow pass can name its own slowest call instead of being guessed at.
	timings *callTimings
}

// runFunc executes a PowerShell script and returns its stdout. It is a field so
// tests can inject canned output in place of a real shell-out.
type runFunc func(ctx context.Context, script string) ([]byte, error)

// runStreamFunc executes a script and invokes onLine for each stdout line as it
// arrives (so long-running scripts can report progress mid-flight), returning
// the exit error. Injectable so tests can drive canned progress lines.
type runStreamFunc func(ctx context.Context, script string, onLine func(string)) error

// runStreamEnvFunc streams stdout AND carries extra environment, for scripts that
// take a credential through the environment and still have progress to report.
type runStreamEnvFunc func(ctx context.Context, script string, env []string, onLine func(string)) error

// NewPowerShell returns a host implementation backed by powershell.exe.
func NewPowerShell(log *slog.Logger) *PowerShell {
	if log == nil {
		log = slog.Default()
	}
	// Timing wraps the two functions that actually execute a script, so every
	// call is covered — including the ones nobody thought to instrument, which is
	// where a surprise will be.
	ps := &PowerShell{log: log, timings: newCallTimings()}
	ps.run = func(ctx context.Context, script string) ([]byte, error) {
		defer ps.timed(time.Now())
		return execPowerShell(ctx, script)
	}
	ps.runStream = func(ctx context.Context, script string, onLine func(string)) error {
		defer ps.timed(time.Now())
		return execPowerShellStream(ctx, script, onLine)
	}
	ps.runStreamEnv = func(ctx context.Context, script string, env []string, onLine func(string)) error {
		defer ps.timed(time.Now())
		return execPowerShellStreamEnv(ctx, script, env, onLine)
	}
	return ps
}

// psFailureDetail explains a powershell.exe failure that left no diagnostic of
// its own.
//
// An empty stderr is not a rare edge. Killing the process — a context deadline,
// a cancelled job, the agent stopping — is reported by Windows as exit status 1
// with nothing written, because the process never got to write anything. The
// operator was then shown the whole of "remove vm \"Windows Standalone\":
// powershell: exit status 1:" and no more: no cause, no remedy, and nothing to
// tell a cancellation apart from a genuine failure. Seen on the rig 2026-08-08
// on repeated RemoveVM attempts.
//
// This does not decide why the command failed. It makes the difference between
// "cancelled" and "failed silently" legible, which is what the empty message
// took away.
func psFailureDetail(ctx context.Context, since time.Time, stdout, stderr string) string {
	if s := tidyPSError(stderr); s != "" {
		return s
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return "the operation ran past its time limit and was cancelled before it reported anything"
	case context.Canceled:
		return "the operation was cancelled before it reported anything (the agent stopped, lost the centre, or the job was superseded)"
	}
	// Whatever it managed to print is more use than nothing — a script that dies
	// part-way at least says how far it got.
	if s := strings.TrimSpace(stdout); s != "" {
		if i := strings.LastIndexByte(s, '\n'); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		}
		return "the command failed without writing any error output; its last output was: " + s
	}
	// Nothing on either stream and not cancelled. Telling the operator to go and
	// read the host's event log is the shape CLAUDE.md calls a defect: the agent
	// is ON that host and reads those same logs in half a dozen other places, so
	// a fact it can establish must not be posted as homework. Ask the host.
	if ev := recentHostErrors(since); ev != "" {
		return "the command failed without writing any error output, but the host logged this while it ran: " + ev
	}
	return "the command failed without writing any error output, was not cancelled, and the host's Hyper-V and System logs recorded nothing at the same time — so PowerShell exited non-zero without reporting a reason"
}

// tidyPSError reduces a PowerShell error record to the sentence a person needs.
//
// A record rendered to stderr repeats the message up to three times — once as the
// message, once in the offending source line, once in FullyQualifiedErrorId — and
// wraps it in positional noise:
//
//	the LUN … already contains a ReFS volume … use "Wipe and adopt".
//	At line:12 char:21
//	+ function Fail($m) { throw $m }
//	+ ~~~~~~~~
//	    + CategoryInfo          : OperationStopped: (…)
//	    + FullyQualifiedErrorId : the LUN … already contains …
//
// The carefully written first sentence is the part that helps, and burying it in
// its own echo is the "raw error passed through" failure by another route: the
// remedy is there and nobody reads that far. The source line is worse than
// useless here, naming the helper that threw rather than anything about the host.
//
// Anything not matching the known boilerplate is kept, because an unrecognised
// error losing its detail is far worse than a tidy one keeping some noise.
func tidyPSError(stderr string) string {
	// Everything from the first position marker onwards is boilerplate, INCLUDING
	// its wrapped continuations. Dropping only the lines that start with a marker
	// kept the continuation of a CategoryInfo line — PowerShell wraps mid-word — so
	// the tidied message ended with fragments like "ntimeException offers 2
	// available disk(s)…", which reads as corruption.
	var keep []string
	for _, line := range strings.Split(stderr, "\n") {
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(t, "At line:") || strings.HasPrefix(t, "At char:") || strings.HasPrefix(t, "+") {
			break
		}
		if t != "" {
			keep = append(keep, t)
		}
	}
	out := strings.TrimSpace(strings.Join(keep, " "))
	// PowerShell's own wrapping breaks long messages mid-word across lines; the
	// join above restores them, but the doubled spaces it leaves read as typos.
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return out
}

// recentHostErrorsScript reads what the host recorded WHILE THE COMMAND RAN.
//
// The window is the command's own execution, not a fixed span, and that is
// load-bearing. A host with a recurring background failure logs it every
// reconcile pass — a member that is not the CSV owner cannot set its default VHD
// path and records event 18172 every few seconds — so any fixed window is
// guaranteed to catch it and present it as the cause of whatever else failed.
// Correlation is all this can offer; restricting it to the command's own
// lifetime is what stops it being spurious correlation.
//
// Narrow in what it reads, too: the Hyper-V channels plus System filtered to
// Hyper-V, clustering and iSCSI providers, errors and warnings, three events.
const recentHostErrorsScript = `
$ErrorActionPreference = 'SilentlyContinue'
$since = (Get-Date).AddSeconds(-%[1]d)
$ev = @()
foreach ($l in @('Microsoft-Windows-Hyper-V-VMMS-Admin','Microsoft-Windows-Hyper-V-Worker-Admin','Microsoft-Windows-Hyper-V-Compute-Admin')) {
  $ev += Get-WinEvent -FilterHashtable @{ LogName=$l; StartTime=$since; Level=1,2,3 } -MaxEvents 4 -ErrorAction SilentlyContinue
}
$ev += Get-WinEvent -FilterHashtable @{ LogName='System'; StartTime=$since; Level=1,2 } -MaxEvents 40 -ErrorAction SilentlyContinue |
  Where-Object { $_.ProviderName -like '*Hyper-V*' -or $_.ProviderName -like '*FailoverClustering*' -or $_.ProviderName -like '*iScsi*' }
$ev = @($ev | Sort-Object TimeCreated -Descending | Select-Object -First 3)
($ev | ForEach-Object {
  $m = [string]$_.Message
  if ($m.Length -gt 300) { $m = $m.Substring(0,300) + '…' }
  ($m -replace '\s+',' ').Trim() + ' (' + [string]$_.ProviderName + ' event ' + [string]$_.Id + ')'
}) -join ' | '
`

// recentHostErrors asks the host what it logged. Best-effort and self-contained:
// it runs on its own short deadline and never reports its own failure, because it
// exists only to enrich somebody else's error and must not replace one unhelpful
// message with a different one. It deliberately does not go through execPowerShell,
// so a failure here can never recurse back into psFailureDetail.
func recentHostErrors(since time.Time) string {
	// A second of slack either side: the event is timestamped by the provider, not
	// by us, and a command that failed in its first instant would otherwise fall
	// outside its own window.
	secs := int(time.Since(since).Seconds()) + 1
	if secs < 2 {
		secs = 2
	}
	if secs > 300 {
		secs = 300 // a very long command's whole lifetime is not a useful window
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		fmt.Sprintf(recentHostErrorsScript, secs))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// withExplicitSuccess makes reaching the end of a script mean success.
//
// powershell.exe -Command takes its exit code from $? at the end, so a script
// whose LAST statement emitted a suppressed non-terminating error exits 1 having
// written nothing at all — indistinguishable from a real failure and impossible
// to diagnose. It is not a corner case: the natural way to verify a removal is
//
//	$still = Get-VM -Name $vm -ErrorAction SilentlyContinue
//	if ($still) { throw ... }
//
// where the VM being gone IS the success condition, and Get-VM not finding it
// leaves $? false. Ballast reported Failed for VM and replica deletions that had
// completed, repeatedly, over days.
//
// Every script here signals failure by throwing, and $ErrorActionPreference is
// Stop, so a throw terminates before this line is ever reached. Reaching it means
// the script ran to the end, which is exactly what success means.
func withExplicitSuccess(script string) string {
	return script + "\nexit 0"
}

// execPowerShell runs a script under Windows PowerShell. It uses powershell.exe
// (5.1) rather than pwsh because the Hyper-V and NetAdapter modules target it.
func execPowerShell(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", withExplicitSuccess(script))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("powershell: %w: %s", err, psFailureDetail(ctx, start, stdout.String(), stderr.String()))
	}
	return stdout.Bytes(), nil
}

// execPowerShellStream runs a script and calls onLine for each stdout line as it
// is produced, so a long-running script (e.g. a migration emitting PROGRESS
// lines) can report mid-flight. stderr is captured and folded into the exit
// error, as with execPowerShell.
func execPowerShellStream(ctx context.Context, script string, onLine func(string)) error {
	return execPowerShellStreamEnv(ctx, script, nil, onLine)
}

// execPowerShellStreamEnv is execPowerShellStream with extra environment.
//
// Streaming and environment were separate capabilities: a script could report
// progress as it ran, or take its operands through the environment, but not
// both. Anything carrying a credential therefore had to run silently to
// completion — which is exactly the long copy an operator most wants to watch.
func execPowerShellStreamEnv(ctx context.Context, script string, extraEnv []string, onLine func(string)) error {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", withExplicitSuccess(script))
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("powershell stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("powershell start: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if onLine != nil {
			onLine(strings.TrimRight(sc.Text(), "\r"))
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("powershell: %w: %s", err, psFailureDetail(ctx, start, "", stderr.String()))
	}
	return nil
}

// runWithEnv executes a script via powershell.exe with extra environment
// variables (used to pass secrets without putting them on the command line). It
// bypasses the injectable run field — only the real host implementation needs
// it, and it is never unit-tested through the stub.
func (p *PowerShell) runWithEnv(ctx context.Context, script string, extraEnv []string) error {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", withExplicitSuccess(script))
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("powershell: %w: %s", err, psFailureDetail(ctx, start, stdout.String(), stderr.String()))
	}
	return nil
}

// psQuote renders s as a PowerShell single-quoted string literal, escaping any
// embedded single quotes. All host/switch/adapter names pass through this
// before being interpolated into a script.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// decodeJSON unmarshals PowerShell JSON output into v. Empty output (a cmdlet
// that produced nothing) is treated as the zero value rather than an error.
//
// Some cluster cmdlets (e.g. Get-ClusterStorageSpacesDirect) emit warning lines
// ahead of their result. Since every script here ends with
// `ConvertTo-Json -Compress` (single-line output), we take the last non-empty
// line as the JSON, tolerating any such preamble.
func decodeJSON(out []byte, v any) error {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil
	}
	// Strip any leading warning/preamble lines: the JSON begins at the first
	// line starting with { or [. This keeps multi-line JSON intact.
	if !startsWithJSON(out) {
		lines := bytes.Split(out, []byte("\n"))
		for i, line := range lines {
			if startsWithJSON(bytes.TrimSpace(line)) {
				out = bytes.TrimSpace(bytes.Join(lines[i:], []byte("\n")))
				break
			}
		}
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("decode powershell output: %w (output: %q)", err, string(out))
	}
	return nil
}

func startsWithJSON(b []byte) bool {
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

// inventoryScript collects physical adapters, physical disks, memory and CPU in
// one invocation. The JSON keys match the api/types json tags so the result
// unmarshals straight into types.HostInventory.
//
// A physical adapter is reported as management when it carries a statically
// configured (Manual) IPv4 address AND a default gateway — that is the host's
// management identity (the address it is known by in DNS), and such a NIC must
// never be teamed into a vSwitch. The gateway matters: a converged host's
// storage and live-migration NICs are static too, but sit on isolated fabric
// subnets with no default route and no domain presence, and treating one as the
// host's address yields a WinRM target nothing can reach. A DHCP-assigned
// address does not make a NIC management either: it is free to assign to a
// switch, so its IP is not reported (the frontend treats a NIC with no host IP
// and no switch as free). A NIC bound to a vSwitch carries no IP here (the
// address lives on its management-OS vNIC), so it is naturally not flagged.
//
// The floating cluster IP is also a Manual address, but it is not a management
// identity — it lives on whichever node currently owns the cluster core group and
// must not pin that NIC as management (else the NIC the operator wants to free for
// a vSwitch looks untouchable). Cluster IPs are gathered from the cluster's IP
// Address resources and excluded, so only a NIC with its own static IP is flagged.
func inventoryScript() string {
	return `
$ErrorActionPreference = 'Stop'
$clusterIps = @()
try {
  Import-Module FailoverClusters -ErrorAction SilentlyContinue
  $clusterIps = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'IP Address' } | ForEach-Object { ($_ | Get-ClusterParameter -Name Address -ErrorAction SilentlyContinue).Value })
} catch {}
$adapters = Get-NetAdapter -Physical -ErrorAction SilentlyContinue | ForEach-Object {
  $a = @(Get-NetIPAddress -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '169.254.*' -and ($clusterIps -notcontains $_.IPAddress) })[0]
  $static = [bool]($a -and $a.PrefixOrigin -eq 'Manual')
  $ip = ''; $plen = 0
  if ($static) { $ip = [string]$a.IPAddress; $plen = [int]$a.PrefixLength }
  # Where-Object { $_ } is load-bearing, not tidiness.
  #
  # An adapter with no resolvers returns $null here, and @($null) is an array of
  # ONE null — which serialises as [null] and decodes into Go as [""]. So a NIC
  # with no DNS reported one empty DNS server rather than none, and every
  # consumer that asked "are there any?" got yes. The console's DNS column drew
  # a blank cell instead of "—" on every teamed adapter, and the check that
  # compares a NIC's resolvers against the domain controller's compared "".
  #
  # Absent is not empty-string, the same way absent is not zero. Found on
  # HVNEW01, 2026-09-07: every adapter on every converged host carried [""].
  $dns = @((Get-DnsClientServerAddress -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses | Where-Object { $_ })
  $reg = [bool](Get-DnsClient -InterfaceIndex $_.ifIndex -ErrorAction SilentlyContinue).RegisterThisConnectionsAddress
  # Default-route next hop on this NIC, so a re-homed management IP can keep the
  # host's default route on the converged switch's vNIC.
  $gw = [string](@(Get-NetRoute -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue)[0].NextHop)
  # A static IP alone does not make a NIC the management NIC: storage and
  # live-migration NICs are static too, on isolated fabric subnets with no
  # default route. The gateway is what separates the routable, domain-facing
  # NIC from fabric NICs, so require it. The IP is still reported for a static
  # fabric NIC — callers use "carries an IP" to refuse teaming it into a vSwitch,
  # and that guard must keep firing for storage/live-migration NICs.
  $isMgmt = $static -and $gw -ne ''
  # MTU, and what the driver will accept.
  #
  # NlMtu is reported rather than the driver's jumbo setting because NlMtu is
  # what the IP stack will actually send; the two disagree while a reset is in
  # flight, and on a card that silently declined the value. The valid values
  # travel too, so the console can say what a card will and will not do instead
  # of an operator finding out by trying.
  # MtuSize first: it is the miniport's own MTU, present whether or not the
  # adapter is teamed, and true whatever the driver's advanced property says.
  # NlMtu is absent on a teamed adapter, and on the 631FLR the *JumboPacket
  # display value read 1514 for a card carrying 9000.
  $mtu = 0
  if ($_.MtuSize) { $mtu = [int]$_.MtuSize }
  if ($mtu -eq 0) {
    $nlm = Get-NetIPInterface -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue
    if ($nlm) { $mtu = [int](@($nlm)[0].NlMtu) }
  }
  # RSC and LSO: the third thing that has to agree for jumbo frames, and the
  # only one nothing else reports. Both re-segment traffic in the NIC.
  $rsc = $false
  foreach ($r in @(Get-NetAdapterRsc -Name $_.Name -ErrorAction SilentlyContinue)) {
    if ($r.IPv4Enabled -or $r.IPv6Enabled) { $rsc = $true }
  }
  $lso = $false
  foreach ($l in @(Get-NetAdapterLso -Name $_.Name -ErrorAction SilentlyContinue)) {
    if ($l.V1IPv4Enabled -or $l.IPv4Enabled -or $l.IPv6Enabled) { $lso = $true }
  }
  $jp = Get-NetAdapterAdvancedProperty -Name $_.Name -RegistryKeyword '*JumboPacket' -ErrorAction SilentlyContinue
  if (-not $jp) { $jp = @(Get-NetAdapterAdvancedProperty -Name $_.Name -ErrorAction SilentlyContinue | Where-Object { $_.DisplayName -like '*Jumbo*' })[0] }
  [pscustomobject]@{ name = $_.Name; mac = $_.MacAddress; linkSpeedBps = [uint64]$_.Speed; up = ($_.Status -eq 'Up'); isManagement = $isMgmt; ipv4 = $ip; prefixLength = $plen; dnsServers = @($dns); registersDNS = $reg; gateway = $gw; mtuBytes = $mtu; jumboKeyword = [string]$jp.RegistryKeyword; jumboSetting = [string]$jp.DisplayValue; jumboValues = @($jp.ValidDisplayValues | ForEach-Object { [string]$_ } | Where-Object { $_ }); rscEnabled = $rsc; lsoEnabled = $lso }
}
# Keyed on UniqueId, NOT DeviceId.
#
# PhysicalDisk DeviceId is unique per bus, not per host: a local SSD and an iSCSI
# LUN both report DeviceId 2, and keying either map on it attributes one disk's
# facts to the other. On the rig HVNEW04 reported its 10GB iSCSI LUN as holding
# drive F — F belongs to the 100GB local SSD that shares its DeviceId, and the LUN
# has no letter at all. The same collision can mark the wrong disk as the OS disk,
# which is what hides a disk from the console and guards it from being formatted.
$osIds = @()
try { $osIds = @(Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.IsBoot -or $_.IsSystem } | Get-PhysicalDisk -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.UniqueId }) } catch {}
# Map UniqueId → first drive letter assigned via a partition (e.g. an NTFS volume
# formatted with Format-Volume and a drive letter).
$diskToLetter = @{}
try {
  Get-Disk -ErrorAction SilentlyContinue | ForEach-Object {
    $d = $_
    $letters = @($d | Get-Partition -ErrorAction SilentlyContinue | Where-Object { $_.DriveLetter } | ForEach-Object { [string]$_.DriveLetter })
    if ($letters.Count -gt 0) {
      $pds = @($d | Get-PhysicalDisk -ErrorAction SilentlyContinue)
      foreach ($pd in $pds) { $diskToLetter[[string]$pd.UniqueId] = $letters[0] }
    }
  }
} catch {}
# Map UniqueId → the storage pool holding the disk. CanPool alone cannot say what
# claims a disk: it is false for a pooled disk, a disk holding a volume, one that
# is offline, removable or too small, all alike. Reporting "in use" from it told
# an operator a disk was in S2D on hosts that have no S2D at all. The primordial
# pool is every disk in the machine and is not a claim on anything, so skip it.
$diskToPool = @{}
try {
  foreach ($sp in @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })) {
    foreach ($pd in @($sp | Get-PhysicalDisk -ErrorAction SilentlyContinue)) {
      $diskToPool[[string]$pd.UniqueId] = [string]$sp.FriendlyName
    }
  }
} catch {}
# In an S2D cluster Get-PhysicalDisk returns the whole cluster pool, so a host
# would report every node's disks. Keep only the disks whose physically-connected
# storage node is this host (mapping per disk, since the node->disk direction
# omits pool-eligible disks); disks with no node info (standalone) are kept too.
# Fall back to all disks if the filter yields nothing.
$cn = $env:COMPUTERNAME
$pdisks = @(Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object {
  $nodes = @($_ | Get-StorageNode -PhysicallyConnected -ErrorAction SilentlyContinue | ForEach-Object { $_.Name })
  (-not $nodes) -or (@($nodes | Where-Object { $_ -like ($cn + '*') }).Count -gt 0)
})
if (-not $pdisks -or $pdisks.Count -eq 0) { $pdisks = @(Get-PhysicalDisk -ErrorAction SilentlyContinue) }
$disks = $pdisks | ForEach-Object {
  $id = [string]$_.DeviceId
  $uid = [string]$_.UniqueId
  $letter = if ($diskToLetter.ContainsKey($uid)) { $diskToLetter[$uid] } else { '' }
  $pool = if ($diskToPool.ContainsKey($uid)) { $diskToPool[$uid] } else { '' }
  # CannotPoolReason is an array of enum values; join rather than stringify, or a
  # disk with two reasons reports one unreadable token.
  $why = ''
  try { if (-not $_.CanPool) { $why = (@($_.CannotPoolReason) | Where-Object { $_ } | ForEach-Object { [string]$_ }) -join ', ' } } catch {}
  # Usage, not just health. A RETIRED disk reports Healthy and contributes
  # nothing: the pool places no new data on it and will not repair onto it. Four
  # retired disks on bcluster2 left a three-way mirror uncreatable while every
  # health figure Ballast showed said the pool was fine.
  $usage = ''
  try { $usage = [string]$_.Usage } catch {}
  [pscustomobject]@{ deviceId = $id; uniqueId = $uid; sizeBytes = [uint64]$_.Size; mediaType = [string]$_.MediaType; canPool = [bool]$_.CanPool; isOSDisk = ($uid -in $osIds); driveLetter = $letter; busType = [string]$_.BusType; poolName = $pool; cannotPoolReason = $why; usage = $usage }
}
$cs = Get-CimInstance Win32_ComputerSystem
$os = Get-CimInstance Win32_OperatingSystem
$driveLetters = @(Get-PSDrive -PSProvider FileSystem -ErrorAction SilentlyContinue | Where-Object { $_.Name.Length -eq 1 } | ForEach-Object { $_.Name })
[pscustomobject]@{
  physicalAdapters = @($adapters)
  physicalDisks    = @($disks)
  totalMemoryBytes = [uint64]$cs.TotalPhysicalMemory
  logicalCPUs      = [int]$cs.NumberOfLogicalProcessors
  osVersion        = [string]($os.Caption + ' ' + $os.Version).Trim()
  usedDriveLetters = @($driveLetters)
} | ConvertTo-Json -Depth 5 -Compress
`
}

// CollectInventory observes host hardware via Get-NetAdapter, Get-PhysicalDisk
// and Win32_ComputerSystem. It is a pure read.
func (p *PowerShell) CollectInventory(ctx context.Context) (types.HostInventory, error) {
	out, err := p.run(ctx, inventoryScript())
	if err != nil {
		return types.HostInventory{}, fmt.Errorf("collect inventory: %w", err)
	}
	var inv types.HostInventory
	if err := decodeJSON(out, &inv); err != nil {
		return types.HostInventory{}, fmt.Errorf("collect inventory: %w", err)
	}
	return inv, nil
}

// metricsScript reads live host utilisation: overall CPU load (averaged across
// processors), physical memory in use (total visible minus free), and uptime
// since last boot. JSON keys match types.HostMetrics.
const metricsScript = `
$ErrorActionPreference = 'Stop'
$os = Get-CimInstance Win32_OperatingSystem
$cpu = (Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average
[pscustomobject]@{
  cpuUsagePercent  = [int]$cpu
  memoryInUseBytes = [uint64]((($os.TotalVisibleMemorySize - $os.FreePhysicalMemory)) * 1024)
  uptimeSeconds    = [int64]((Get-Date) - $os.LastBootUpTime).TotalSeconds
} | ConvertTo-Json -Compress
`

// CollectMetrics observes live CPU load, memory in use and uptime via CIM. It is
// a pure read.
func (p *PowerShell) CollectMetrics(ctx context.Context) (types.HostMetrics, error) {
	out, err := p.run(ctx, metricsScript)
	if err != nil {
		return types.HostMetrics{}, fmt.Errorf("collect metrics: %w", err)
	}
	var m types.HostMetrics
	if err := decodeJSON(out, &m); err != nil {
		return types.HostMetrics{}, fmt.Errorf("collect metrics: %w", err)
	}
	return m, nil
}

// resourcesScript observes existing vSwitches, storage volumes (CSV mount points
// when clustered, else fixed local volumes) and ISO files under each volume's
// ISOs folder and C:\ISOs. JSON keys match types.HostResources. All lookups are
// best-effort so a non-clustered or sparse host still returns what it can.
const resourcesScript = `
$ErrorActionPreference = 'SilentlyContinue'
$switchDetails = @(Get-VMSwitch | ForEach-Object {
  $sw = $_
  $descs = @()
  if ($sw.NetAdapterInterfaceDescriptions) { $descs = @($sw.NetAdapterInterfaceDescriptions) }
  elseif ($sw.NetAdapterInterfaceDescription) { $descs = @($sw.NetAdapterInterfaceDescription) }
  $nics = @($descs | ForEach-Object { (Get-NetAdapter -InterfaceDescription $_ -ErrorAction SilentlyContinue).Name } | Where-Object { $_ })
  if (-not $nics) { $nics = @($descs | Where-Object { $_ }) }
  $vlan = 0
  $mgmt = @(Get-VMNetworkAdapter -ManagementOS -ErrorAction SilentlyContinue | Where-Object { $_.SwitchName -eq $sw.Name })
  if ($mgmt.Count -gt 0) {
    $v = @($mgmt | Get-VMNetworkAdapterVlan -ErrorAction SilentlyContinue | Where-Object { $_.OperationMode -eq 'Access' })[0]
    if ($v) { $vlan = [int]$v.AccessVlanId }
  }
  [pscustomobject]@{ name = [string]$sw.Name; netAdapters = @($nics); allowManagementOS = [bool]$sw.AllowManagementOS; vlanId = [int]$vlan }
})
$switches = @($switchDetails | ForEach-Object { $_.name } | Where-Object { $_ })
# Cluster Shared Volumes AND the host's own volumes, not one or the other.
#
# This used to be an either/or: a host with CSVs reported only its CSVs, and its
# local data volumes were invisible to the centre. A member with a drive
# formatted for Hyper-V could not be offered it anywhere — a VM could not be
# placed on it, and an evacuation had nothing to put in its destination picker,
# so the path had to be typed from memory. Which kind a volume IS matters and is
# carried on each one; leaving half of them out is not how to express it.
$vols = @()
# The GUID paths already accounted for, so nothing is reported twice. A CSV has
# no drive letter of its own, so the letter-less pass below would otherwise
# collect every one of them a second time under its raw volume name.
$claimedPaths = @{}
$csv = Get-ClusterSharedVolume 2>$null
if ($csv) {
  $vols = @($csv | ForEach-Object {
    $p = $_.SharedVolumeInfo.Partition
    if ($p -and $p.Name) { $claimedPaths[([string]$p.Name).TrimEnd('\').ToLower()] = $true }
    [pscustomobject]@{ name = [string]$_.Name; path = [string]$_.SharedVolumeInfo.FriendlyVolumeName; sizeBytes = [uint64]$p.Size; usedBytes = [uint64]($p.Size - $p.FreeSpace); shared = $true }
  })
}
# Exclude the OS volume, and ONLY the OS volume: it is not somewhere the agent
# can create VHDXs without risking the Windows partition.
#
# Asked of the PARTITION, not the disk. Disk-level IsBoot/IsSystem marks a disk
# that carries a boot or system partition, and taking every lettered partition
# on such a disk excludes far more than the OS. Measured on HVNEW06: Windows put
# the 16MB system partition on disk 1 and the boot volume on disk 0, so disk 1
# was IsSystem — and its other partition, a 3.8TB data volume the operator had
# formatted for Hyper-V, was swept up with it. I: existed, was Fixed and NTFS,
# and Ballast reported "no volumes on this host".
#
# The partition flags mark the specific partition instead, and ESP/MSR/recovery
# partitions carry no drive letter, so they are already excluded by the letter
# test. $env:SystemDrive covers a Windows that is not on C; the literal C stays
# as a fallback for a host where detection fails.
$osDriveLetters = @('C')
try {
  if ($env:SystemDrive) { $osDriveLetters += [string]($env:SystemDrive -replace ':', '') }
  $osDriveLetters += @(Get-Partition -ErrorAction SilentlyContinue |
    Where-Object { ($_.IsBoot -or $_.IsSystem) -and $_.DriveLetter } |
    ForEach-Object { [string]$_.DriveLetter })
  $osDriveLetters = @($osDriveLetters | Sort-Object -Unique)
} catch {}
# A CSV mount lives under C:\ClusterStorage, and C is excluded already, so
# there is no way for one to be reported twice.
$vols += @(Get-Volume | Where-Object { $_.DriveType -eq 'Fixed' -and $_.DriveLetter -and ($osDriveLetters -notcontains [string]$_.DriveLetter) } | ForEach-Object {
  # Claimed too, so a lettered volume cannot also arrive as a letter-less one.
  if ($_.Path) { $claimedPaths[([string]$_.Path).TrimEnd('\').ToLower()] = $true }
  [pscustomobject]@{ name = "$($_.DriveLetter):"; path = "$($_.DriveLetter):\"; sizeBytes = [uint64]$_.Size; usedBytes = [uint64]($_.Size - $_.SizeRemaining); shared = $false }
})

# A formatted volume with NO drive letter is still a volume.
#
# The filter above requires one, which was true of every volume Ballast could
# create until it learned to format without a letter. Now the console offers
# that deliberately — for a disk destined to be mounted into a folder or handed
# to a cluster — and the volume it makes was invisible the moment it existed,
# appearing only as "1 disk with no drive letter". Offering an operator a way to
# create something the inventory then drops is worse than not offering it.
#
# Identified by its GUID path, which is what it HAS in place of a letter and
# what mounts it. The label leads when there is one, because an operator named
# it for a reason and a bare GUID is not a name anybody recognises.
#
# Still excluded: the reserved and recovery partitions Windows makes for itself.
# They are fixed volumes with no letter too, and listing them would bury the one
# volume this exists to show under three nobody asked about.
# NOT one already reported above.
#
# The old comment here said a CSV "cannot be reported twice" because its mount
# lives under C: and C is excluded — true of the LETTERED query and not of this
# one, which was added afterwards and inherited none of that protection. A CSV
# has no drive letter of its own, so every one of them came back a second time
# under its raw volume name: DS1 at C:\ClusterStorage\DS1, and "Cluster Disk 1"
# at the same GUID path with identical size and usage. Observed on Primary1 and
# Secondary, 2026-09-02, as a nameless extra row under each cluster's Storage.
$vols += @(Get-Volume | Where-Object {
  $_.DriveType -eq 'Fixed' -and -not $_.DriveLetter -and $_.Path -and
  (-not $claimedPaths.ContainsKey(([string]$_.Path).TrimEnd('\').ToLower())) -and
  $_.FileSystemType -and $_.FileSystemType -ne 'Unknown' -and
  ([string]$_.FileSystemLabel -notmatch '^(Recovery|System Reserved|EFI system partition)$') -and
  ([uint64]$_.Size -gt 1073741824)
} | ForEach-Object {
  $label = [string]$_.FileSystemLabel
  $shown = if ($label) { $label } else { 'unlettered volume' }
  [pscustomobject]@{
    name = $shown; path = [string]$_.Path
    sizeBytes = [uint64]$_.Size; usedBytes = [uint64]($_.Size - $_.SizeRemaining)
    shared = $false; unlettered = $true
  }
})
$roots = @($vols | ForEach-Object { Join-Path $_.path 'ISOs' }) + 'C:\ISOs'
$isos = @()
foreach ($r in $roots) {
  if (Test-Path $r) { $isos += @((Get-ChildItem -Path $r -Filter *.iso -File -Recurse -Depth 1).FullName) }
}
# Observed management-OS vNICs with their switch, VLAN, DNS, network profile and
# IP addresses classified by kind (host static / cluster VIP / dhcp / apipa) —
# drives the networking topology view.
$clusterIps = @()
try { $clusterIps = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'IP Address' } | ForEach-Object { ($_ | Get-ClusterParameter -Name Address -ErrorAction SilentlyContinue).Value }) } catch {}
$netCat = @{}
Get-NetConnectionProfile -ErrorAction SilentlyContinue | ForEach-Object { $netCat[[string]$_.InterfaceAlias] = [string]$_.NetworkCategory }
$mgmtVnics = @(Get-VMNetworkAdapter -ManagementOS -ErrorAction SilentlyContinue | ForEach-Object {
  $a = $_; $alias = 'vEthernet (' + [string]$a.Name + ')'
  $vlan = 0
  try { $vv = Get-VMNetworkAdapterVlan -ManagementOS -VMNetworkAdapterName $a.Name -ErrorAction SilentlyContinue; if ($vv -and $vv.OperationMode -eq 'Access') { $vlan = [int]$vv.AccessVlanId } } catch {}
  $addrs = @(Get-NetIPAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -ne '127.0.0.1' } | ForEach-Object {
    $k = 'host'
    if ($clusterIps -contains $_.IPAddress) { $k = 'cluster' }
    elseif ($_.IPAddress -like '169.254.*') { $k = 'apipa' }
    elseif ([string]$_.PrefixOrigin -eq 'Dhcp') { $k = 'dhcp' }
    [pscustomobject]@{ address = ([string]$_.IPAddress + '/' + [string]$_.PrefixLength); kind = $k }
  })
  # Same as the physical-adapter read above: @($null) is an array of one null,
  # which reaches Go as [""] and makes "no DNS" indistinguishable from "one
  # blank DNS server".
  $dns = @((Get-DnsClientServerAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses | Where-Object { $_ })
  # Default-route next hop on this vNIC. Without it the observation cannot tell a
  # routable management vNIC from an isolated fabric one, and anything rebuilding
  # a spec from what is observed writes a management vNIC with no gateway — which
  # is the edit that stranded the members off-subnet on 2026-08-05. An isolated
  # vNIC correctly reports none, and that absence is the fact worth carrying.
  $gw = [string](@(Get-NetRoute -InterfaceAlias $alias -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue)[0].NextHop)
  # The MTU this interface will actually send at. Observed rather than assumed
  # from the spec, because it is reset by anything that re-creates the interface
  # — including the IP reconcile's own remove-and-re-add — so a vNIC can sit at
  # 1500 having been set to 9000 an hour earlier with nothing reporting it.
  # NlMtu here, not MtuSize: on a management vNIC it is the IP interface's MTU
  # that decides what the stack puts on the wire, and that is the number an
  # operator is comparing against the uplinks below it.
  $vmtu = 0
  $vi = Get-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue
  if ($vi) { $vmtu = [int](@($vi)[0].NlMtu) }
  [pscustomobject]@{ name = [string]$a.Name; switchName = [string]$a.SwitchName; vlanID = $vlan; dnsServers = @($dns); profile = $netCat[$alias]; addresses = @($addrs); gateway = $gw; mtuBytes = $vmtu }
})
[pscustomobject]@{ switches = @($switches); switchDetails = @($switchDetails); volumes = @($vols); isos = @($isos); managementVNICs = @($mgmtVnics) } | ConvertTo-Json -Depth 5 -Compress
`

// CollectResources observes existing switches, storage volumes and ISO files.
func (p *PowerShell) CollectResources(ctx context.Context) (types.HostResources, error) {
	out, err := p.run(ctx, resourcesScript)
	if err != nil {
		return types.HostResources{}, fmt.Errorf("collect resources: %w", err)
	}
	var r types.HostResources
	if err := decodeJSON(out, &r); err != nil {
		return types.HostResources{}, fmt.Errorf("collect resources: %w", err)
	}
	return r, nil
}

// compile-time assertion that PowerShell satisfies the interface.
var _ Interface = (*PowerShell)(nil)

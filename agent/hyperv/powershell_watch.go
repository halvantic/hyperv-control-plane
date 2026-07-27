package hyperv

import (
	"context"
	"strings"
)

// vmStateWatchScript subscribes to VM power-state changes via a WMI/CIM
// indication and prints one EVENT line per change, flushing immediately so the
// reader sees it without buffering delay. It watches Msvm_ComputerSystem (each
// VM, plus the host) in root\virtualization\v2 and fires only when EnabledState
// actually changes — not on the constant uptime/heartbeat modifications — so a
// running VM does not spew events. WITHIN 2 sets the provider's poll interval to
// two seconds, a balance between latency and cost. The loop runs until the
// process is killed (ctx cancelled) or the subscription faults, at which point
// the caller re-establishes it.
const vmStateWatchScript = `$ErrorActionPreference = 'Stop'
$q = "SELECT * FROM __InstanceModificationEvent WITHIN 2 WHERE TargetInstance ISA 'Msvm_ComputerSystem' AND TargetInstance.EnabledState <> PreviousInstance.EnabledState"
Register-CimIndicationEvent -Namespace 'root/virtualization/v2' -Query $q -SourceIdentifier 'BallastVMState' | Out-Null
try {
  while ($true) {
    $e = Wait-Event -SourceIdentifier 'BallastVMState'
    if ($e) { Remove-Event -EventIdentifier $e.EventIdentifier -ErrorAction SilentlyContinue }
    [Console]::Out.WriteLine('EVENT')
    [Console]::Out.Flush()
  }
} finally {
  Unregister-Event -SourceIdentifier 'BallastVMState' -ErrorAction SilentlyContinue
}`

// WatchVMState streams VM power-state indications, invoking onEvent per change.
// It blocks until ctx is cancelled (the process is killed) or the subscription
// ends. See the interface doc: the callback only nudges a re-observe; it never
// carries authoritative state.
func (p *PowerShell) WatchVMState(ctx context.Context, onEvent func()) error {
	return p.runStream(ctx, vmStateWatchScript, func(line string) {
		if onEvent != nil && strings.TrimSpace(line) == "EVENT" {
			onEvent()
		}
	})
}

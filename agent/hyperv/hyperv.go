// Package hyperv is the agent's sole boundary to the host's virtualisation,
// networking and storage stack. Every Hyper-V interaction goes through the
// Interface defined here, so the v1 PowerShell-module implementation (and the
// later WMI/CIM one) can drop in without touching the agent's service loop,
// store, or reconcilers.
//
// The surface is two kinds of operation: pure reads that observe the host, and
// idempotent ensure-operations that drive one piece of host state towards a
// declared spec. The reconciler composes these; it holds no host knowledge of
// its own.
package hyperv

import (
	"context"

	"github.com/joshua-fourie/ballast/api/types"
)

// Outcome reports what an idempotent ensure-operation did. It lets the
// reconciler build accurate status and detect convergence (an all-Unchanged
// pass means the host already matches desired) without the host layer knowing
// anything about generations or reporting.
type Outcome int

const (
	// OutcomeUnchanged: actual state already matched the spec; nothing was done.
	OutcomeUnchanged Outcome = iota
	// OutcomeCreated: the resource was absent and has been created.
	OutcomeCreated
	// OutcomeUpdated: the resource existed but differed and has been adjusted.
	OutcomeUpdated
)

func (o Outcome) String() string {
	switch o {
	case OutcomeCreated:
		return "Created"
	case OutcomeUpdated:
		return "Updated"
	default:
		return "Unchanged"
	}
}

// Interface is the host-facing capability set the agent depends on. All methods
// take a context so a slow PowerShell or WMI call can be cancelled when the
// service is stopping.
//
// Every Ensure* method must be idempotent: applying the same spec twice is a
// no-op that returns OutcomeUnchanged. No method may assume it runs exactly
// once. This is the contract the reconcile loop relies on.
type Interface interface {
	// CollectInventory observes host hardware the centre uses for placement:
	// physical network adapters at minimum, plus physical disks, total memory
	// and logical CPU count where the implementation can determine them.
	//
	// It is a pure read; it must never mutate host state.
	CollectInventory(ctx context.Context) (types.HostInventory, error)

	// EnsureSwitch makes the SET-backed virtual switch described by spec exist
	// and match it: creating it (with the named team members, teaming mode and
	// load-balancing algorithm) when absent, adjusting it when it differs, and
	// doing nothing when it already matches.
	EnsureSwitch(ctx context.Context, spec types.VirtualSwitchSpec) (Outcome, error)

	// EnsureMgmtVNIC makes the management OS vNIC described by spec exist on its
	// switch and carry its VLAN, IP and QoS-weight intent. The switch named by
	// spec.SwitchName is expected to exist already; the reconciler ensures
	// switches before vNICs.
	EnsureMgmtVNIC(ctx context.Context, spec types.ManagementVNICSpec) (Outcome, error)
}

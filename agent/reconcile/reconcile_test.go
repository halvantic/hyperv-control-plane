package reconcile

import (
	"context"
	"io"
	"testing"
	"time"

	"log/slog"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

func testReconciler(hv hyperv.Interface) *Reconciler {
	r := New(hv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fixed := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return fixed }
	return r
}

func hostWithNetworking() types.Host {
	return types.Host{
		Meta: types.ObjectMeta{Name: "host01", Generation: 3},
		Spec: types.HostSpec{
			Networking: types.HostNetworkingSpec{
				Switches: []types.VirtualSwitchSpec{{
					Name:          "ConvergedSwitch",
					TeamMembers:   []string{"NIC1", "NIC2"},
					TeamingMode:   types.SETSwitchIndependent,
					LoadBalancing: types.SETDynamic,
				}},
				ManagementVNICs: []types.ManagementVNICSpec{{
					Name:       "Management",
					SwitchName: "ConvergedSwitch",
					IPConfig:   &types.IPConfig{Address: "10.0.0.21/24"},
				}},
			},
		},
	}
}

func reasonByType(conds []types.Condition, condType string) (string, bool) {
	for _, c := range conds {
		if c.Type == condType {
			return c.Reason, true
		}
	}
	return "", false
}

// A fresh host converges (everything Created, Honoured) and a second identical
// pass is a no-op (everything AlreadyConfigured, Changed == false). This is the
// load-bearing idempotency guarantee.
func TestReconcileFreshThenIdempotent(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	desired := hostWithNetworking()

	res, err := r.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("first pass error: %v", err)
	}
	if !res.Honoured || res.Phase != types.PhaseReady || !res.Changed {
		t.Fatalf("first pass: want honoured/ready/changed, got %+v", res)
	}
	if reason, _ := reasonByType(res.Conditions, "Switch/ConvergedSwitch"); reason != "Created" {
		t.Fatalf("switch reason: want Created, got %q", reason)
	}
	if reason, _ := reasonByType(res.Conditions, "ManagementVNIC/Management"); reason != "Created" {
		t.Fatalf("vNIC reason: want Created, got %q", reason)
	}
	if !stub.HasSwitch("ConvergedSwitch") || !stub.HasMgmtVNIC("Management") {
		t.Fatal("expected switch and vNIC to exist after first pass")
	}

	// Second pass: no host mutation.
	res2, err := r.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("second pass error: %v", err)
	}
	if !res2.Honoured || res2.Phase != types.PhaseReady {
		t.Fatalf("second pass: want honoured/ready, got %+v", res2)
	}
	if res2.Changed {
		t.Fatal("second pass mutated host state; reconcile is not idempotent")
	}
	if reason, _ := reasonByType(res2.Conditions, "Switch/ConvergedSwitch"); reason != "AlreadyConfigured" {
		t.Fatalf("switch reason on converged pass: want AlreadyConfigured, got %q", reason)
	}
}

// A failed ensure leaves the pass not honoured and Degraded, and the failure is
// surfaced as a Condition. The agent must not advance ObservedGeneration off this.
func TestReconcileFailureIsDegradedAndNotHonoured(t *testing.T) {
	stub := &hyperv.Stub{FailSwitch: "ConvergedSwitch"}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), hostWithNetworking())
	if err == nil {
		t.Fatal("expected an error when a switch fails")
	}
	if res.Honoured {
		t.Fatal("must not report Honoured when an ensure failed")
	}
	if res.Phase != types.PhaseDegraded {
		t.Fatalf("phase: want Degraded, got %v", res.Phase)
	}
	reason, ok := reasonByType(res.Conditions, "Switch/ConvergedSwitch")
	if !ok || reason != "ApplyFailed" {
		t.Fatalf("switch condition: want ApplyFailed, got %q (present=%v)", reason, ok)
	}
}

// Switches are ensured before the vNICs that depend on them: the stub errors a
// vNIC whose switch is absent, so a clean pass proves the ordering.
func TestReconcileEnsuresSwitchesBeforeVNICs(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), hostWithNetworking())
	if err != nil {
		t.Fatalf("unexpected error (vNIC ensured before its switch?): %v", err)
	}
	if !res.Honoured {
		t.Fatalf("want honoured, got %+v", res)
	}
}

// An empty networking spec is trivially honoured with no changes.
func TestReconcileEmptyIsHonoured(t *testing.T) {
	r := testReconciler(&hyperv.Stub{})

	res, err := r.Reconcile(context.Background(), types.Host{Meta: types.ObjectMeta{Generation: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Honoured || res.Phase != types.PhaseReady || res.Changed || len(res.Conditions) != 0 {
		t.Fatalf("empty spec: want honoured/ready/unchanged/no-conditions, got %+v", res)
	}
}

// A spec change after convergence is detected and applied as an Update.
func TestReconcileDetectsUpdate(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	desired := hostWithNetworking()
	if _, err := r.Reconcile(context.Background(), desired); err != nil {
		t.Fatal(err)
	}

	desired.Spec.Networking.Switches[0].AllowManagementOS = true
	res, err := r.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("expected Changed on a spec change")
	}
	if reason, _ := reasonByType(res.Conditions, "Switch/ConvergedSwitch"); reason != "Updated" {
		t.Fatalf("switch reason: want Updated, got %q", reason)
	}
}

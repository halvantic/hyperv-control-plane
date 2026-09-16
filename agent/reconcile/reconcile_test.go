package reconcile

import (
	"context"
	"io"
	"testing"
	"time"

	"log/slog"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
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

func hostWithRole(policy types.RebootPolicy) types.Host {
	h := hostWithNetworking()
	h.Spec.EnableHyperVRole = true
	h.Spec.RebootPolicy = policy
	return h
}

// When the role is already active the reconciler proceeds to networking and
// reports HyperVInstalled.
func TestReconcileRoleAlreadyInstalled(t *testing.T) {
	stub := &hyperv.Stub{HyperVInstalled: true}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithRole(types.RebootNever), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Honoured || res.Phase != types.PhaseReady || !res.HyperVInstalled {
		t.Fatalf("want honoured/ready/installed, got %+v", res)
	}
	if !stub.HasSwitch("ConvergedSwitch") {
		t.Fatal("networking should run once the role is active")
	}
}

// A best-effort step that fails (here the default VM/VHD host paths) must not
// degrade the host or hold back its generation: the host stays Ready/Honoured
// and the failure is recorded as a non-blocking advisory (Reason "NotApplied"),
// so the UI can surface "settled, with advisories" rather than a hard error.
func TestReconcileBestEffortFailureIsAdvisory(t *testing.T) {
	host := hostWithRole(types.RebootNever)
	host.Spec.Storage.DefaultVMPath = `C:\ClusterStorage\Vol01`
	host.Spec.Storage.DefaultVHDPath = `C:\ClusterStorage\Vol01`
	stub := &hyperv.Stub{HyperVInstalled: true, FailVMHostPaths: true}

	res, err := testReconciler(stub).Reconcile(context.Background(), host, nil)
	if err != nil {
		t.Fatalf("best-effort failure must not return an error: %v", err)
	}
	if !res.Honoured || res.Phase != types.PhaseReady {
		t.Fatalf("want honoured/ready despite best-effort failure, got phase=%s honoured=%v", res.Phase, res.Honoured)
	}
	reason, ok := reasonByType(res.Conditions, "VMHostPaths")
	if !ok || reason != "NotApplied" {
		t.Fatalf("VMHostPaths condition: want advisory reason NotApplied, got %q (present=%v)", reason, ok)
	}
}

// RebootNever: the agent installs the role but must NOT reboot; it surfaces
// RebootRequired, does not honour the generation, and does not touch networking.
func TestReconcileRoleNeedsRebootPolicyNever(t *testing.T) {
	stub := &hyperv.Stub{HyperVInstalled: false}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithRole(types.RebootNever), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.EnsureRoleCalled {
		t.Fatal("role should have been installed")
	}
	if stub.RebootCalled {
		t.Fatal("RebootPolicy=Never must never reboot")
	}
	if res.Honoured || !res.RebootRequired || res.Phase != types.PhaseProgressing {
		t.Fatalf("want progressing/reboot-required/not-honoured, got %+v", res)
	}
	if stub.HasSwitch("ConvergedSwitch") {
		t.Fatal("networking must not run before the role is active")
	}
}

// RebootIfNeeded: the agent installs and reboots to activate the role.
func TestReconcileRoleNeedsRebootPolicyIfNeeded(t *testing.T) {
	stub := &hyperv.Stub{HyperVInstalled: false}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithRole(types.RebootIfNeeded), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.EnsureRoleCalled || !stub.RebootCalled {
		t.Fatalf("RebootPolicy=IfNeeded should install and reboot; ensure=%v reboot=%v",
			stub.EnsureRoleCalled, stub.RebootCalled)
	}
	if res.Honoured || res.Phase != types.PhaseProgressing {
		t.Fatalf("want progressing/not-honoured while rebooting, got %+v", res)
	}
}

// A fresh host converges (everything Created, Honoured) and a second identical
// pass is a no-op (everything AlreadyConfigured, Changed == false). This is the
// load-bearing idempotency guarantee.
func TestReconcileFreshThenIdempotent(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	desired := hostWithNetworking()

	res, err := r.Reconcile(context.Background(), desired, nil)
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
	res2, err := r.Reconcile(context.Background(), desired, nil)
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

// A host managing networking heals a NIC stranded on the Public profile as part
// of the ordinary reconcile pass — no operator job needed.
func TestReconcileHealsPublicNetworkProfile(t *testing.T) {
	stub := &hyperv.Stub{NetworkProfilePublic: true}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), hostWithNetworking(), nil)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if !res.Honoured || res.Phase != types.PhaseReady || !res.Changed {
		t.Fatalf("want honoured/ready/changed, got %+v", res)
	}
	if reason, ok := reasonByType(res.Conditions, "NetworkProfile"); !ok || reason != "Updated" {
		t.Fatalf("NetworkProfile condition: want Updated, got %q (present=%v)", reason, ok)
	}
}

// The profile step only runs where the agent manages networking; a host with no
// switch or management vNIC declared does not touch connection profiles.
func TestReconcileSkipsNetworkProfileWithoutNetworking(t *testing.T) {
	stub := &hyperv.Stub{NetworkProfilePublic: true}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), types.Host{Meta: types.ObjectMeta{Name: "bare", Generation: 1}}, nil)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if _, ok := reasonByType(res.Conditions, "NetworkProfile"); ok {
		t.Fatal("NetworkProfile condition present on a host with no managed networking")
	}
}

// A profile-repair failure is advisory: it is surfaced as a NotApplied condition
// but must not degrade the host or hold its generation back.
func TestReconcileNetworkProfileFailureIsAdvisory(t *testing.T) {
	stub := &hyperv.Stub{FailNetworkProfile: true}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), hostWithNetworking(), nil)
	if err != nil {
		t.Fatalf("reconcile should not error on advisory failure: %v", err)
	}
	if !res.Honoured || res.Phase != types.PhaseReady {
		t.Fatalf("advisory failure must stay honoured/ready, got %+v", res)
	}
	if reason, ok := reasonByType(res.Conditions, "NetworkProfile"); !ok || reason != "NotApplied" {
		t.Fatalf("NetworkProfile condition: want NotApplied, got %q (present=%v)", reason, ok)
	}
}

// A failed ensure leaves the pass not honoured and Degraded, and the failure is
// surfaced as a Condition. The agent must not advance ObservedGeneration off this.
func TestReconcileFailureIsDegradedAndNotHonoured(t *testing.T) {
	stub := &hyperv.Stub{FailSwitch: "ConvergedSwitch"}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), hostWithNetworking(), nil)
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

	res, err := r.Reconcile(context.Background(), hostWithNetworking(), nil)
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

	res, err := r.Reconcile(context.Background(), types.Host{Meta: types.ObjectMeta{Generation: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The privilege self-check runs unconditionally (it asserts a fact about
	// the running identity, not about anything the spec declares), so even
	// an empty spec now carries the one condition it always produces —
	// Privilege/LocalAdmin. Everything else about an empty spec stays
	// no-op: no switches, no vNICs, nothing else to report.
	if !res.Honoured || res.Phase != types.PhaseReady || res.Changed {
		t.Fatalf("empty spec: want honoured/ready/unchanged, got %+v", res)
	}
	if len(res.Conditions) != 1 || res.Conditions[0].Type != "Privilege/LocalAdmin" {
		t.Fatalf("empty spec: want exactly one Privilege/LocalAdmin condition, got %+v", res.Conditions)
	}
}

// A spec change after convergence is detected and applied as an Update.
func TestReconcileDetectsUpdate(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	desired := hostWithNetworking()
	if _, err := r.Reconcile(context.Background(), desired, nil); err != nil {
		t.Fatal(err)
	}

	desired.Spec.Networking.Switches[0].AllowManagementOS = true
	res, err := r.Reconcile(context.Background(), desired, nil)
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

// A cluster member heals a NIC stranded on Public even when the agent authored
// none of its networking. A member NIC on Public blocks the cluster and SMB
// traffic that carries CSV I/O regardless of who created the adapter, so the
// hygiene cannot be conditional on Ballast managing the switch — that left the
// hosts most in need of it doing nothing.
func TestReconcileHealsPublicProfileOnAClusterMemberWithoutManagedNetworking(t *testing.T) {
	stub := &hyperv.Stub{NetworkProfilePublic: true}
	r := testReconciler(stub)

	host := types.Host{
		Meta: types.ObjectMeta{Name: "member01", Generation: 1},
		Spec: types.HostSpec{
			ClusterMembership: &types.ClusterMembershipSpec{ClusterName: "bcluster2"},
		},
	}
	res, err := r.Reconcile(context.Background(), host, nil)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if reason, ok := reasonByType(res.Conditions, "NetworkProfile"); !ok || reason != "Updated" {
		t.Fatalf("a cluster member must heal its own profiles: want Updated, got %q (present=%v)", reason, ok)
	}
}

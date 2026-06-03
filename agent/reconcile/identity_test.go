package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

func hostWithName(current, desired string, policy types.RebootPolicy) types.Host {
	return types.Host{
		Meta: types.ObjectMeta{Name: "host01", Generation: 1},
		Spec: types.HostSpec{ComputerName: desired, RebootPolicy: policy},
	}
}

// Name already matches: identity settles, networking (none here) proceeds, Ready.
func TestReconcileNameAlreadyCorrect(t *testing.T) {
	stub := &hyperv.Stub{ComputerName: "HV-PROD-01"}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithName("HV-PROD-01", "HV-PROD-01", types.RebootNever))
	if err != nil {
		t.Fatal(err)
	}
	if stub.RenameCalled {
		t.Fatal("should not rename when the name already matches")
	}
	if !res.Honoured || res.Phase != types.PhaseReady {
		t.Fatalf("want honoured/ready, got %+v", res)
	}
	if reason, _ := reasonByType(res.Conditions, "ComputerName"); reason != "AlreadyConfigured" {
		t.Fatalf("want AlreadyConfigured, got %q", reason)
	}
}

// RebootNever: rename is performed but the agent must not reboot; it surfaces
// RebootRequired, is not honoured, and Progressing.
func TestReconcileRenamePolicyNever(t *testing.T) {
	stub := &hyperv.Stub{ComputerName: "WIN-TEMP"}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithName("WIN-TEMP", "HV-PROD-01", types.RebootNever))
	if err != nil {
		t.Fatal(err)
	}
	if !stub.RenameCalled {
		t.Fatal("rename should have been performed")
	}
	if stub.RebootCalled {
		t.Fatal("RebootPolicy=Never must not reboot")
	}
	if res.Honoured || !res.RebootRequired || res.Phase != types.PhaseProgressing {
		t.Fatalf("want progressing/reboot-required/not-honoured, got %+v", res)
	}
}

// RebootIfNeeded: rename and reboot to apply.
func TestReconcileRenamePolicyIfNeeded(t *testing.T) {
	stub := &hyperv.Stub{ComputerName: "WIN-TEMP"}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithName("WIN-TEMP", "HV-PROD-01", types.RebootIfNeeded))
	if err != nil {
		t.Fatal(err)
	}
	if !stub.RenameCalled || !stub.RebootCalled {
		t.Fatalf("IfNeeded should rename and reboot; rename=%v reboot=%v", stub.RenameCalled, stub.RebootCalled)
	}
	if res.Honoured || res.Phase != types.PhaseProgressing {
		t.Fatalf("want progressing/not-honoured while rebooting, got %+v", res)
	}
}

// A management IP is assigned idempotently and does not block the pass.
func TestReconcileManagementIP(t *testing.T) {
	stub := &hyperv.Stub{ComputerName: "HV-PROD-01"}
	h := hostWithName("HV-PROD-01", "HV-PROD-01", types.RebootNever)
	h.Spec.ManagementNIC = &types.PhysicalNICConfig{AdapterName: "Ethernet1", IPConfig: types.IPConfig{Address: "10.0.0.10/24", Gateway: "10.0.0.1"}}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), h)
	if err != nil || !res.Honoured {
		t.Fatalf("first pass: want honoured, got %+v err %v", res, err)
	}
	if reason, _ := reasonByType(res.Conditions, "ManagementNIC/Ethernet1"); reason != "Updated" {
		t.Fatalf("want Updated, got %q", reason)
	}
	// Idempotent second pass.
	res2, _ := r.Reconcile(context.Background(), h)
	if reason, _ := reasonByType(res2.Conditions, "ManagementNIC/Ethernet1"); reason != "AlreadyConfigured" {
		t.Fatalf("second pass: want AlreadyConfigured, got %q", reason)
	}
}

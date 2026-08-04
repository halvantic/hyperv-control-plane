package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

func hostWithThreeVNICs() types.Host {
	h := hostWithNetworking()
	h.Spec.Networking.DNSServers = []string{"192.168.1.168"}
	h.Spec.Networking.ManagementVNICs = []types.ManagementVNICSpec{
		{Name: "Management", SwitchName: "ConvergedSwitch", IPConfig: &types.IPConfig{Address: "192.168.1.71/24"}},
		{Name: "LiveMigration", SwitchName: "ConvergedSwitch", VLANID: 50, IPConfig: &types.IPConfig{Address: "10.0.50.10/24"}},
		{Name: "Storage", SwitchName: "ConvergedSwitch", VLANID: 60, IPConfig: &types.IPConfig{Address: "10.0.60.10/24"}},
	}
	return h
}

// Batching the observation must not batch the OUTCOME. Each vNIC still reports
// against its own condition, or an operator loses the ability to tell which one
// is wrong — and a single bad spec would read as three failures.
func TestBatchedVNICsEachGetTheirOwnCondition(t *testing.T) {
	stub := &hyperv.Stub{}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithThreeVNICs(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Management", "LiveMigration", "Storage"} {
		reason, ok := reasonByType(res.Conditions, "ManagementVNIC/"+n)
		if !ok {
			t.Fatalf("no condition for %q — batching must not merge them", n)
		}
		if reason != "Created" {
			t.Fatalf("%s: want Created, got %q", n, reason)
		}
	}
}

// One failing vNIC must not take the others down with it. They are independent,
// and stopping at the first would leave later ones unreconciled with nothing
// said about them.
func TestOneFailingVNICDoesNotStopTheRest(t *testing.T) {
	stub := &hyperv.Stub{FailVNIC: "LiveMigration"}
	res, err := testReconciler(stub).Reconcile(context.Background(), hostWithThreeVNICs(), nil)
	if err == nil {
		t.Fatal("a failing vNIC must surface as an error")
	}
	if !strings.Contains(err.Error(), "LiveMigration") {
		t.Fatalf("the error must name the vNIC that failed, got %v", err)
	}

	// The failure is attributed to its own condition, and only its own.
	if reason, _ := reasonByType(res.Conditions, "ManagementVNIC/LiveMigration"); reason == "Created" {
		t.Fatal("the failing vNIC must not report success")
	}
	for _, n := range []string{"Management", "Storage"} {
		reason, ok := reasonByType(res.Conditions, "ManagementVNIC/"+n)
		if !ok {
			t.Fatalf("%s must still be reconciled and reported", n)
		}
		if reason != "Created" {
			t.Fatalf("%s should have succeeded independently, got %q", n, reason)
		}
	}
	// And the healthy ones actually got applied, not just reported.
	for _, n := range []string{"Management", "Storage"} {
		if !stub.HasMgmtVNIC(n) {
			t.Fatalf("%s should exist: a sibling's failure must not skip it", n)
		}
	}
}

// The host-level DNS fallback is applied per vNIC before the batch runs, so
// collecting the specs up front must not lose it.
func TestBatchedVNICsStillInheritHostDNS(t *testing.T) {
	stub := &hyperv.Stub{}
	if _, err := testReconciler(stub).Reconcile(context.Background(), hostWithThreeVNICs(), nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Management", "LiveMigration", "Storage"} {
		got, ok := stub.MgmtVNIC(n)
		if !ok {
			t.Fatalf("%s not created", n)
		}
		if got.IPConfig == nil || len(got.IPConfig.DNSServers) != 1 || got.IPConfig.DNSServers[0] != "192.168.1.168" {
			t.Fatalf("%s must inherit the host DNS servers, got %+v", n, got.IPConfig)
		}
	}
}

// A second identical pass changes nothing. Batching the observation must not
// disturb the idempotency guarantee — it is the whole point of the reconciler.
func TestBatchedVNICsAreIdempotent(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	desired := hostWithThreeVNICs()

	if _, err := r.Reconcile(context.Background(), desired, nil); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(context.Background(), desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("a second identical pass must change nothing")
	}
	for _, n := range []string{"Management", "LiveMigration", "Storage"} {
		if reason, _ := reasonByType(res.Conditions, "ManagementVNIC/"+n); reason != "AlreadyConfigured" {
			t.Fatalf("%s: want AlreadyConfigured on the second pass, got %q", n, reason)
		}
	}
}

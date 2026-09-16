package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

func TestPrivilegeConditions(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name       string
		chk        hyperv.PrivilegeCheck
		wantTypes  []string
		wantStatus map[string]bool
	}{
		{
			name:       "local admin only, nothing else applicable",
			chk:        hyperv.PrivilegeCheck{IsLocalAdmin: true},
			wantTypes:  []string{"Privilege/LocalAdmin"},
			wantStatus: map[string]bool{"Privilege/LocalAdmin": true},
		},
		{
			name:       "not local admin",
			chk:        hyperv.PrivilegeCheck{IsLocalAdmin: false},
			wantTypes:  []string{"Privilege/LocalAdmin"},
			wantStatus: map[string]bool{"Privilege/LocalAdmin": false},
		},
		{
			name: "cluster member, access OK",
			chk: hyperv.PrivilegeCheck{
				IsLocalAdmin: true, ClusterApplicable: true, ClusterAccessOK: true,
			},
			wantTypes: []string{"Privilege/LocalAdmin", "Privilege/ClusterAccess"},
			wantStatus: map[string]bool{
				"Privilege/LocalAdmin": true, "Privilege/ClusterAccess": true,
			},
		},
		{
			name: "cluster member, access denied",
			chk: hyperv.PrivilegeCheck{
				IsLocalAdmin: true, ClusterApplicable: true, ClusterAccessOK: false,
			},
			wantTypes: []string{"Privilege/LocalAdmin", "Privilege/ClusterAccess"},
			wantStatus: map[string]bool{
				"Privilege/LocalAdmin": true, "Privilege/ClusterAccess": false,
			},
		},
		{
			name:      "not a cluster member -- no cluster condition at all",
			chk:       hyperv.PrivilegeCheck{IsLocalAdmin: true, ClusterApplicable: false},
			wantTypes: []string{"Privilege/LocalAdmin"},
		},
		{
			name: "OUPath declared, delegation present",
			chk: hyperv.PrivilegeCheck{
				IsLocalAdmin: true, ADDelegationApplicable: true, ADDelegationOK: true,
			},
			wantTypes: []string{"Privilege/LocalAdmin", "Privilege/ADDelegation"},
			wantStatus: map[string]bool{
				"Privilege/LocalAdmin": true, "Privilege/ADDelegation": true,
			},
		},
		{
			name: "OUPath declared, delegation missing",
			chk: hyperv.PrivilegeCheck{
				IsLocalAdmin: true, ADDelegationApplicable: true, ADDelegationOK: false,
			},
			wantTypes: []string{"Privilege/LocalAdmin", "Privilege/ADDelegation"},
			wantStatus: map[string]bool{
				"Privilege/LocalAdmin": true, "Privilege/ADDelegation": false,
			},
		},
		{
			name: "OUPath declared, could not determine -- NOT the same as missing",
			chk: hyperv.PrivilegeCheck{
				IsLocalAdmin: true, ADDelegationApplicable: true,
				ADDelegationOK: false, ADDelegationErr: "no domain controller reachable",
			},
			wantTypes: []string{"Privilege/LocalAdmin", "Privilege/ADDelegation"},
			wantStatus: map[string]bool{
				"Privilege/LocalAdmin": true, "Privilege/ADDelegation": false,
			},
		},
		{
			name:      "no OUPath declared -- no AD delegation condition at all",
			chk:       hyperv.PrivilegeCheck{IsLocalAdmin: true, ADDelegationApplicable: false},
			wantTypes: []string{"Privilege/LocalAdmin"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := privilegeConditions(c.chk, now)
			if len(got) != len(c.wantTypes) {
				t.Fatalf("got %d conditions, want %d: %+v", len(got), len(c.wantTypes), got)
			}
			for i, wantType := range c.wantTypes {
				if got[i].Type != wantType {
					t.Errorf("condition %d: type = %q, want %q", i, got[i].Type, wantType)
				}
				if wantStatus, ok := c.wantStatus[wantType]; ok && got[i].Status != wantStatus {
					t.Errorf("condition %q: status = %v, want %v", wantType, got[i].Status, wantStatus)
				}
			}
		})
	}
}

// TestADDelegationCouldNotDetermineIsNamedDistinctly proves the specific
// thing that matters for this condition: a read failure (no DC reachable)
// must not read the same as a confirmed-missing delegation. Both currently
// report Status=false (the type only has a bool), so the distinction has to
// live in Reason and Message -- this pins that it actually does.
func TestADDelegationCouldNotDetermineIsNamedDistinctly(t *testing.T) {
	now := time.Now()
	adCondition := func(conds []types.Condition) types.Condition {
		for _, c := range conds {
			if c.Type == "Privilege/ADDelegation" {
				return c
			}
		}
		t.Fatal("no Privilege/ADDelegation condition produced")
		return types.Condition{}
	}
	missing := adCondition(privilegeConditions(hyperv.PrivilegeCheck{
		IsLocalAdmin: true, ADDelegationApplicable: true, ADDelegationOK: false,
	}, now))
	unknown := adCondition(privilegeConditions(hyperv.PrivilegeCheck{
		IsLocalAdmin: true, ADDelegationApplicable: true, ADDelegationOK: false, ADDelegationErr: "no DC reachable",
	}, now))

	if missing.Reason == unknown.Reason {
		t.Errorf("a confirmed shortfall and an unreadable check must not share a Reason: both got %q", missing.Reason)
	}
	if unknown.Reason != "CouldNotDetermine" {
		t.Errorf("an unreadable check's Reason = %q, want CouldNotDetermine", unknown.Reason)
	}
	if missing.Reason != "MissingDelegation" {
		t.Errorf("a confirmed shortfall's Reason = %q, want MissingDelegation", missing.Reason)
	}
}

// TestPrivilegeShortfallDoesNotDegradeTheHost is the "report, do not block
// startup" requirement, proved at the Reconcile level rather than assumed:
// a local-admin shortfall must still leave the host Honoured/Ready if
// nothing else is wrong, with the shortfall visible only as a condition.
func TestPrivilegeShortfallDoesNotDegradeTheHost(t *testing.T) {
	stub := &hyperv.Stub{PrivilegeCheckResult: &hyperv.PrivilegeCheck{IsLocalAdmin: false}}
	r := testReconciler(stub)

	res, err := r.Reconcile(context.Background(), types.Host{Meta: types.ObjectMeta{Generation: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Honoured || res.Phase != types.PhaseReady {
		t.Fatalf("a privilege shortfall must not degrade the host: got Honoured=%v Phase=%v", res.Honoured, res.Phase)
	}
	found := false
	for _, c := range res.Conditions {
		if c.Type == "Privilege/LocalAdmin" {
			found = true
			if c.Status {
				t.Error("Privilege/LocalAdmin Status=true, want false (stub reports not-admin)")
			}
		}
	}
	if !found {
		t.Fatal("no Privilege/LocalAdmin condition reported for a real shortfall")
	}
}

// TestPrivilegeCheckThrottlesOnceSettledButNotOnAShortfall proves the two
// halves of the cadence: a clean result is throttled after the first pass
// (a DC round-trip on every pass across a fleet is exactly what
// ouDriftCheckEvery already exists to avoid, for the same reason), but a
// shortfall keeps checking every pass so a fix is reflected promptly.
func TestPrivilegeCheckThrottlesOnceSettledButNotOnAShortfall(t *testing.T) {
	t.Run("settled: second pass reuses the cached condition", func(t *testing.T) {
		stub := &hyperv.Stub{PrivilegeCheckResult: &hyperv.PrivilegeCheck{IsLocalAdmin: true}}
		r := testReconciler(stub)
		host := types.Host{Meta: types.ObjectMeta{Generation: 1}}

		if _, err := r.Reconcile(context.Background(), host, nil); err != nil {
			t.Fatal(err)
		}
		if !r.privCheckSettled {
			t.Fatal("a clean result must settle")
		}
		// Flip the stub's answer without bumping Generation -- if the second
		// pass actually re-checked, this would show up as not-admin.
		stub.PrivilegeCheckResult = &hyperv.PrivilegeCheck{IsLocalAdmin: false}
		res, err := r.Reconcile(context.Background(), host, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range res.Conditions {
			if c.Type == "Privilege/LocalAdmin" && !c.Status {
				t.Error("throttled pass re-checked instead of replaying the cached (settled) condition")
			}
		}
	})

	t.Run("shortfall: second pass re-checks regardless", func(t *testing.T) {
		stub := &hyperv.Stub{PrivilegeCheckResult: &hyperv.PrivilegeCheck{IsLocalAdmin: false}}
		r := testReconciler(stub)
		host := types.Host{Meta: types.ObjectMeta{Generation: 1}}

		if _, err := r.Reconcile(context.Background(), host, nil); err != nil {
			t.Fatal(err)
		}
		if r.privCheckSettled {
			t.Fatal("a shortfall must not settle")
		}
		// Fix it, still no Generation bump -- a settled implementation would
		// wrongly keep replaying the old shortfall for privilegeCheckEvery
		// passes; this must see the fix immediately.
		stub.PrivilegeCheckResult = &hyperv.PrivilegeCheck{IsLocalAdmin: true}
		res, err := r.Reconcile(context.Background(), host, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range res.Conditions {
			if c.Type == "Privilege/LocalAdmin" && !c.Status {
				t.Error("a fixed shortfall was not reflected on the very next pass")
			}
		}
	})
}

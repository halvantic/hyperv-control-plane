package main

import (
	"reflect"
	"testing"

	"github.com/joshua-fourie/ballast/agent/reconcile"
	"github.com/joshua-fourie/ballast/api/types"
)

/* The gap between what the agent OBSERVES and what it REPORTS has no guard, and
   it has now swallowed two fields in one session.

   ClusterStatus.ReplicaBroker was added to the schema, the proto and both
   converters, with a round-trip test proving the wire carried it — and it still
   never arrived, because the agent never copied it out of the reconcile result.
   VMStatus.MemoryDemandBytes was worse: observed by the agent, carried by the
   proto, read correctly by the console, and dropped in the middle for long
   enough that every VM read 100% memory used, because assigned equals startup on
   a static-memory VM and that is what was being reported instead.

   The proto every-field guard cannot see this leg. It proves ClusterStatus and
   VMStatus survive the wire; it says nothing about whether anything ever put a
   value in them. This is that missing half: populate every field of a VMResult,
   build a VMStatus from it, and require the ones that correspond by name to
   arrive non-zero.

   A field that legitimately does not map needs an entry in the exception list
   below, with the reason — which is the point. Dropping a value then becomes a
   deliberate, reviewed act rather than an omission nobody notices. */

func TestBuildVMStatusCarriesEveryObservedField(t *testing.T) {
	// Fields of VMResult that deliberately do not become VMStatus fields.
	notReported := map[string]string{
		"Name":     "identifies which VM the status is for; it is the map key, not a status field",
		"Honoured": "decides whether ObservedGeneration advances; not itself reported",
		"Changed":  "tells the runner whether to nudge; not a property of the VM",
		"Phase":    "reported, but as a computed phase rather than a copied value — covered below",
	}
	// The other direction — VMStatus fields with no VMResult counterpart
	// (ObservedGeneration from vmObservedGen, LastReportedAt stamped by the
	// centre, ScreenPNG carried separately and throttled) — is deliberately not
	// checked here. This test is about values the agent observed and failed to
	// pass on, which is the direction that loses data silently.

	res := reconcile.VMResult{
		Name:                "web01",
		Phase:               types.PhaseReady,
		Honoured:            true,
		Changed:             true,
		PowerState:          types.VMPowerRunning,
		VMID:                "vm-guid",
		GuestOS:             "Windows Server 2025",
		IPAddress:           "192.168.1.90",
		GuestFQDN:           "web01.lab.local",
		Checkpoints:         []types.VMCheckpoint{{Name: "cp1"}},
		Observed:            &types.VMObserved{ProcessorCount: 4},
		Replication:         &types.VMReplicationStatus{Mode: "Primary"},
		AssignedMemoryBytes: 4 << 30,
		MemoryDemandBytes:   3 << 30,
		MemoryStatus:        "OK",
		CPUUsagePercent:     12,
		UptimeSeconds:       3600,
		// The integration layer's verdict on the guest. It reached nothing for
		// as long as it existed: the script read it, VMState had no field for
		// it, so VMStatus.Heartbeat was always empty and the console's
		// heartbeatOK was false on every VM. This guard could not catch it
		// because there was no VMResult field to compare against.
		Heartbeat:  "OkApplicationsHealthy",
		Conditions: []types.Condition{{Type: "VMConfigured", Status: true}},
	}

	// Every exported VMResult field must be set, or this proves nothing about it.
	rv := reflect.ValueOf(res)
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if rv.Field(i).IsZero() {
			t.Errorf("this test does not populate VMResult.%s, so it cannot prove buildVMStatus carries it — populate it", f.Name)
		}
	}

	r := &runner{vmObservedGen: map[string]int64{"web01": 7}}
	got := r.buildVMStatus(res)
	gv := reflect.ValueOf(got)

	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if _, skip := notReported[f.Name]; skip {
			continue
		}
		sf := gv.FieldByName(f.Name)
		if !sf.IsValid() {
			t.Errorf("VMResult.%s has no VMStatus counterpart — either report it or add it to notReported with a reason", f.Name)
			continue
		}
		if sf.IsZero() {
			t.Errorf("buildVMStatus drops VMResult.%s: the agent observed it and the report does not carry it", f.Name)
		}
	}

	// The two that were actually lost, asserted by value so a future refactor
	// cannot satisfy the loop above with the wrong field.
	if got.MemoryDemandBytes != 3<<30 {
		t.Errorf("MemoryDemandBytes: want 3GiB, got %d", got.MemoryDemandBytes)
	}
	if got.MemoryStatus != "OK" {
		t.Errorf("MemoryStatus: want OK, got %q", got.MemoryStatus)
	}
	// And the field that comes from the runner rather than the result.
	if got.ObservedGeneration != 7 {
		t.Errorf("ObservedGeneration must come from vmObservedGen, got %d", got.ObservedGeneration)
	}
}

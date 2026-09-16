package hyperv

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* An unreadable observation must never be mistaken for an absent object.

   "Hyper-V did not answer" and "the object is not there" arrive as the same
   empty result, and they have opposite remedies: defer, or create. Reading the
   first as the second is how the agent came to run New-VMSwitch against a live
   SET team carrying the host's management IP — seen on the rig 2026-08-06 when
   VMMS terminated unexpectedly on two nodes. The creates failed only because
   VMMS was fully down.

   Same defect class as the cluster-membership guard, and the same asymmetry
   decides it: being wrong about "present" costs one deferred pass, being wrong
   about "absent" rebuilds the host's networking underneath a running cluster. */

func TestSwitchIsNotCreatedWhenHyperVDidNotAnswer(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"exists":false,"known":false}`)}}
	out, err := newTestPS(f).EnsureSwitch(context.Background(), types.VirtualSwitchSpec{
		Name: "ConvergedSwitch", TeamMembers: []string{"NIC1", "NIC2"},
	})
	if !errors.Is(err, ErrHyperVUnavailable) {
		t.Fatalf("an unreadable observation must surface as ErrHyperVUnavailable, got %v", err)
	}
	if out != OutcomeUnchanged {
		t.Fatalf("nothing may be reported as done, got %v", out)
	}
	// The decisive assertion: only the query ran. A second invocation here is a
	// New-VMSwitch against a switch that may well exist.
	if len(f.calls) != 1 {
		t.Fatalf("expected the observation and nothing else, got %d invocations: %v", len(f.calls), f.calls)
	}
}

func TestMgmtVNICIsNotCreatedWhenHyperVDidNotAnswer(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"exists":false,"known":false}`)}}
	out, err := newTestPS(f).EnsureMgmtVNIC(context.Background(), types.ManagementVNICSpec{
		Name: "Storage", SwitchName: "ConvergedSwitch",
		IPConfig: &types.IPConfig{Address: "10.0.60.10/24"},
	})
	if !errors.Is(err, ErrHyperVUnavailable) {
		t.Fatalf("an unreadable observation must surface as ErrHyperVUnavailable, got %v", err)
	}
	if out != OutcomeUnchanged {
		t.Fatalf("nothing may be reported as done, got %v", out)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected the observation and nothing else, got %d invocations: %v", len(f.calls), f.calls)
	}
}

// The batch is the path a converged host actually takes. When Hyper-V is down it
// must report that for every vNIC rather than falling back to the per-vNIC path,
// which would ask the same dead service the same question three more times.
func TestBatchedVNICsDeferWhenHyperVDidNotAnswer(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"known":false,"adapters":{},"ips":{}}`)}}
	specs := []types.ManagementVNICSpec{
		{Name: "ConvergedSwitch", SwitchName: "ConvergedSwitch"},
		{Name: "LiveMigration", SwitchName: "ConvergedSwitch"},
		{Name: "Storage", SwitchName: "ConvergedSwitch"},
	}
	outs, errs := newTestPS(f).EnsureMgmtVNICs(context.Background(), specs)
	for i, s := range specs {
		if !errors.Is(errs[i], ErrHyperVUnavailable) {
			t.Errorf("%s: want ErrHyperVUnavailable, got %v", s.Name, errs[i])
		}
		if outs[i] != OutcomeUnchanged {
			t.Errorf("%s: nothing may be reported as done, got %v", s.Name, outs[i])
		}
	}
	if len(f.calls) != 1 {
		t.Fatalf("a dead Hyper-V must be asked once, not once per vNIC; got %d invocations", len(f.calls))
	}
}

// The fail-safe: a script that omits the flag entirely reads as unknown, not as
// absent. Absent is the dangerous verdict, so it must be the one that has to be
// stated explicitly.
func TestAnObservationMissingTheKnownFlagDefers(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"exists":false}`)}}
	if _, err := newTestPS(f).EnsureSwitch(context.Background(), types.VirtualSwitchSpec{Name: "ConvergedSwitch"}); !errors.Is(err, ErrHyperVUnavailable) {
		t.Fatalf("an observation with no known flag must not authorise a create, got %v", err)
	}
}

// A vNIC that is genuinely absent is simply missing from the batch's map, and a
// map miss yields the zero observation — whose Known is false. Without filling
// the absences in, every legitimate create would become a permanent deferral and
// a host would never get its vNICs at all.
func TestBatchStillCreatesAVNICHyperVSaysIsAbsent(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{
		[]byte(`{"known":true,"adapters":{},"ips":{}}`), // Hyper-V answered: no vNICs exist
		[]byte(``), // the create
	}}
	outs, errs := newTestPS(f).EnsureMgmtVNICs(context.Background(),
		[]types.ManagementVNICSpec{{Name: "Storage", SwitchName: "ConvergedSwitch"}})
	if errs[0] != nil {
		t.Fatalf("an absent vNIC on a healthy Hyper-V must be created, got %v", errs[0])
	}
	if outs[0] != OutcomeCreated {
		t.Fatalf("want Created, got %v", outs[0])
	}
	if len(f.calls) != 2 || !strings.Contains(f.calls[1], "Add-VMNetworkAdapter") {
		t.Fatalf("expected the create to run, got %v", f.calls)
	}
}

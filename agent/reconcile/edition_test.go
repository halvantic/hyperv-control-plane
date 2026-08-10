package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* Converting a Windows edition is IRREVERSIBLE: there is no way back to an
   evaluation edition and no way down from a higher one. So the rules here are
   about refusing rather than doing — an absent declaration converts nothing, a
   missing key converts nothing, and a failure leaves the host exactly as it was. */

func editionHost(edition, secret string, policy types.RebootPolicy) types.Host {
	h := types.Host{
		Meta: types.ObjectMeta{Name: "hv01", Generation: 1},
		Spec: types.HostSpec{RebootPolicy: policy, EnableHyperVRole: true},
	}
	if edition != "" {
		h.Spec.WindowsLicence = &types.WindowsLicenceSpec{Edition: edition, ProductKeySecret: secret}
	}
	return h
}

func keySecret() map[string]types.Secret {
	return map[string]types.Secret{"key": {
		Name: "key", Type: types.SecretProductKey,
		Data: map[string]string{"productKey": "AAAAA-BBBBB-CCCCC-DDDDD-EEEEE"},
	}}
}

// Nil means "Ballast does not manage the edition", never "keep the current one".
// A schema that treated silence as consent would convert a host nobody meant to
// touch, and there is no way back.
func TestAnUndeclaredEditionConvertsNothing(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	res, done, err := r.reconcileWindowsEdition(context.Background(), editionHost("", "", types.RebootNever), nil)
	if done || err != nil || len(res.Conditions) != 0 {
		t.Fatalf("no declaration means no action: done=%v err=%v conds=%+v", done, err, res.Conditions)
	}
	if stub.EditionAsked != "" {
		t.Fatalf("the host layer must not be asked at all, got %q", stub.EditionAsked)
	}
}

func TestAHostAlreadyOnTheDeclaredEditionIsLeftAlone(t *testing.T) {
	stub := &hyperv.Stub{WindowsLicence: &hyperv.WindowsLicence{Edition: "ServerDatacenter"}}
	r := testReconciler(stub)

	res, done, err := r.reconcileWindowsEdition(context.Background(),
		editionHost("ServerDatacenter", "key", types.RebootNever), keySecret())
	if done || err != nil {
		t.Fatalf("nothing to do: done=%v err=%v", done, err)
	}
	if len(res.Conditions) != 1 || !res.Conditions[0].Status {
		t.Fatalf("a settled edition reports a passing condition, got %+v", res.Conditions)
	}
	if res.Changed {
		t.Error("nothing changed")
	}
}

// The key is a reference, and the reference can fail to resolve. That is the
// centre's fault, not the host's, and the host is running perfectly meanwhile.
func TestAMissingProductKeyIsReportedNotAttempted(t *testing.T) {
	stub := &hyperv.Stub{WindowsLicence: &hyperv.WindowsLicence{Edition: "ServerDatacenterEval"}}
	r := testReconciler(stub)

	res, done, err := r.reconcileWindowsEdition(context.Background(),
		editionHost("ServerDatacenter", "key", types.RebootNever), map[string]types.Secret{})

	if done || err != nil {
		t.Fatalf("a missing key is not a host failure: done=%v err=%v", done, err)
	}
	if len(res.Conditions) != 1 || res.Conditions[0].Status {
		t.Fatalf("it must be reported as unmet, got %+v", res.Conditions)
	}
	if !strings.Contains(res.Conditions[0].Message, "delivery fault at the centre") {
		t.Errorf("the message must say where the fault is: %q", res.Conditions[0].Message)
	}
	if stub.EditionAsked != "" {
		t.Error("no conversion may be attempted without a key")
	}
}

// The key must reach the host layer — a conversion attempted without it fails on
// the host with a worse message than the one above.
func TestTheProductKeyReachesTheHost(t *testing.T) {
	stub := &hyperv.Stub{WindowsLicence: &hyperv.WindowsLicence{Edition: "ServerDatacenterEval"}}
	r := testReconciler(stub)

	r.reconcileWindowsEdition(context.Background(),
		editionHost("ServerDatacenter", "key", types.RebootNever), keySecret())

	if stub.EditionKeySeen != "AAAAA-BBBBB-CCCCC-DDDDD-EEEEE" {
		t.Fatalf("the product key must be delivered to the conversion, got %q", stub.EditionKeySeen)
	}
}

// A staged conversion changes the OS on the next boot, so the pass stops and the
// restart is governed by RebootPolicy — the same rule as the domain join and the
// role install, because it is the same kind of pending change.
func TestARebootIsGovernedByPolicy(t *testing.T) {
	stub := &hyperv.Stub{WindowsLicence: &hyperv.WindowsLicence{Edition: "ServerDatacenterEval"}}
	r := testReconciler(stub)

	res, done, err := r.reconcileWindowsEdition(context.Background(),
		editionHost("ServerDatacenter", "key", types.RebootNever), keySecret())
	if err != nil || !done {
		t.Fatalf("a staged conversion must stop the pass: done=%v err=%v", done, err)
	}
	if !res.RebootRequired {
		t.Error("RebootNever must surface reboot-required rather than rebooting")
	}
	if stub.RebootCalled {
		t.Error("RebootNever must not reboot the host")
	}

	stub2 := &hyperv.Stub{WindowsLicence: &hyperv.WindowsLicence{Edition: "ServerDatacenterEval"}}
	r2 := testReconciler(stub2)
	res2, done2, _ := r2.reconcileWindowsEdition(context.Background(),
		editionHost("ServerDatacenter", "key", types.RebootIfNeeded), keySecret())
	if !done2 || !stub2.RebootCalled {
		t.Errorf("IfNeeded must restart the host itself: done=%v rebooted=%v", done2, stub2.RebootCalled)
	}
	if res2.RebootRequired {
		t.Error("a host being restarted now is not one waiting for a restart")
	}
}

// A conversion that cannot happen leaves the host exactly as it was and working.
// Degrading it would hold back every other piece of desired state for something
// that is not broken.
func TestAFailedConversionIsAdvisory(t *testing.T) {
	stub := &hyperv.Stub{FailEdition: true, WindowsLicence: &hyperv.WindowsLicence{Edition: "ServerDatacenterEval"}}
	r := testReconciler(stub)

	res, done, err := r.reconcileWindowsEdition(context.Background(),
		editionHost("ServerDatacenter", "key", types.RebootNever), keySecret())

	if done || err != nil {
		t.Fatalf("a refused conversion must not stop or fail the pass: done=%v err=%v", done, err)
	}
	if len(res.Conditions) != 1 || res.Conditions[0].Reason == "ApplyFailed" {
		t.Fatalf("it must be advisory, not degrading: %+v", res.Conditions)
	}
}

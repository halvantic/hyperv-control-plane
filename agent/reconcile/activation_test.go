package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* Activation is a different act from a conversion, on a different schedule and
   with a different cost: a conversion happens once and consumes nothing, an
   activation consumes a seat from a key pool and can be repeated after hardware
   changes. So they are declared separately and reconciled separately. */

func activationHost(method, keySecret, kms string) types.Host {
	return types.Host{
		Meta: types.ObjectMeta{Name: "hv01", Generation: 1},
		Spec: types.HostSpec{
			RebootPolicy: types.RebootNever,
			WindowsLicence: &types.WindowsLicenceSpec{
				Edition: "ServerDatacenter", Activation: method,
				ActivationKeySecret: keySecret, KMSServer: kms,
			},
		},
	}
}

func activationSecret() map[string]types.Secret {
	return map[string]types.Secret{"mak": {
		Name: "mak", Type: types.SecretProductKey,
		Data: map[string]string{"productKey": "MAKKK-BBBBB-CCCCC-DDDDD-EEEEE"},
	}}
}

func TestNoDeclaredActivationDoesNothing(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	h := activationHost("", "", "")

	if conds := r.reconcileWindowsActivation(context.Background(), h, nil); len(conds) != 0 {
		t.Fatalf("nothing declared means nothing done, got %+v", conds)
	}
	if stub.ActivationAsked != "" {
		t.Fatalf("the host layer must not be asked, got %q", stub.ActivationAsked)
	}
}

func TestMAKActivationDeliversItsKey(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	r.reconcileWindowsActivation(context.Background(),
		activationHost(types.ActivationMAK, "mak", ""), activationSecret())

	if stub.ActivationAsked != "MAK" {
		t.Errorf("method: %q", stub.ActivationAsked)
	}
	if stub.ActivationKeySeen != "MAKKK-BBBBB-CCCCC-DDDDD-EEEEE" {
		t.Errorf("the key must reach the host, got %q", stub.ActivationKeySeen)
	}
}

// A KMS client often needs no key at all — it already carries a volume one — and
// only wants pointing at a server. Requiring a key would block that.
func TestKMSActivationWorksWithNoKey(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	conds := r.reconcileWindowsActivation(context.Background(),
		activationHost(types.ActivationKMS, "", "kms.ballast.local"), nil)

	if stub.ActivationAsked != "KMS" || stub.ActivationKMS != "kms.ballast.local" {
		t.Fatalf("the KMS server must reach the host: method=%q kms=%q", stub.ActivationAsked, stub.ActivationKMS)
	}
	if len(conds) != 1 {
		t.Fatalf("the outcome must be reported, got %+v", conds)
	}
}

// The key is a reference and the reference can fail to resolve. That is the
// centre's fault, and the message says so rather than blaming the host.
func TestAMissingActivationKeyIsReportedNotAttempted(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	conds := r.reconcileWindowsActivation(context.Background(),
		activationHost(types.ActivationMAK, "mak", ""), map[string]types.Secret{})

	if len(conds) != 1 || conds[0].Status {
		t.Fatalf("it must be reported as unmet, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "delivery fault at the centre") {
		t.Errorf("the message must place the fault: %q", conds[0].Message)
	}
	if stub.ActivationAsked != "" {
		t.Error("no activation may be attempted without the key it was told to use")
	}
}

// An unactivated host runs, serves VMs and reconciles everything else perfectly.
// Windows nags and eventually restricts, but that is not a reason to hold back a
// host's entire desired state.
func TestAFailedActivationIsAdvisory(t *testing.T) {
	stub := &hyperv.Stub{FailActivation: true}
	r := testReconciler(stub)

	conds := r.reconcileWindowsActivation(context.Background(),
		activationHost(types.ActivationMAK, "mak", ""), activationSecret())

	if len(conds) != 1 {
		t.Fatalf("expected one condition, got %+v", conds)
	}
	if conds[0].Reason == "ApplyFailed" {
		t.Errorf("a failed activation must be advisory, not degrading: %+v", conds[0])
	}
}

// No key can activate an evaluation edition — the remedy is a conversion. The
// refusal must survive to the operator rather than reading as a key problem.
func TestAnEvaluationHostSaysWhyActivationCannotWork(t *testing.T) {
	stub := &hyperv.Stub{WindowsLicence: &hyperv.WindowsLicence{Edition: "ServerDatacenterEval", Evaluation: true}}
	r := testReconciler(stub)

	conds := r.reconcileWindowsActivation(context.Background(),
		activationHost(types.ActivationMAK, "mak", ""), activationSecret())

	if len(conds) != 1 || conds[0].Status {
		t.Fatalf("it must be reported, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "evaluation") {
		t.Errorf("the message must name the evaluation edition as the cause: %q", conds[0].Message)
	}
}

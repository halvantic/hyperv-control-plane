package ballastpb

import (
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* A field the proto does not carry round-trips as its zero value, silently. For
   this one the silent value is the dangerous one: Evaluation false means "this
   host can be activated", and a fleet of evaluation hosts would report as
   ordinary unlicensed ones with a remedy that cannot work. */

func TestTheWindowsLicenceSurvivesTheProto(t *testing.T) {
	in := types.HostStatus{
		WindowsLicence: &types.WindowsLicenceStatus{
			Edition: "ServerDatacenterEval", Description: "Microsoft Windows Server 2025 Datacenter Evaluation",
			Evaluation: true, Status: "Notification", GraceDaysRemaining: 118,
			Channel: "Evaluation", PartialProductKey: "ABCDE", KMSServer: "kms.ballast.local",
			Message: "this is an evaluation edition and cannot be activated",
		},
	}

	got := HostFromProto(HostToProto(types.Host{Status: in})).Status.WindowsLicence

	if got == nil {
		t.Fatal("the licence did not survive; every host would report as having none")
	}
	if !got.Evaluation {
		t.Error("Evaluation lost — the fleet would read as activatable when no key can work")
	}
	if got.GraceDaysRemaining != 118 {
		t.Errorf("grace days: got %d want 118 — the countdown is the point of showing it", got.GraceDaysRemaining)
	}
	for _, c := range []struct{ name, got, want string }{
		{"edition", got.Edition, in.WindowsLicence.Edition},
		{"description", got.Description, in.WindowsLicence.Description},
		{"status", got.Status, in.WindowsLicence.Status},
		{"channel", got.Channel, in.WindowsLicence.Channel},
		{"partial key", got.PartialProductKey, in.WindowsLicence.PartialProductKey},
		{"KMS server", got.KMSServer, in.WindowsLicence.KMSServer},
		{"message", got.Message, in.WindowsLicence.Message},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.got, c.want)
		}
	}
}

// A host that has not reported one must not grow an empty licence, or every host
// would show a licence panel asserting an unknown edition is not an evaluation.
func TestAHostWithNoLicenceReportedStaysNil(t *testing.T) {
	got := HostFromProto(HostToProto(types.Host{})).Status.WindowsLicence
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// The DECLARED edition. A spec field the proto drops round-trips as nil, and nil
// means "Ballast does not manage the edition" — so the centre would store a
// conversion the agent never hears about, and nothing anywhere would say why the
// host stayed on its evaluation edition.
func TestTheDeclaredEditionSurvivesTheProto(t *testing.T) {
	in := types.HostSpec{
		WindowsLicence: &types.WindowsLicenceSpec{
			Edition: "ServerDatacenter", ProductKeySecret: "datacenter-key",
		},
	}

	got := HostFromProto(HostToProto(types.Host{Spec: in})).Spec.WindowsLicence

	if got == nil {
		t.Fatal("the declared edition did not survive; the agent would never convert")
	}
	if got.Edition != "ServerDatacenter" {
		t.Errorf("edition: %q", got.Edition)
	}
	// The NAME of the secret crosses; the key itself never does.
	if got.ProductKeySecret != "datacenter-key" {
		t.Errorf("product key secret: %q — without it the conversion has no key", got.ProductKeySecret)
	}
}

func TestAHostDeclaringNoEditionStaysNil(t *testing.T) {
	if got := HostFromProto(HostToProto(types.Host{})).Spec.WindowsLicence; got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// The spec side matters more than the status side, and fails more quietly. A
// field the proto does not carry round-trips as its zero value: the centre stores
// the activation method, the agent never receives it, and the host sits
// unactivated with nothing anywhere reporting a problem.
func TestTheLicenceSpecSurvivesTheProto(t *testing.T) {
	in := &types.WindowsLicenceSpec{
		Edition:             "ServerDatacenter",
		ProductKeySecret:    "conversion-key",
		Activation:          types.ActivationKMS,
		ActivationKeySecret: "gvlk",
		KMSServer:           "kms.ballast.local:1688",
	}

	got := HostFromProto(HostToProto(types.Host{
		Spec: types.HostSpec{WindowsLicence: in},
	})).Spec.WindowsLicence
	if got == nil {
		t.Fatal("the licence spec did not survive at all")
	}

	for _, c := range []struct{ name, got, want string }{
		{"edition", got.Edition, in.Edition},
		{"product key secret", got.ProductKeySecret, in.ProductKeySecret},
		{"activation method", got.Activation, in.Activation},
		{"activation key secret", got.ActivationKeySecret, in.ActivationKeySecret},
		{"KMS server", got.KMSServer, in.KMSServer},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// The conversion key and the activation key are routinely DIFFERENT keys — a
// retail key to convert and a GVLK to activate — so they must not collapse into
// one field on the way across.
func TestTheTwoKeysStaySeparateAcrossTheProto(t *testing.T) {
	got := HostFromProto(HostToProto(types.Host{Spec: types.HostSpec{
		WindowsLicence: &types.WindowsLicenceSpec{
			ProductKeySecret: "retail", ActivationKeySecret: "mak",
		},
	}})).Spec.WindowsLicence

	if got.ProductKeySecret == got.ActivationKeySecret {
		t.Fatalf("the two key references collapsed into one: %q", got.ProductKeySecret)
	}
}

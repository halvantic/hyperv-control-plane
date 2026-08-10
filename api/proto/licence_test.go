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

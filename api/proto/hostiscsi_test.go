package ballastpb

import (
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// A standalone host's array connection has to survive the wire in both
// directions, or the centre stores a configuration the agent never receives and
// nothing anywhere reports a problem.

func TestAStandaloneHostsISCSISpecSurvivesTheProto(t *testing.T) {
	on := true
	in := types.HostSpec{
		Storage: types.HostStorageSpec{
			DefaultVMPath: `D:\VMs`,
			ISCSI: &types.ISCSIStorageSpec{
				Portals:          []string{"10.0.60.52", "10.0.60.53"},
				Targets:          []string{"iqn.2000-01.com.synology:Xpenology.default-target.aa584d33bc2"},
				CredentialSecret: "iSCSI Chap Auth",
				MutualCHAP:       true,
				EnableMPIO:       &on,
			},
		},
	}

	out := HostFromProto(HostToProto(types.Host{Spec: in})).Spec

	got := out.Storage.ISCSI
	if got == nil {
		t.Fatal("the host's iSCSI spec did not survive; the agent would receive a host with no array")
	}
	if len(got.Portals) != 2 || got.Portals[0] != "10.0.60.52" {
		t.Errorf("portals: %v", got.Portals)
	}
	if got.CredentialSecret != "iSCSI Chap Auth" {
		t.Errorf("credential: %q — without it the node cannot authenticate", got.CredentialSecret)
	}
	if !got.MutualCHAP {
		t.Error("mutual CHAP lost; the login would be attempted the wrong way round")
	}
	if got.EnableMPIO == nil || !*got.EnableMPIO {
		t.Error("the MPIO setting lost its value")
	}
	// The neighbouring fields must be untouched by the addition.
	if out.Storage.DefaultVMPath != `D:\VMs` {
		t.Errorf("default VM path: %q", out.Storage.DefaultVMPath)
	}
}

func TestAStandaloneHostsISCSIStatusSurvivesTheProto(t *testing.T) {
	in := types.HostStatus{
		Phase: types.PhaseReady,
		ISCSI: &types.ISCSIStatus{
			Node:           "hv09",
			InitiatorIQN:   "iqn.1991-05.com.microsoft:hv09.ballast.local",
			ServiceRunning: true,
			Portals:        []string{"10.0.60.52:3260"},
			MPIOInstalled:  true,
			MPIOEffective:  true,
			Sessions:       []types.ISCSISession{{TargetIQN: "iqn.test:t1", Connected: true, Persistent: true, Paths: 2}},
			Disks:          []types.ISCSIDisk{{SerialNumber: "ABC123", SizeBytes: 1 << 40}},
		},
	}

	out := HostFromProto(HostToProto(types.Host{Status: in})).Status

	got := out.ISCSI
	if got == nil {
		t.Fatal("the host's iSCSI status did not survive; the console would show a host with no array state")
	}
	if got.InitiatorIQN != in.ISCSI.InitiatorIQN {
		t.Errorf("initiator IQN: %q — this is the value an operator needs before anything works", got.InitiatorIQN)
	}
	if !got.MPIOEffective {
		t.Error("MPIOEffective lost; the console cannot tell installed from protecting")
	}
	if len(got.Sessions) != 1 || !got.Sessions[0].Connected || got.Sessions[0].Paths != 2 {
		t.Errorf("sessions: %+v", got.Sessions)
	}
	if len(got.Disks) != 1 || got.Disks[0].SerialNumber != "ABC123" {
		t.Errorf("disks: %+v", got.Disks)
	}
}

// A host with no array must not grow one, or every host would report an empty
// iSCSI panel it never asked for.
func TestAHostWithNoArrayStaysThatWay(t *testing.T) {
	out := HostFromProto(HostToProto(types.Host{})).Spec
	if out.Storage.ISCSI != nil {
		t.Fatalf("expected no iSCSI spec, got %+v", out.Storage.ISCSI)
	}
	st := HostFromProto(HostToProto(types.Host{})).Status
	if st.ISCSI != nil {
		t.Fatalf("expected no iSCSI status, got %+v", st.ISCSI)
	}
}

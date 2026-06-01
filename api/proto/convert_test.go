package ballastpb

import (
	"reflect"
	"testing"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

// sampleHost is a fully-populated Host used to prove the conversion is lossless.
// Times are constructed without a monotonic component so reflect.DeepEqual holds
// after a timestamppb round-trip.
func sampleHost() types.Host {
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	return types.Host{
		Meta: types.ObjectMeta{
			Name:       "host01",
			UID:        "abc123",
			Generation: 7,
			Labels:     map[string]string{"role": "compute", "rack": "R12"},
			CreatedAt:  ts,
			UpdatedAt:  ts.Add(time.Hour),
		},
		Spec: types.HostSpec{
			FQDN:             "host01.lab.local",
			EnableHyperVRole: true,
			RebootPolicy:     types.RebootIfNeeded,
			Networking: types.HostNetworkingSpec{
				Switches: []types.VirtualSwitchSpec{{
					Name:              "ConvergedSwitch",
					TeamMembers:       []string{"NIC1", "NIC2"},
					TeamingMode:       types.SETSwitchIndependent,
					LoadBalancing:     types.SETDynamic,
					AllowManagementOS: true,
				}},
				ManagementVNICs: []types.ManagementVNICSpec{{
					Name:       "Management",
					SwitchName: "ConvergedSwitch",
					VLANID:     10,
					IPConfig: &types.IPConfig{
						Address:    "10.0.0.21/24",
						Gateway:    "10.0.0.1",
						DNSServers: []string{"10.0.0.1", "10.0.0.2"},
					},
					MinBandwidthWeight: 10,
				}},
			},
			Storage: types.HostStorageSpec{
				ContributeToS2D:      true,
				EligibleDiskSelector: map[string]string{"media": "SSD"},
			},
			ClusterMembership: &types.ClusterMembershipSpec{ClusterName: "cluster01"},
		},
		Status: types.HostStatus{
			Phase:              types.PhaseReady,
			ObservedGeneration: 7,
			HyperVInstalled:    true,
			RebootRequired:     false,
			Autonomous:         true,
			LastContact:        ts.Add(2 * time.Hour),
			Inventory: types.HostInventory{
				PhysicalAdapters: []types.PhysicalAdapter{
					{Name: "NIC1", MAC: "00:15:5D:00:00:01", LinkSpeedBps: 25_000_000_000, Up: true},
				},
				PhysicalDisks: []types.PhysicalDisk{
					{DeviceID: "0", SizeBytes: 1_920_383_410_176, MediaType: "SSD", CanPool: true},
				},
				TotalMemoryBytes: 137_438_953_472,
				LogicalCPUs:      32,
			},
			Conditions: []types.Condition{{
				Type:               "SwitchConfigured",
				Status:             true,
				Reason:             "Applied",
				Message:            "SET switch present",
				LastTransitionTime: ts,
			}},
		},
	}
}

func TestHostRoundTrip(t *testing.T) {
	in := sampleHost()
	got := HostFromProto(HostToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round trip mismatch:\n in:  %#v\n got: %#v", in, got)
	}
}

func TestHostFromProtoNil(t *testing.T) {
	if got := HostFromProto(nil); !reflect.DeepEqual(got, types.Host{}) {
		t.Fatalf("expected zero Host from nil, got %#v", got)
	}
}

func TestEnumsUnspecifiedForUnknown(t *testing.T) {
	if p := phaseToProto(types.Phase("bogus")); p != Phase_PHASE_UNSPECIFIED {
		t.Errorf("unknown phase: want UNSPECIFIED, got %v", p)
	}
	if r := rebootPolicyToProto(types.RebootPolicy("bogus")); r != RebootPolicy_REBOOT_POLICY_UNSPECIFIED {
		t.Errorf("unknown reboot policy: want UNSPECIFIED, got %v", r)
	}
}

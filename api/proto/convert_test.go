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
	if p := vmPowerStateToProto(types.VMPowerState("bogus")); p != VMPowerState_VM_POWER_STATE_UNSPECIFIED {
		t.Errorf("unknown power state: want UNSPECIFIED, got %v", p)
	}
	if a := vmStartActionToProto(types.VMStartAction("bogus")); a != VMStartAction_VM_START_ACTION_UNSPECIFIED {
		t.Errorf("unknown start action: want UNSPECIFIED, got %v", a)
	}
}

// sampleVM is a fully-populated VM used to prove the conversion is lossless.
func sampleVM() types.VM {
	ts := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	return types.VM{
		Meta: types.ObjectMeta{
			Name:       "web01",
			UID:        "vm-abc123",
			Generation: 3,
			Labels:     map[string]string{"tier": "web"},
			CreatedAt:  ts,
			UpdatedAt:  ts.Add(time.Hour),
		},
		Spec: types.VMSpec{
			Placement:          types.VMPlacementSpec{HostName: "host01"},
			HyperVGeneration:   2,
			ProcessorCount:     4,
			MemoryStartupBytes: 4_294_967_296,
			DynamicMemory:      &types.DynamicMemorySpec{MinBytes: 2_147_483_648, MaxBytes: 8_589_934_592},
			Disks: []types.VMDiskSpec{
				{Path: `C:\ClusterStorage\Volume1\web01.vhdx`, SizeBytes: 137_438_953_472, Dynamic: true},
			},
			NetworkAdapters: []types.VMNetworkAdapterSpec{
				{Name: "net0", SwitchName: "ConvergedSwitch", VLANID: 10, MACAddress: "00:15:5D:00:01:02"},
			},
			DesiredPowerState:    types.VMPowerRunning,
			AutomaticStartAction: types.VMStartIfWasRunning,
		},
		Status: types.VMStatus{
			Phase:               types.PhaseReady,
			ObservedGeneration:  3,
			PowerState:          types.VMPowerRunning,
			AssignedMemoryBytes: 4_294_967_296,
			CPUUsagePercent:     12,
			UptimeSeconds:       3600,
			Conditions: []types.Condition{{
				Type:               "VMConfigured",
				Status:             true,
				Reason:             "Applied",
				Message:            "VM matches desired state",
				LastTransitionTime: ts,
			}},
		},
	}
}

func TestVMRoundTrip(t *testing.T) {
	in := sampleVM()
	got := VMFromProto(VMToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round trip mismatch:\n in:  %#v\n got: %#v", in, got)
	}
}

func TestVMFromProtoNil(t *testing.T) {
	if got := VMFromProto(nil); !reflect.DeepEqual(got, types.VM{}) {
		t.Fatalf("expected zero VM from nil, got %#v", got)
	}
}

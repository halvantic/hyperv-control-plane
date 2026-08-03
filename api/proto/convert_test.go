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
			ISOPath:              `C:\ClusterStorage\Volume1\ISOs\boot.iso`,
			SecureBoot:           "linux",
			BootOrder:            []string{"DVD", "Drive", "Network"},
			Replication: &types.VMReplicationSpec{
				Enabled:            true,
				TargetHost:         "host04",
				TargetCluster:      "cluster02",
				ReplicaServer:      "host04.lab.local",
				FrequencySeconds:   300,
				AuthenticationType: "Kerberos",
				Port:               80,
			},
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

// The round-trip test only proves a field survives if the sample actually sets
// it: a field the proto never carries round-trips perfectly as its zero value.
// SecureBoot and BootOrder were added to VMSpec but not to the proto, and the
// gap went unnoticed for exactly this reason — the centre stored them, the agent
// received empty values, defaulted Secure Boot to on, and reported the VM as
// already matching desired state while a Linux guest refused to boot.
//
// So require every exported VMSpec field to be non-zero in the sample. Adding a
// field to the schema now fails here until it is populated, which in turn makes
// the round trip prove the proto carries it.
func TestSampleVMSpecCoversEveryField(t *testing.T) {
	v := reflect.ValueOf(sampleVM().Spec)
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("sampleVM does not set VMSpec.%s, so TestVMRoundTrip cannot prove the proto carries it — populate it", f.Name)
		}
	}
}

func TestVMFromProtoNil(t *testing.T) {
	if got := VMFromProto(nil); !reflect.DeepEqual(got, types.VM{}) {
		t.Fatalf("expected zero VM from nil, got %#v", got)
	}
}

// sampleClusterStatus is fully populated for the same reason sampleVM is: a
// field the proto never carries round-trips perfectly as its zero value, so a
// sample that leaves fields unset proves nothing.
func sampleClusterStatus() types.ClusterStatus {
	return types.ClusterStatus{
		Phase:              types.PhaseReady,
		ObservedGeneration: 7,
		FormedMembers:      []string{"n1", "n2"},
		S2DEnabled:         true,
		Nodes:              []types.ClusterNodeStatus{{Name: "n1", State: "Up"}},
		Groups:             []types.ClusterGroupStatus{{Name: "Cluster Group", OwnerNode: "n1", State: "Online", GroupType: "Cluster"}},
		VMs:                []types.ClusterVMStatus{{Name: "Web01", OwnerNode: "n2", State: "Online"}},
		CSVs: []types.CSVStatus{{
			Name: "Cluster Virtual Disk (Vol01)", OwnerNode: "n1", State: "Online",
			Health: "Warning", Operational: "Degraded", DetachedReason: "By Policy",
			SizeBytes: 322055438336, FreeBytes: 273657683968,
		}},
		Pool: &types.ClusterPoolStatus{
			Name: "S2D on c1", RawBytes: 1 << 40, AllocatedBytes: 1 << 38,
			Health: "Warning", Operational: "Degraded", UnhealthyDisks: 1, TotalDisks: 12,
			Resyncing: true, ResyncPercent: 41, ResyncJob: "Repair", ResyncRemainingBytes: 1 << 35,
		},
		Networks: []types.ClusterNetworkStatus{{
			Name: "Cluster Network 2", CIDR: "10.0.50.0/24", Role: "Cluster", State: "Up", Metric: 30000,
		}},
		Conditions: []types.Condition{{Type: "Cluster", Status: true, Reason: "Formed", Message: "ok"}},
	}
}

// Cluster status crosses the wire like everything else, and until now nothing
// proved it. The pool's resync fields, the CSVs' own health, and the networks'
// metric were all added to the schema without a round-trip test — each could
// have been dropped by the proto and reported as a confident zero.
func TestClusterStatusRoundTrip(t *testing.T) {
	in := sampleClusterStatus()
	got := ClusterStatusFromProto(ClusterStatusToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round trip mismatch:\n in:  %#v\n got: %#v", in, got)
	}
}

// As with VMSpec: require every exported field of the nested status structs to
// be set, so adding one fails here until the sample populates it and the round
// trip is made to prove the proto carries it.
func TestSampleClusterStatusCoversEveryField(t *testing.T) {
	st := sampleClusterStatus()
	check := func(name string, v reflect.Value) {
		t.Helper()
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if v.Field(i).IsZero() {
				t.Errorf("sampleClusterStatus does not set %s.%s, so the round trip cannot prove the proto carries it — populate it", name, f.Name)
			}
		}
	}
	check("ClusterStatus", reflect.ValueOf(st))
	check("ClusterPoolStatus", reflect.ValueOf(*st.Pool))
	check("CSVStatus", reflect.ValueOf(st.CSVs[0]))
	check("ClusterNetworkStatus", reflect.ValueOf(st.Networks[0]))
	check("ClusterNodeStatus", reflect.ValueOf(st.Nodes[0]))
	check("ClusterGroupStatus", reflect.ValueOf(st.Groups[0]))
	check("ClusterVMStatus", reflect.ValueOf(st.VMs[0]))
}

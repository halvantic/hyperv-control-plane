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
			ClusterNode:        "Quarantined",
			ClusterService:     "Stopped",
			Autonomous:         true,
			LastContact:        ts.Add(2 * time.Hour),
			Inventory: types.HostInventory{
				// Every field of both is set, including combinations no real host
				// would report (a disk that is both poolable and in a pool). These
				// prove the WIRE carries each field; the guard at the bottom of
				// TestHostRoundTripCarriesEveryField fails if one is left zero,
				// because a field the proto drops round-trips perfectly as zero.
				PhysicalAdapters: []types.PhysicalAdapter{
					{
						Name: "NIC1", MAC: "00:15:5D:00:00:01", LinkSpeedBps: 25_000_000_000, Up: true,
						IsManagement: true, IPv4: "192.168.1.50", PrefixLength: 24,
						DNSServers: []string{"192.168.1.168"}, RegistersDNS: true, Gateway: "192.168.1.1",
					},
				},
				PhysicalDisks: []types.PhysicalDisk{
					{
						DeviceID: "0", UniqueID: "60022480", BusType: "SAS",
						SizeBytes: 1_920_383_410_176, MediaType: "SSD", CanPool: true,
						IsOSDisk: true, DriveLetter: "E",
						PoolName: "S2D on hv-cl01", CannotPoolReason: "In a Pool",
						// Retired is the value that matters: it reports Healthy and
						// contributes nothing, so it must survive the wire or the
						// console cannot explain a pool that will not allocate.
						Usage: "Retired",
					},
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
			ComputerName:         "WEB01-GUEST",
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
			Heartbeat:           "OkApplicationsUnknown",
			ScreenBlank:         true,
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
		StateUnreadable:    true,
		FormedMembers:      []string{"n1", "n2"},
		S2DEnabled:         true,
		Nodes: []types.ClusterNodeStatus{
			{Name: "n1", State: "Up", StatusInformation: "Normal"},
			// A quarantined node reports State=Down like a switched-off one, so the
			// distinguishing field is the one that most needs to survive the wire.
			{Name: "n2", State: "Down", StatusInformation: "Quarantined"},
		},
		// A group carrying its resources: the case the field exists for is a group
		// that is not Online, where the group state names the group and not the
		// fault. Two resources up and one down is the shape seen on the rig.
		Groups: []types.ClusterGroupStatus{{
			Name: "SDDC Group", OwnerNode: "n1", State: "PartialOnline", GroupType: "CoreSddc",
			Resources: []types.ClusterResourceStatus{
				{Name: "Health", Type: "Health Service", State: "Online"},
				{Name: "SDDC Management", Type: "SDDC Management", State: "Offline"},
			},
		}},
		VMs: []types.ClusterVMStatus{{Name: "Web01", OwnerNode: "n2", State: "Online"}},
		CSVs: []types.CSVStatus{{
			Name: "Cluster Virtual Disk (Vol01)", OwnerNode: "n1", State: "Online",
			Health: "Warning", Operational: "Degraded", DetachedReason: "By Policy",
			SizeBytes: 322055438336, FreeBytes: 273657683968,
		}},
		Pool: &types.ClusterPoolStatus{
			Name: "S2D on c1", RawBytes: 1 << 40, AllocatedBytes: 1 << 38,
			Health: "Warning", Operational: "Degraded", UnhealthyDisks: 1, DisksInMaintenance: 4, TotalDisks: 12,
			Resyncing: true, ResyncPercent: 41, ResyncJob: "Repair", ResyncRemainingBytes: 1 << 35,
		},
		Networks: []types.ClusterNetworkStatus{{
			Name: "Cluster Network 2", CIDR: "10.0.50.0/24", Role: "Cluster", State: "Up", Metric: 30000,
		}},
		Witness: &types.ClusterWitnessStatus{
			Type: types.WitnessFileShare, Path: `\fileserver\c1-witness`,
			State: "Online", QuorumType: "NodeAndFileShareMajority",
		},
		ReplicaBroker: &types.ClusterReplicaBrokerStatus{
			Name: "c1-Brk.lab.local", State: "Online",
			StorageLocation: `C:\ClusterStorage\Vol01\Replica`,
		},
		FunctionalLevel: 12,
		NodeOSBuild:     26100,
		ISCSI: []types.ISCSIStatus{{
			Node: "n1", InitiatorIQN: "iqn.1991-05.com.microsoft:n1.lab.local",
			ServiceRunning: true, Portals: []string{"10.0.70.10:3260"},
			MPIOInstalled: true, MPIOEffective: true, Message: "connected",
			Sessions: []types.ISCSISession{{
				TargetIQN: "iqn.2000-01.com.synology:nas.target-1", Connected: true, Persistent: true, Paths: 2,
			}},
			Disks: []types.ISCSIDisk{{
				SerialNumber: "6001405abcdef", Number: 4, SizeBytes: 1 << 40,
				TargetIQN: "iqn.2000-01.com.synology:nas.target-1", LUN: 1, Clustered: true, Offline: true,
			}},
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

	// Fields the proto deliberately does not carry, and why. Keep this list
	// short and justified: every entry is a field the round trip cannot vouch
	// for, which is the exact hazard this test exists to catch.
	centreOnly := map[string]string{
		// Stamped by the centre on receipt so one clock decides freshness. If the
		// agent set it, a host with a skewed or wrong clock could make a stale
		// snapshot look current and defeat the gate that reads it.
		"ClusterStatus.ObservedAt": "stamped by the centre on receipt; see ClusterStatus.ObservedAt",
	}

	check := func(name string, v reflect.Value) {
		t.Helper()
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if _, skip := centreOnly[name+"."+f.Name]; skip {
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
	check("ClusterResourceStatus", reflect.ValueOf(st.Groups[0].Resources[0]))
	check("ClusterWitnessStatus", reflect.ValueOf(*st.Witness))
	check("ClusterReplicaBrokerStatus", reflect.ValueOf(*st.ReplicaBroker))
	check("ISCSIStatus", reflect.ValueOf(st.ISCSI[0]))
	check("ISCSISession", reflect.ValueOf(st.ISCSI[0].Sessions[0]))
	check("ISCSIDisk", reflect.ValueOf(st.ISCSI[0].Disks[0]))
	check("ClusterVMStatus", reflect.ValueOf(st.VMs[0]))
}

// The same guard as TestSampleClusterStatusCoversEveryField, for the host.
//
// HostSpec.Maintenance and HostStatus.InMaintenance were added to the Go types
// and not to the proto, so the centre stored the operator's intent, HostToProto
// silently dropped it, and the agent received nil — the node never drained while
// the console showed "draining" over a host still running its VMs. A field the
// proto does not carry round-trips perfectly as its zero value, which is exactly
// what makes this class invisible.
//
// ClusterStatus had this guard because the same thing happened there. The host is
// the object every agent pulls on every cycle; it needed one more.
func TestHostRoundTripCarriesEveryField(t *testing.T) {
	h := types.Host{
		Meta: types.ObjectMeta{Name: "hv01", UID: "u1", Generation: 7},
		Spec: types.HostSpec{
			FQDN:             "hv01.lab.local",
			EnableHyperVRole: true,
			RebootPolicy:     types.RebootIfNeeded,
			ComputerName:     "HV01",
			Maintenance:      &types.MaintenanceSpec{Enabled: true, Reason: "firmware"},
		},
	}
	got := HostFromProto(HostToProto(h))
	if got.Spec.Maintenance == nil {
		t.Fatal("HostSpec.Maintenance did not survive the round trip — the agent would never see the intent, so the node would never drain")
	}
	if !got.Spec.Maintenance.Enabled || got.Spec.Maintenance.Reason != "firmware" {
		t.Fatalf("maintenance mangled: %+v", got.Spec.Maintenance)
	}

	st := types.HostStatus{Phase: types.PhaseReady, InMaintenance: true}
	if !StatusFromProto(StatusToProto(st)).InMaintenance {
		t.Fatal("HostStatus.InMaintenance did not survive the round trip — the centre could never tell asked-to-drain from drained")
	}

	// WHICH cluster a host is in. ClusterNode and ClusterService say how its
	// membership is and never of what, so a host still joined to a cluster
	// somebody thought they had removed reports Up, Running and healthy. Dropped
	// on the wire it would read as "in no cluster", which is the same wrong
	// answer by a different route.
	member := types.HostStatus{Phase: types.PhaseReady, ClusterNode: "Up", ClusterService: "Running", ClusterNodeOf: "NewCluster"}
	if got := StatusFromProto(StatusToProto(member)); got.ClusterNodeOf != "NewCluster" {
		t.Fatalf("HostStatus.ClusterNodeOf did not survive the round trip (%q) — the centre could not tell a host joined to the authored cluster from one joined to something else", got.ClusterNodeOf)
	}

	// Every field of MaintenanceSpec must be exercised above, or a future one is
	// added and silently dropped exactly as these were.
	check := func(name string, v reflect.Value) {
		t.Helper()
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.IsExported() && v.Field(i).IsZero() {
				t.Errorf("this test does not set %s.%s, so the round trip cannot prove the proto carries it — populate it", name, f.Name)
			}
		}
	}
	check("MaintenanceSpec", reflect.ValueOf(*h.Spec.Maintenance))

	// And the inventory the console reads a host's disks from. PoolName and
	// CannotPoolReason were added because the console could otherwise only infer
	// what claims a disk from CanPool — the sample above sets every field so a
	// later addition cannot ride the round trip as a silent zero value.
	check("PhysicalDisk", reflect.ValueOf(sampleHost().Status.Inventory.PhysicalDisks[0]))
	check("PhysicalAdapter", reflect.ValueOf(sampleHost().Status.Inventory.PhysicalAdapters[0]))
}

// A field the proto does not carry round-trips perfectly as its zero value, so
// the failure is silent: the centre stores the setting, the agent never receives
// it, and nothing anywhere reports a problem. CLAUDE.md requires the proto and
// both converters to change with the schema, covered by a round trip.
//
// These three decide whether storage redundancy is real, so a silent loss would
// be a fleet that believes it is multipathed and is not.
func TestVNICPurposeAndAffinityRoundTrip(t *testing.T) {
	rdma := true
	want := types.ManagementVNICSpec{
		Name:               "SMB01",
		SwitchName:         "ConvergedSwitch",
		VLANID:             40,
		MinBandwidthWeight: 50,
		Purpose:            types.VNICStorage,
		TeamMemberAdapter:  "Ethernet2",
		RDMA:               &rdma,
		IPConfig:           &types.IPConfig{Address: "10.0.40.11/24"},
	}
	got := mgmtVNICFromProto(mgmtVNICToProto(want))

	if got.Purpose != types.VNICStorage {
		t.Errorf("purpose lost: %q", got.Purpose)
	}
	if got.TeamMemberAdapter != "Ethernet2" {
		t.Errorf("the uplink this vNIC is pinned to lost: %q — without it two storage vNICs can share one port", got.TeamMemberAdapter)
	}
	if got.RDMA == nil || !*got.RDMA {
		t.Errorf("RDMA lost: %v", got.RDMA)
	}
	if got.Name != want.Name || got.VLANID != want.VLANID || got.IPConfig == nil || got.IPConfig.Address != want.IPConfig.Address {
		t.Errorf("the fields that already worked must keep working: %+v", got)
	}
}

// "Not declared" and "declared off" are different answers, and RDMA is the field
// where that matters: a fleet whose adapters cannot do it has decided, and one
// nobody has configured has not.
func TestVNICRDMADistinguishesUnsetFromOff(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name string
		in   *bool
	}{{"unset", nil}, {"off", &off}} {
		t.Run(tc.name, func(t *testing.T) {
			got := mgmtVNICFromProto(mgmtVNICToProto(types.ManagementVNICSpec{Name: "v", RDMA: tc.in}))
			if (got.RDMA == nil) != (tc.in == nil) {
				t.Fatalf("nil-ness must survive: sent %v, got %v", tc.in, got.RDMA)
			}
			if tc.in != nil && *got.RDMA != *tc.in {
				t.Fatalf("value lost: %v", *got.RDMA)
			}
		})
	}
}

// A management vNIC authored before any of this existed must be unchanged by it.
func TestVNICWithoutPurposeIsStillManagement(t *testing.T) {
	got := mgmtVNICFromProto(mgmtVNICToProto(types.ManagementVNICSpec{Name: "Mgmt", SwitchName: "sw"}))
	if got.Purpose != types.VNICManagement {
		t.Errorf("an undeclared purpose is management: %q", got.Purpose)
	}
	if got.Purpose.IsStorage() {
		t.Error("and must not be treated as storage")
	}
	if got.TeamMemberAdapter != "" || got.RDMA != nil {
		t.Errorf("nothing may be invented for it: %+v", got)
	}
}

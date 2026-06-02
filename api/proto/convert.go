package ballastpb

// This file is the single, deliberate mapping between the Go desired-state
// schema (api/types, the source of truth) and the gRPC wire types generated
// from ballast.proto. Keeping every field translation here means the wire form
// cannot silently diverge from the schema: if a field is added to api/types
// and to the proto, the compiler points here until the mapping is updated.
//
// Convention: <Type>ToProto converts schema -> wire; <type>FromProto converts
// wire -> schema. Nil-safe throughout.

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/joshua-fourie/ballast/api/types"
)

// ---------------------------------------------------------------------------
// Time helpers
// ---------------------------------------------------------------------------

func tsToProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsFromProto(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

func phaseToProto(p types.Phase) Phase {
	switch p {
	case types.PhasePending:
		return Phase_PHASE_PENDING
	case types.PhaseProgressing:
		return Phase_PHASE_PROGRESSING
	case types.PhaseReady:
		return Phase_PHASE_READY
	case types.PhaseDegraded:
		return Phase_PHASE_DEGRADED
	case types.PhaseError:
		return Phase_PHASE_ERROR
	default:
		return Phase_PHASE_UNSPECIFIED
	}
}

func phaseFromProto(p Phase) types.Phase {
	switch p {
	case Phase_PHASE_PENDING:
		return types.PhasePending
	case Phase_PHASE_PROGRESSING:
		return types.PhaseProgressing
	case Phase_PHASE_READY:
		return types.PhaseReady
	case Phase_PHASE_DEGRADED:
		return types.PhaseDegraded
	case Phase_PHASE_ERROR:
		return types.PhaseError
	default:
		return ""
	}
}

func rebootPolicyToProto(r types.RebootPolicy) RebootPolicy {
	switch r {
	case types.RebootNever:
		return RebootPolicy_REBOOT_POLICY_NEVER
	case types.RebootIfNeeded:
		return RebootPolicy_REBOOT_POLICY_IF_NEEDED
	default:
		return RebootPolicy_REBOOT_POLICY_UNSPECIFIED
	}
}

func rebootPolicyFromProto(r RebootPolicy) types.RebootPolicy {
	switch r {
	case RebootPolicy_REBOOT_POLICY_NEVER:
		return types.RebootNever
	case RebootPolicy_REBOOT_POLICY_IF_NEEDED:
		return types.RebootIfNeeded
	default:
		return ""
	}
}

func teamingModeToProto(m types.SETTeamingMode) SETTeamingMode {
	switch m {
	case types.SETSwitchIndependent:
		return SETTeamingMode_SET_TEAMING_MODE_SWITCH_INDEPENDENT
	default:
		return SETTeamingMode_SET_TEAMING_MODE_UNSPECIFIED
	}
}

func teamingModeFromProto(m SETTeamingMode) types.SETTeamingMode {
	switch m {
	case SETTeamingMode_SET_TEAMING_MODE_SWITCH_INDEPENDENT:
		return types.SETSwitchIndependent
	default:
		return ""
	}
}

func loadBalancingToProto(l types.SETLoadBalancing) SETLoadBalancing {
	switch l {
	case types.SETHyperVPort:
		return SETLoadBalancing_SET_LOAD_BALANCING_HYPERV_PORT
	case types.SETDynamic:
		return SETLoadBalancing_SET_LOAD_BALANCING_DYNAMIC
	default:
		return SETLoadBalancing_SET_LOAD_BALANCING_UNSPECIFIED
	}
}

func loadBalancingFromProto(l SETLoadBalancing) types.SETLoadBalancing {
	switch l {
	case SETLoadBalancing_SET_LOAD_BALANCING_HYPERV_PORT:
		return types.SETHyperVPort
	case SETLoadBalancing_SET_LOAD_BALANCING_DYNAMIC:
		return types.SETDynamic
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// ObjectMeta / Condition
// ---------------------------------------------------------------------------

func MetaToProto(m types.ObjectMeta) *ObjectMeta {
	return &ObjectMeta{
		Name:       m.Name,
		Uid:        m.UID,
		Generation: m.Generation,
		Labels:     m.Labels,
		CreatedAt:  tsToProto(m.CreatedAt),
		UpdatedAt:  tsToProto(m.UpdatedAt),
	}
}

func MetaFromProto(m *ObjectMeta) types.ObjectMeta {
	if m == nil {
		return types.ObjectMeta{}
	}
	return types.ObjectMeta{
		Name:       m.GetName(),
		UID:        m.GetUid(),
		Generation: m.GetGeneration(),
		Labels:     m.GetLabels(),
		CreatedAt:  tsFromProto(m.GetCreatedAt()),
		UpdatedAt:  tsFromProto(m.GetUpdatedAt()),
	}
}

func conditionsToProto(cs []types.Condition) []*Condition {
	if cs == nil {
		return nil
	}
	out := make([]*Condition, 0, len(cs))
	for _, c := range cs {
		out = append(out, &Condition{
			Type:               c.Type,
			Status:             c.Status,
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: tsToProto(c.LastTransitionTime),
		})
	}
	return out
}

func conditionsFromProto(cs []*Condition) []types.Condition {
	if cs == nil {
		return nil
	}
	out := make([]types.Condition, 0, len(cs))
	for _, c := range cs {
		out = append(out, types.Condition{
			Type:               c.GetType(),
			Status:             c.GetStatus(),
			Reason:             c.GetReason(),
			Message:            c.GetMessage(),
			LastTransitionTime: tsFromProto(c.GetLastTransitionTime()),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Inventory
// ---------------------------------------------------------------------------

func InventoryToProto(inv types.HostInventory) *HostInventory {
	out := &HostInventory{
		TotalMemoryBytes: inv.TotalMemoryBytes,
		LogicalCpus:      int32(inv.LogicalCPUs),
	}
	for _, a := range inv.PhysicalAdapters {
		out.PhysicalAdapters = append(out.PhysicalAdapters, &PhysicalAdapter{
			Name:         a.Name,
			Mac:          a.MAC,
			LinkSpeedBps: a.LinkSpeedBps,
			Up:           a.Up,
		})
	}
	for _, d := range inv.PhysicalDisks {
		out.PhysicalDisks = append(out.PhysicalDisks, &PhysicalDisk{
			DeviceId:  d.DeviceID,
			SizeBytes: d.SizeBytes,
			MediaType: d.MediaType,
			CanPool:   d.CanPool,
		})
	}
	return out
}

func InventoryFromProto(inv *HostInventory) types.HostInventory {
	if inv == nil {
		return types.HostInventory{}
	}
	out := types.HostInventory{
		TotalMemoryBytes: inv.GetTotalMemoryBytes(),
		LogicalCPUs:      int(inv.GetLogicalCpus()),
	}
	for _, a := range inv.GetPhysicalAdapters() {
		out.PhysicalAdapters = append(out.PhysicalAdapters, types.PhysicalAdapter{
			Name:         a.GetName(),
			MAC:          a.GetMac(),
			LinkSpeedBps: a.GetLinkSpeedBps(),
			Up:           a.GetUp(),
		})
	}
	for _, d := range inv.GetPhysicalDisks() {
		out.PhysicalDisks = append(out.PhysicalDisks, types.PhysicalDisk{
			DeviceID:  d.GetDeviceId(),
			SizeBytes: d.GetSizeBytes(),
			MediaType: d.GetMediaType(),
			CanPool:   d.GetCanPool(),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Networking
// ---------------------------------------------------------------------------

func networkingToProto(n types.HostNetworkingSpec) *HostNetworkingSpec {
	out := &HostNetworkingSpec{}
	for _, s := range n.Switches {
		out.Switches = append(out.Switches, &VirtualSwitchSpec{
			Name:              s.Name,
			TeamMembers:       s.TeamMembers,
			TeamingMode:       teamingModeToProto(s.TeamingMode),
			LoadBalancing:     loadBalancingToProto(s.LoadBalancing),
			AllowManagementOs: s.AllowManagementOS,
		})
	}
	for _, v := range n.ManagementVNICs {
		out.ManagementVnics = append(out.ManagementVnics, mgmtVNICToProto(v))
	}
	return out
}

func networkingFromProto(n *HostNetworkingSpec) types.HostNetworkingSpec {
	if n == nil {
		return types.HostNetworkingSpec{}
	}
	out := types.HostNetworkingSpec{}
	for _, s := range n.GetSwitches() {
		out.Switches = append(out.Switches, types.VirtualSwitchSpec{
			Name:              s.GetName(),
			TeamMembers:       s.GetTeamMembers(),
			TeamingMode:       teamingModeFromProto(s.GetTeamingMode()),
			LoadBalancing:     loadBalancingFromProto(s.GetLoadBalancing()),
			AllowManagementOS: s.GetAllowManagementOs(),
		})
	}
	for _, v := range n.GetManagementVnics() {
		out.ManagementVNICs = append(out.ManagementVNICs, mgmtVNICFromProto(v))
	}
	return out
}

func mgmtVNICToProto(v types.ManagementVNICSpec) *ManagementVNICSpec {
	out := &ManagementVNICSpec{
		Name:               v.Name,
		SwitchName:         v.SwitchName,
		VlanId:             int32(v.VLANID),
		MinBandwidthWeight: int32(v.MinBandwidthWeight),
	}
	if v.IPConfig != nil {
		out.IpConfig = &IPConfig{
			Address:    v.IPConfig.Address,
			Gateway:    v.IPConfig.Gateway,
			DnsServers: v.IPConfig.DNSServers,
		}
	}
	return out
}

func mgmtVNICFromProto(v *ManagementVNICSpec) types.ManagementVNICSpec {
	out := types.ManagementVNICSpec{
		Name:               v.GetName(),
		SwitchName:         v.GetSwitchName(),
		VLANID:             int(v.GetVlanId()),
		MinBandwidthWeight: int(v.GetMinBandwidthWeight()),
	}
	if ip := v.GetIpConfig(); ip != nil {
		out.IPConfig = &types.IPConfig{
			Address:    ip.GetAddress(),
			Gateway:    ip.GetGateway(),
			DNSServers: ip.GetDnsServers(),
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Storage / cluster membership
// ---------------------------------------------------------------------------

func storageToProto(s types.HostStorageSpec) *HostStorageSpec {
	return &HostStorageSpec{
		ContributeToS2D:      s.ContributeToS2D,
		EligibleDiskSelector: s.EligibleDiskSelector,
	}
}

func storageFromProto(s *HostStorageSpec) types.HostStorageSpec {
	if s == nil {
		return types.HostStorageSpec{}
	}
	return types.HostStorageSpec{
		ContributeToS2D:      s.GetContributeToS2D(),
		EligibleDiskSelector: s.GetEligibleDiskSelector(),
	}
}

// ---------------------------------------------------------------------------
// HostSpec / HostStatus / Host
// ---------------------------------------------------------------------------

func specToProto(s types.HostSpec) *HostSpec {
	out := &HostSpec{
		Fqdn:             s.FQDN,
		EnableHypervRole: s.EnableHyperVRole,
		Networking:       networkingToProto(s.Networking),
		Storage:          storageToProto(s.Storage),
		RebootPolicy:     rebootPolicyToProto(s.RebootPolicy),
	}
	if s.ClusterMembership != nil {
		out.ClusterMembership = &ClusterMembershipSpec{
			ClusterName: s.ClusterMembership.ClusterName,
		}
	}
	return out
}

func specFromProto(s *HostSpec) types.HostSpec {
	if s == nil {
		return types.HostSpec{}
	}
	out := types.HostSpec{
		FQDN:             s.GetFqdn(),
		EnableHyperVRole: s.GetEnableHypervRole(),
		Networking:       networkingFromProto(s.GetNetworking()),
		Storage:          storageFromProto(s.GetStorage()),
		RebootPolicy:     rebootPolicyFromProto(s.GetRebootPolicy()),
	}
	if cm := s.GetClusterMembership(); cm != nil {
		out.ClusterMembership = &types.ClusterMembershipSpec{
			ClusterName: cm.GetClusterName(),
		}
	}
	return out
}

// StatusToProto converts an agent-reported HostStatus to the wire form.
func StatusToProto(s types.HostStatus) *HostStatus {
	return &HostStatus{
		Phase:              phaseToProto(s.Phase),
		ObservedGeneration: s.ObservedGeneration,
		HypervInstalled:    s.HyperVInstalled,
		RebootRequired:     s.RebootRequired,
		Autonomous:         s.Autonomous,
		LastContact:        tsToProto(s.LastContact),
		Inventory:          InventoryToProto(s.Inventory),
		Conditions:         conditionsToProto(s.Conditions),
	}
}

// StatusFromProto converts a wire HostStatus back to the schema type.
func StatusFromProto(s *HostStatus) types.HostStatus {
	if s == nil {
		return types.HostStatus{}
	}
	return types.HostStatus{
		Phase:              phaseFromProto(s.GetPhase()),
		ObservedGeneration: s.GetObservedGeneration(),
		HyperVInstalled:    s.GetHypervInstalled(),
		RebootRequired:     s.GetRebootRequired(),
		Autonomous:         s.GetAutonomous(),
		LastContact:        tsFromProto(s.GetLastContact()),
		Inventory:          InventoryFromProto(s.GetInventory()),
		Conditions:         conditionsFromProto(s.GetConditions()),
	}
}

// HostToProto converts a schema Host (meta + spec + status) to the wire form.
func HostToProto(h types.Host) *Host {
	return &Host{
		Meta:   MetaToProto(h.Meta),
		Spec:   specToProto(h.Spec),
		Status: StatusToProto(h.Status),
	}
}

// HostFromProto converts a wire Host back to the schema type.
func HostFromProto(h *Host) types.Host {
	if h == nil {
		return types.Host{}
	}
	return types.Host{
		Meta:   MetaFromProto(h.GetMeta()),
		Spec:   specFromProto(h.GetSpec()),
		Status: StatusFromProto(h.GetStatus()),
	}
}

// ---------------------------------------------------------------------------
// Cluster
// ---------------------------------------------------------------------------
//
// The proto ClusterSpec omits Volumes (CSVSpec): Cluster Shared Volume
// provisioning is a later increment, so it is not yet carried on the wire.

func witnessTypeToProto(w types.WitnessType) WitnessType {
	switch w {
	case types.WitnessFileShare:
		return WitnessType_WITNESS_TYPE_FILE_SHARE
	case types.WitnessCloud:
		return WitnessType_WITNESS_TYPE_CLOUD
	case types.WitnessDisk:
		return WitnessType_WITNESS_TYPE_DISK
	default:
		return WitnessType_WITNESS_TYPE_NONE
	}
}

func witnessTypeFromProto(w WitnessType) types.WitnessType {
	switch w {
	case WitnessType_WITNESS_TYPE_FILE_SHARE:
		return types.WitnessFileShare
	case WitnessType_WITNESS_TYPE_CLOUD:
		return types.WitnessCloud
	case WitnessType_WITNESS_TYPE_DISK:
		return types.WitnessDisk
	default:
		return ""
	}
}

func clusterSpecToProto(s types.ClusterSpec) *ClusterSpec {
	return &ClusterSpec{
		Members:      s.Members,
		ManagementIp: s.ManagementIP,
		EnableS2D:    s.EnableS2D,
		Witness: &WitnessSpec{
			Type:          witnessTypeToProto(s.Witness.Type),
			FileSharePath: s.Witness.FileSharePath,
			CloudAccount:  s.Witness.CloudAccount,
		},
	}
}

func clusterSpecFromProto(s *ClusterSpec) types.ClusterSpec {
	if s == nil {
		return types.ClusterSpec{}
	}
	out := types.ClusterSpec{
		Members:      s.GetMembers(),
		ManagementIP: s.GetManagementIp(),
		EnableS2D:    s.GetEnableS2D(),
	}
	if w := s.GetWitness(); w != nil {
		out.Witness = types.WitnessSpec{
			Type:          witnessTypeFromProto(w.GetType()),
			FileSharePath: w.GetFileSharePath(),
			CloudAccount:  w.GetCloudAccount(),
		}
	}
	return out
}

// ClusterStatusToProto converts an agent-reported ClusterStatus to the wire form.
func ClusterStatusToProto(s types.ClusterStatus) *ClusterStatus {
	return &ClusterStatus{
		Phase:              phaseToProto(s.Phase),
		ObservedGeneration: s.ObservedGeneration,
		FormedMembers:      s.FormedMembers,
		S2DEnabled:         s.S2DEnabled,
		Conditions:         conditionsToProto(s.Conditions),
	}
}

// ClusterStatusFromProto converts a wire ClusterStatus back to the schema type.
func ClusterStatusFromProto(s *ClusterStatus) types.ClusterStatus {
	if s == nil {
		return types.ClusterStatus{}
	}
	return types.ClusterStatus{
		Phase:              phaseFromProto(s.GetPhase()),
		ObservedGeneration: s.GetObservedGeneration(),
		FormedMembers:      s.GetFormedMembers(),
		S2DEnabled:         s.GetS2DEnabled(),
		Conditions:         conditionsFromProto(s.GetConditions()),
	}
}

// ClusterToProto converts a schema Cluster to the wire form.
func ClusterToProto(c types.Cluster) *Cluster {
	return &Cluster{
		Meta:   MetaToProto(c.Meta),
		Spec:   clusterSpecToProto(c.Spec),
		Status: ClusterStatusToProto(c.Status),
	}
}

// ClusterFromProto converts a wire Cluster back to the schema type.
func ClusterFromProto(c *Cluster) types.Cluster {
	if c == nil {
		return types.Cluster{}
	}
	return types.Cluster{
		Meta:   MetaFromProto(c.GetMeta()),
		Spec:   clusterSpecFromProto(c.GetSpec()),
		Status: ClusterStatusFromProto(c.GetStatus()),
	}
}

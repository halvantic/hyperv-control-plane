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
	"encoding/json"
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
		OsVersion:        inv.OSVersion,
	}
	for _, a := range inv.PhysicalAdapters {
		out.PhysicalAdapters = append(out.PhysicalAdapters, &PhysicalAdapter{
			Name:         a.Name,
			Mac:          a.MAC,
			LinkSpeedBps: a.LinkSpeedBps,
			Up:           a.Up,
			IsManagement: a.IsManagement,
			Ipv4:         a.IPv4,
			DnsServers:   a.DNSServers,
			RegistersDns: a.RegistersDNS,
			Gateway:      a.Gateway,
			PrefixLength: int32(a.PrefixLength),
		})
	}
	for _, d := range inv.PhysicalDisks {
		out.PhysicalDisks = append(out.PhysicalDisks, &PhysicalDisk{
			DeviceId:    d.DeviceID,
			SizeBytes:   d.SizeBytes,
			MediaType:   d.MediaType,
			CanPool:     d.CanPool,
			IsOsDisk:    d.IsOSDisk,
			DriveLetter: d.DriveLetter,
		})
	}
	out.UsedDriveLetters = inv.UsedDriveLetters
	return out
}

func InventoryFromProto(inv *HostInventory) types.HostInventory {
	if inv == nil {
		return types.HostInventory{}
	}
	out := types.HostInventory{
		TotalMemoryBytes: inv.GetTotalMemoryBytes(),
		LogicalCPUs:      int(inv.GetLogicalCpus()),
		OSVersion:        inv.GetOsVersion(),
	}
	for _, a := range inv.GetPhysicalAdapters() {
		out.PhysicalAdapters = append(out.PhysicalAdapters, types.PhysicalAdapter{
			Name:         a.GetName(),
			MAC:          a.GetMac(),
			LinkSpeedBps: a.GetLinkSpeedBps(),
			Up:           a.GetUp(),
			IsManagement: a.GetIsManagement(),
			IPv4:         a.GetIpv4(),
			DNSServers:   a.GetDnsServers(),
			RegistersDNS: a.GetRegistersDns(),
			Gateway:      a.GetGateway(),
			PrefixLength: int(a.GetPrefixLength()),
		})
	}
	for _, d := range inv.GetPhysicalDisks() {
		out.PhysicalDisks = append(out.PhysicalDisks, types.PhysicalDisk{
			DeviceID:    d.GetDeviceId(),
			SizeBytes:   d.GetSizeBytes(),
			MediaType:   d.GetMediaType(),
			CanPool:     d.GetCanPool(),
			IsOSDisk:    d.GetIsOsDisk(),
			DriveLetter: d.GetDriveLetter(),
		})
	}
	out.UsedDriveLetters = inv.GetUsedDriveLetters()
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
	out.DnsServers = n.DNSServers
	for _, c := range n.NICConfigs {
		out.NicConfigs = append(out.NicConfigs, &PhysicalNICConfig{
			AdapterName: c.AdapterName,
			IpConfig:    &IPConfig{Address: c.IPConfig.Address, Gateway: c.IPConfig.Gateway, DnsServers: c.IPConfig.DNSServers},
		})
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
	out.DNSServers = n.GetDnsServers()
	for _, c := range n.GetNicConfigs() {
		out.NICConfigs = append(out.NICConfigs, types.PhysicalNICConfig{
			AdapterName: c.GetAdapterName(),
			IPConfig:    types.IPConfig{Address: c.GetIpConfig().GetAddress(), Gateway: c.GetIpConfig().GetGateway(), DNSServers: c.GetIpConfig().GetDnsServers()},
		})
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
		DefaultVmPath:        s.DefaultVMPath,
		DefaultVhdPath:       s.DefaultVHDPath,
	}
}

func storageFromProto(s *HostStorageSpec) types.HostStorageSpec {
	if s == nil {
		return types.HostStorageSpec{}
	}
	return types.HostStorageSpec{
		ContributeToS2D:      s.GetContributeToS2D(),
		EligibleDiskSelector: s.GetEligibleDiskSelector(),
		DefaultVMPath:        s.GetDefaultVmPath(),
		DefaultVHDPath:       s.GetDefaultVhdPath(),
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
		ComputerName:     s.ComputerName,
	}
	if s.ClusterMembership != nil {
		out.ClusterMembership = &ClusterMembershipSpec{
			ClusterName: s.ClusterMembership.ClusterName,
		}
	}
	if n := s.ManagementNIC; n != nil {
		out.ManagementNic = &PhysicalNICConfig{
			AdapterName: n.AdapterName,
			IpConfig:    &IPConfig{Address: n.IPConfig.Address, Gateway: n.IPConfig.Gateway, DnsServers: n.IPConfig.DNSServers},
		}
	}
	if d := s.DomainJoin; d != nil {
		out.DomainJoin = &DomainJoinSpec{DomainName: d.DomainName, OuPath: d.OUPath, CredentialSecret: d.CredentialSecret}
	}
	if m := s.LiveMigration; m != nil {
		out.LiveMigration = &LiveMigrationSpec{
			Enabled:            m.Enabled,
			AuthenticationType: m.AuthenticationType,
			MaxConcurrent:      int32(m.MaxConcurrent),
			Networks:           m.Networks,
		}
	}
	if r := s.ReplicaServer; r != nil {
		out.ReplicaServer = &ReplicaServerSpec{
			Enabled:                r.Enabled,
			AuthenticationType:     r.AuthenticationType,
			Port:                   int32(r.Port),
			DefaultStorageLocation: r.DefaultStorageLocation,
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
		ComputerName:     s.GetComputerName(),
	}
	if cm := s.GetClusterMembership(); cm != nil {
		out.ClusterMembership = &types.ClusterMembershipSpec{
			ClusterName: cm.GetClusterName(),
		}
	}
	if n := s.GetManagementNic(); n != nil {
		ip := n.GetIpConfig()
		out.ManagementNIC = &types.PhysicalNICConfig{
			AdapterName: n.GetAdapterName(),
			IPConfig:    types.IPConfig{Address: ip.GetAddress(), Gateway: ip.GetGateway(), DNSServers: ip.GetDnsServers()},
		}
	}
	if d := s.GetDomainJoin(); d != nil {
		out.DomainJoin = &types.DomainJoinSpec{DomainName: d.GetDomainName(), OUPath: d.GetOuPath(), CredentialSecret: d.GetCredentialSecret()}
	}
	if m := s.GetLiveMigration(); m != nil {
		out.LiveMigration = &types.LiveMigrationSpec{
			Enabled:            m.GetEnabled(),
			AuthenticationType: m.GetAuthenticationType(),
			MaxConcurrent:      int(m.GetMaxConcurrent()),
			Networks:           m.GetNetworks(),
		}
	}
	if r := s.GetReplicaServer(); r != nil {
		out.ReplicaServer = &types.ReplicaServerSpec{
			Enabled:                r.GetEnabled(),
			AuthenticationType:     r.GetAuthenticationType(),
			Port:                   int(r.GetPort()),
			DefaultStorageLocation: r.GetDefaultStorageLocation(),
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
		ComputerName:       s.ComputerName,
		Domain:             s.Domain,
		AgentVersion:       s.AgentVersion,
		NetworkProfile:     s.NetworkProfile,
		RebootRequired:     s.RebootRequired,
		Autonomous:         s.Autonomous,
		LastContact:        tsToProto(s.LastContact),
		Inventory:          InventoryToProto(s.Inventory),
		Conditions:         conditionsToProto(s.Conditions),
		Metrics: &HostMetrics{
			CpuUsagePercent:  int32(s.Metrics.CPUUsagePercent),
			MemoryInUseBytes: s.Metrics.MemoryInUseBytes,
			UptimeSeconds:    s.Metrics.UptimeSeconds,
		},
		Resources:       resourcesToProto(s.Resources),
		ObservedVmsJson: observedVMsToJSON(s.ObservedVMs),
	}
}

func observedVMsToJSON(vms []types.ObservedVM) []string {
	if len(vms) == 0 {
		return nil
	}
	out := make([]string, 0, len(vms))
	for _, v := range vms {
		if b, err := json.Marshal(v); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

func observedVMsFromJSON(in []string) []types.ObservedVM {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.ObservedVM, 0, len(in))
	for _, s := range in {
		var v types.ObservedVM
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func resourcesToProto(r types.HostResources) *HostResources {
	out := &HostResources{Switches: r.Switches, Isos: r.ISOs}
	for _, v := range r.Volumes {
		out.Volumes = append(out.Volumes, &StorageVolume{Name: v.Name, Path: v.Path, SizeBytes: v.SizeBytes, UsedBytes: v.UsedBytes})
	}
	for _, s := range r.SwitchDetails {
		out.SwitchDetails = append(out.SwitchDetails, &VirtualSwitchInfo{
			Name: s.Name, NetAdapters: s.NetAdapters, AllowManagementOs: s.AllowManagementOS, VlanId: int32(s.VLANID),
		})
	}
	for _, v := range r.ManagementVNICs {
		mv := &ManagementVNICInfo{Name: v.Name, SwitchName: v.SwitchName, VlanId: int32(v.VlanID), DnsServers: v.DNSServers, Profile: v.Profile}
		for _, a := range v.Addresses {
			mv.Addresses = append(mv.Addresses, &VNICAddress{Address: a.Address, Kind: a.Kind})
		}
		out.ManagementVnics = append(out.ManagementVnics, mv)
	}
	return out
}

func resourcesFromProto(r *HostResources) types.HostResources {
	if r == nil {
		return types.HostResources{}
	}
	out := types.HostResources{Switches: r.GetSwitches(), ISOs: r.GetIsos()}
	for _, v := range r.GetVolumes() {
		out.Volumes = append(out.Volumes, types.StorageVolume{Name: v.GetName(), Path: v.GetPath(), SizeBytes: v.GetSizeBytes(), UsedBytes: v.GetUsedBytes()})
	}
	for _, s := range r.GetSwitchDetails() {
		out.SwitchDetails = append(out.SwitchDetails, types.VirtualSwitchInfo{
			Name: s.GetName(), NetAdapters: s.GetNetAdapters(), AllowManagementOS: s.GetAllowManagementOs(), VLANID: int(s.GetVlanId()),
		})
	}
	for _, v := range r.GetManagementVnics() {
		mv := types.ManagementVNICInfo{Name: v.GetName(), SwitchName: v.GetSwitchName(), VlanID: int(v.GetVlanId()), DNSServers: v.GetDnsServers(), Profile: v.GetProfile()}
		for _, a := range v.GetAddresses() {
			mv.Addresses = append(mv.Addresses, types.VNICAddress{Address: a.GetAddress(), Kind: a.GetKind()})
		}
		out.ManagementVNICs = append(out.ManagementVNICs, mv)
	}
	return out
}

// StatusFromProto converts a wire HostStatus back to the schema type.
func StatusFromProto(s *HostStatus) types.HostStatus {
	if s == nil {
		return types.HostStatus{}
	}
	out := types.HostStatus{
		Phase:              phaseFromProto(s.GetPhase()),
		ObservedGeneration: s.GetObservedGeneration(),
		HyperVInstalled:    s.GetHypervInstalled(),
		ComputerName:       s.GetComputerName(),
		Domain:             s.GetDomain(),
		AgentVersion:       s.GetAgentVersion(),
		NetworkProfile:     s.GetNetworkProfile(),
		RebootRequired:     s.GetRebootRequired(),
		Autonomous:         s.GetAutonomous(),
		LastContact:        tsFromProto(s.GetLastContact()),
		Inventory:          InventoryFromProto(s.GetInventory()),
		Conditions:         conditionsFromProto(s.GetConditions()),
	}
	if m := s.GetMetrics(); m != nil {
		out.Metrics = types.HostMetrics{
			CPUUsagePercent:  int(m.GetCpuUsagePercent()),
			MemoryInUseBytes: m.GetMemoryInUseBytes(),
			UptimeSeconds:    m.GetUptimeSeconds(),
		}
	}
	out.Resources = resourcesFromProto(s.GetResources())
	out.ObservedVMs = observedVMsFromJSON(s.GetObservedVmsJson())
	return out
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
// ClusterSpec carries members, witness, S2D enable, CSV volumes and live
// migration on the wire — the former needs them to act. Cluster switches and
// the default storage path are deliberately not on the wire: the centre fans
// them into each member's HostSpec, so they reach agents through the host spec.

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
	out := &ClusterSpec{
		Members:      s.Members,
		ManagementIp: s.ManagementIP,
		EnableS2D:    s.EnableS2D,
		Witness: &WitnessSpec{
			Type:          witnessTypeToProto(s.Witness.Type),
			FileSharePath: s.Witness.FileSharePath,
			CloudAccount:  s.Witness.CloudAccount,
		},
	}
	for _, v := range s.Volumes {
		out.Volumes = append(out.Volumes, &CSVSpec{
			Name:           v.Name,
			SizeBytes:      v.SizeBytes,
			ResiliencyType: v.ResiliencyType,
			NumberOfCopies: int32(v.NumberOfCopies),
		})
	}
	if m := s.LiveMigration; m != nil {
		out.LiveMigration = &LiveMigrationSpec{
			Enabled:            m.Enabled,
			AuthenticationType: m.AuthenticationType,
			MaxConcurrent:      int32(m.MaxConcurrent),
			Networks:           m.Networks,
		}
	}
	if b := s.ReplicaBroker; b != nil {
		out.ReplicaBroker = &ReplicaBrokerSpec{Name: b.Name, StaticIp: b.StaticIP, StoragePath: b.StoragePath}
	}
	return out
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
	for _, v := range s.GetVolumes() {
		out.Volumes = append(out.Volumes, types.CSVSpec{
			Name:           v.GetName(),
			SizeBytes:      v.GetSizeBytes(),
			ResiliencyType: v.GetResiliencyType(),
			NumberOfCopies: int(v.GetNumberOfCopies()),
		})
	}
	if m := s.GetLiveMigration(); m != nil {
		out.LiveMigration = &types.LiveMigrationSpec{
			Enabled:            m.GetEnabled(),
			AuthenticationType: m.GetAuthenticationType(),
			MaxConcurrent:      int(m.GetMaxConcurrent()),
			Networks:           m.GetNetworks(),
		}
	}
	if b := s.GetReplicaBroker(); b != nil {
		out.ReplicaBroker = &types.ReplicaBrokerSpec{Name: b.GetName(), StaticIP: b.GetStaticIp(), StoragePath: b.GetStoragePath()}
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
		Groups:             clusterGroupsToProto(s.Groups),
		Csvs:               clusterCSVsToProto(s.CSVs),
		ClusterVms:         clusterVMsToProto(s.VMs),
		Nodes:              clusterNodesToProto(s.Nodes),
		Pool:               clusterPoolToProto(s.Pool),
		Networks:           clusterNetworksToProto(s.Networks),
	}
}

func clusterNetworksToProto(ns []types.ClusterNetworkStatus) []*ClusterNetwork {
	out := make([]*ClusterNetwork, 0, len(ns))
	for _, n := range ns {
		out = append(out, &ClusterNetwork{Name: n.Name, Cidr: n.CIDR, Role: n.Role, State: n.State})
	}
	return out
}

func clusterNetworksFromProto(ns []*ClusterNetwork) []types.ClusterNetworkStatus {
	out := make([]types.ClusterNetworkStatus, 0, len(ns))
	for _, n := range ns {
		out = append(out, types.ClusterNetworkStatus{Name: n.GetName(), CIDR: n.GetCidr(), Role: n.GetRole(), State: n.GetState()})
	}
	return out
}

func clusterPoolToProto(p *types.ClusterPoolStatus) *ClusterPool {
	if p == nil {
		return nil
	}
	return &ClusterPool{Name: p.Name, RawBytes: p.RawBytes, AllocatedBytes: p.AllocatedBytes,
		Health: p.Health, Operational: p.Operational, UnhealthyDisks: int32(p.UnhealthyDisks), TotalDisks: int32(p.TotalDisks)}
}

func clusterPoolFromProto(p *ClusterPool) *types.ClusterPoolStatus {
	if p == nil {
		return nil
	}
	return &types.ClusterPoolStatus{Name: p.GetName(), RawBytes: p.GetRawBytes(), AllocatedBytes: p.GetAllocatedBytes(),
		Health: p.GetHealth(), Operational: p.GetOperational(), UnhealthyDisks: int(p.GetUnhealthyDisks()), TotalDisks: int(p.GetTotalDisks())}
}

func clusterNodesToProto(ns []types.ClusterNodeStatus) []*ClusterNode {
	out := make([]*ClusterNode, 0, len(ns))
	for _, n := range ns {
		out = append(out, &ClusterNode{Name: n.Name, State: n.State})
	}
	return out
}

func clusterNodesFromProto(ns []*ClusterNode) []types.ClusterNodeStatus {
	out := make([]types.ClusterNodeStatus, 0, len(ns))
	for _, n := range ns {
		out = append(out, types.ClusterNodeStatus{Name: n.GetName(), State: n.GetState()})
	}
	return out
}

func clusterVMsToProto(vs []types.ClusterVMStatus) []*ClusterVM {
	out := make([]*ClusterVM, 0, len(vs))
	for _, v := range vs {
		out = append(out, &ClusterVM{Name: v.Name, OwnerNode: v.OwnerNode, State: v.State})
	}
	return out
}

func clusterVMsFromProto(vs []*ClusterVM) []types.ClusterVMStatus {
	out := make([]types.ClusterVMStatus, 0, len(vs))
	for _, v := range vs {
		out = append(out, types.ClusterVMStatus{Name: v.GetName(), OwnerNode: v.GetOwnerNode(), State: v.GetState()})
	}
	return out
}

func clusterGroupsToProto(gs []types.ClusterGroupStatus) []*ClusterGroup {
	out := make([]*ClusterGroup, 0, len(gs))
	for _, g := range gs {
		out = append(out, &ClusterGroup{Name: g.Name, OwnerNode: g.OwnerNode, State: g.State, GroupType: g.GroupType})
	}
	return out
}

func clusterCSVsToProto(vs []types.CSVStatus) []*ClusterCSV {
	out := make([]*ClusterCSV, 0, len(vs))
	for _, v := range vs {
		out = append(out, &ClusterCSV{Name: v.Name, OwnerNode: v.OwnerNode, State: v.State})
	}
	return out
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
		Groups:             clusterGroupsFromProto(s.GetGroups()),
		CSVs:               clusterCSVsFromProto(s.GetCsvs()),
		VMs:                clusterVMsFromProto(s.GetClusterVms()),
		Nodes:              clusterNodesFromProto(s.GetNodes()),
		Pool:               clusterPoolFromProto(s.GetPool()),
		Networks:           clusterNetworksFromProto(s.GetNetworks()),
	}
}

func clusterGroupsFromProto(gs []*ClusterGroup) []types.ClusterGroupStatus {
	out := make([]types.ClusterGroupStatus, 0, len(gs))
	for _, g := range gs {
		out = append(out, types.ClusterGroupStatus{Name: g.GetName(), OwnerNode: g.GetOwnerNode(), State: g.GetState(), GroupType: g.GetGroupType()})
	}
	return out
}

func clusterCSVsFromProto(vs []*ClusterCSV) []types.CSVStatus {
	out := make([]types.CSVStatus, 0, len(vs))
	for _, v := range vs {
		out = append(out, types.CSVStatus{Name: v.GetName(), OwnerNode: v.GetOwnerNode(), State: v.GetState()})
	}
	return out
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

// ---------------------------------------------------------------------------
// Virtual machine
// ---------------------------------------------------------------------------

func vmPowerStateToProto(p types.VMPowerState) VMPowerState {
	switch p {
	case types.VMPowerRunning:
		return VMPowerState_VM_POWER_STATE_RUNNING
	case types.VMPowerOff:
		return VMPowerState_VM_POWER_STATE_OFF
	case types.VMPowerPaused:
		return VMPowerState_VM_POWER_STATE_PAUSED
	case types.VMPowerSaved:
		return VMPowerState_VM_POWER_STATE_SAVED
	default:
		return VMPowerState_VM_POWER_STATE_UNSPECIFIED
	}
}

func vmPowerStateFromProto(p VMPowerState) types.VMPowerState {
	switch p {
	case VMPowerState_VM_POWER_STATE_RUNNING:
		return types.VMPowerRunning
	case VMPowerState_VM_POWER_STATE_OFF:
		return types.VMPowerOff
	case VMPowerState_VM_POWER_STATE_PAUSED:
		return types.VMPowerPaused
	case VMPowerState_VM_POWER_STATE_SAVED:
		return types.VMPowerSaved
	default:
		return ""
	}
}

func vmStartActionToProto(a types.VMStartAction) VMStartAction {
	switch a {
	case types.VMStartNothing:
		return VMStartAction_VM_START_ACTION_NOTHING
	case types.VMStartIfWasRunning:
		return VMStartAction_VM_START_ACTION_START_IF_RUNNING
	case types.VMStartAlways:
		return VMStartAction_VM_START_ACTION_START
	default:
		return VMStartAction_VM_START_ACTION_UNSPECIFIED
	}
}

func vmStartActionFromProto(a VMStartAction) types.VMStartAction {
	switch a {
	case VMStartAction_VM_START_ACTION_NOTHING:
		return types.VMStartNothing
	case VMStartAction_VM_START_ACTION_START_IF_RUNNING:
		return types.VMStartIfWasRunning
	case VMStartAction_VM_START_ACTION_START:
		return types.VMStartAlways
	default:
		return ""
	}
}

func vmSpecToProto(s types.VMSpec) *VMSpec {
	out := &VMSpec{
		Placement:            &VMPlacementSpec{HostName: s.Placement.HostName, ClusterName: s.Placement.ClusterName},
		HypervGeneration:     int32(s.HyperVGeneration),
		ProcessorCount:       int32(s.ProcessorCount),
		MemoryStartupBytes:   s.MemoryStartupBytes,
		DesiredPowerState:    vmPowerStateToProto(s.DesiredPowerState),
		AutomaticStartAction: vmStartActionToProto(s.AutomaticStartAction),
		IsoPath:              s.ISOPath,
		SecureBoot:           s.SecureBoot,
		BootOrder:            append([]string(nil), s.BootOrder...),
	}
	if s.DynamicMemory != nil {
		out.DynamicMemory = &DynamicMemorySpec{
			MinBytes: s.DynamicMemory.MinBytes,
			MaxBytes: s.DynamicMemory.MaxBytes,
		}
	}
	for _, d := range s.Disks {
		out.Disks = append(out.Disks, &VMDiskSpec{
			Path:      d.Path,
			SizeBytes: d.SizeBytes,
			Dynamic:   d.Dynamic,
		})
	}
	for _, a := range s.NetworkAdapters {
		out.NetworkAdapters = append(out.NetworkAdapters, &VMNetworkAdapterSpec{
			Name:       a.Name,
			SwitchName: a.SwitchName,
			VlanId:     int32(a.VLANID),
			MacAddress: a.MACAddress,
		})
	}
	if r := s.Replication; r != nil {
		out.Replication = &VMReplicationSpec{
			Enabled:            r.Enabled,
			TargetHost:         r.TargetHost,
			TargetCluster:      r.TargetCluster,
			ReplicaServer:      r.ReplicaServer,
			FrequencySeconds:   int32(r.FrequencySeconds),
			AuthenticationType: r.AuthenticationType,
			Port:               int32(r.Port),
		}
	}
	return out
}

func vmSpecFromProto(s *VMSpec) types.VMSpec {
	if s == nil {
		return types.VMSpec{}
	}
	out := types.VMSpec{
		Placement:            types.VMPlacementSpec{HostName: s.GetPlacement().GetHostName(), ClusterName: s.GetPlacement().GetClusterName()},
		HyperVGeneration:     int(s.GetHypervGeneration()),
		ProcessorCount:       int(s.GetProcessorCount()),
		MemoryStartupBytes:   s.GetMemoryStartupBytes(),
		DesiredPowerState:    vmPowerStateFromProto(s.GetDesiredPowerState()),
		AutomaticStartAction: vmStartActionFromProto(s.GetAutomaticStartAction()),
		ISOPath:              s.GetIsoPath(),
		SecureBoot:           s.GetSecureBoot(),
		BootOrder:            append([]string(nil), s.GetBootOrder()...),
	}
	if dm := s.GetDynamicMemory(); dm != nil {
		out.DynamicMemory = &types.DynamicMemorySpec{
			MinBytes: dm.GetMinBytes(),
			MaxBytes: dm.GetMaxBytes(),
		}
	}
	for _, d := range s.GetDisks() {
		out.Disks = append(out.Disks, types.VMDiskSpec{
			Path:      d.GetPath(),
			SizeBytes: d.GetSizeBytes(),
			Dynamic:   d.GetDynamic(),
		})
	}
	for _, a := range s.GetNetworkAdapters() {
		out.NetworkAdapters = append(out.NetworkAdapters, types.VMNetworkAdapterSpec{
			Name:       a.GetName(),
			SwitchName: a.GetSwitchName(),
			VLANID:     int(a.GetVlanId()),
			MACAddress: a.GetMacAddress(),
		})
	}
	if r := s.GetReplication(); r != nil {
		out.Replication = &types.VMReplicationSpec{
			Enabled:            r.GetEnabled(),
			TargetHost:         r.GetTargetHost(),
			TargetCluster:      r.GetTargetCluster(),
			ReplicaServer:      r.GetReplicaServer(),
			FrequencySeconds:   int(r.GetFrequencySeconds()),
			AuthenticationType: r.GetAuthenticationType(),
			Port:               int(r.GetPort()),
		}
	}
	return out
}

// VMStatusToProto converts an agent-reported VMStatus to the wire form.
func VMStatusToProto(s types.VMStatus) *VMStatus {
	return &VMStatus{
		Phase:               phaseToProto(s.Phase),
		ObservedGeneration:  s.ObservedGeneration,
		PowerState:          vmPowerStateToProto(s.PowerState),
		AssignedMemoryBytes: s.AssignedMemoryBytes,
		CpuUsagePercent:     int32(s.CPUUsagePercent),
		UptimeSeconds:       s.UptimeSeconds,
		Conditions:          conditionsToProto(s.Conditions),
		ScreenPng:           s.ScreenPNG,
		VmId:                s.VMID,
		GuestOs:             s.GuestOS,
		IpAddress:           s.IPAddress,
		GuestFqdn:           s.GuestFQDN,
		Checkpoints:         checkpointsToProto(s.Checkpoints),
		ObservedJson:        marshalObserved(s.Observed),
		Replication:         vmReplicationStatusToProto(s.Replication),
	}
}

func vmReplicationStatusToProto(r *types.VMReplicationStatus) *VMReplicationStatus {
	if r == nil {
		return nil
	}
	return &VMReplicationStatus{
		Mode:                r.Mode,
		State:               r.State,
		Health:              r.Health,
		PrimaryServer:       r.PrimaryServer,
		ReplicaServer:       r.ReplicaServer,
		LastReplicationTime: r.LastReplicationTime,
		FrequencySeconds:    int32(r.FrequencySeconds),
	}
}

func vmReplicationStatusFromProto(r *VMReplicationStatus) *types.VMReplicationStatus {
	if r == nil {
		return nil
	}
	return &types.VMReplicationStatus{
		Mode:                r.GetMode(),
		State:               r.GetState(),
		Health:              r.GetHealth(),
		PrimaryServer:       r.GetPrimaryServer(),
		ReplicaServer:       r.GetReplicaServer(),
		LastReplicationTime: r.GetLastReplicationTime(),
		FrequencySeconds:    int(r.GetFrequencySeconds()),
	}
}

func marshalObserved(o *types.VMObserved) string {
	if o == nil {
		return ""
	}
	b, err := json.Marshal(o)
	if err != nil {
		return ""
	}
	return string(b)
}

func unmarshalObserved(s string) *types.VMObserved {
	if s == "" {
		return nil
	}
	var o types.VMObserved
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return nil
	}
	return &o
}

func checkpointsToProto(cs []types.VMCheckpoint) []*VMCheckpoint {
	if len(cs) == 0 {
		return nil
	}
	out := make([]*VMCheckpoint, 0, len(cs))
	for _, c := range cs {
		created := ""
		if !c.CreatedAt.IsZero() {
			created = c.CreatedAt.Format(time.RFC3339)
		}
		out = append(out, &VMCheckpoint{Name: c.Name, ParentName: c.ParentName, Type: c.Type, CreatedAt: created, IsCurrent: c.IsCurrent})
	}
	return out
}

func checkpointsFromProto(cs []*VMCheckpoint) []types.VMCheckpoint {
	if len(cs) == 0 {
		return nil
	}
	out := make([]types.VMCheckpoint, 0, len(cs))
	for _, c := range cs {
		var created time.Time
		if t, err := time.Parse(time.RFC3339, c.GetCreatedAt()); err == nil {
			created = t
		}
		out = append(out, types.VMCheckpoint{Name: c.GetName(), ParentName: c.GetParentName(), Type: c.GetType(), CreatedAt: created, IsCurrent: c.GetIsCurrent()})
	}
	return out
}

// VMStatusFromProto converts a wire VMStatus back to the schema type.
func VMStatusFromProto(s *VMStatus) types.VMStatus {
	if s == nil {
		return types.VMStatus{}
	}
	return types.VMStatus{
		Phase:               phaseFromProto(s.GetPhase()),
		ObservedGeneration:  s.GetObservedGeneration(),
		PowerState:          vmPowerStateFromProto(s.GetPowerState()),
		AssignedMemoryBytes: s.GetAssignedMemoryBytes(),
		CPUUsagePercent:     int(s.GetCpuUsagePercent()),
		UptimeSeconds:       s.GetUptimeSeconds(),
		Conditions:          conditionsFromProto(s.GetConditions()),
		ScreenPNG:           s.GetScreenPng(),
		VMID:                s.GetVmId(),
		GuestOS:             s.GetGuestOs(),
		IPAddress:           s.GetIpAddress(),
		GuestFQDN:           s.GetGuestFqdn(),
		Checkpoints:         checkpointsFromProto(s.GetCheckpoints()),
		Observed:            unmarshalObserved(s.GetObservedJson()),
		Replication:         vmReplicationStatusFromProto(s.GetReplication()),
	}
}

// JobToProto converts a schema Job to the wire form.
func JobToProto(j types.Job) *Job {
	return &Job{
		Id: j.ID, HostName: j.HostName, Kind: j.Kind, Params: j.Params,
		State: string(j.State), Message: j.Message,
	}
}

// JobFromProto converts a wire Job back to the schema type.
func JobFromProto(j *Job) types.Job {
	if j == nil {
		return types.Job{}
	}
	return types.Job{
		ID: j.GetId(), HostName: j.GetHostName(), Kind: j.GetKind(), Params: j.GetParams(),
		State: types.JobState(j.GetState()), Message: j.GetMessage(),
	}
}

// SecretToProto converts a schema Secret to the wire form (with its values).
func SecretToProto(s types.Secret) *Secret {
	return &Secret{Name: s.Name, Type: s.Type, Data: s.Data}
}

// SecretFromProto converts a wire Secret back to the schema type.
func SecretFromProto(s *Secret) types.Secret {
	if s == nil {
		return types.Secret{}
	}
	return types.Secret{Name: s.GetName(), Type: s.GetType(), Data: s.GetData()}
}

// VMToProto converts a schema VM to the wire form.
func VMToProto(v types.VM) *VM {
	return &VM{
		Meta:   MetaToProto(v.Meta),
		Spec:   vmSpecToProto(v.Spec),
		Status: VMStatusToProto(v.Status),
	}
}

// VMFromProto converts a wire VM back to the schema type.
func VMFromProto(v *VM) types.VM {
	if v == nil {
		return types.VM{}
	}
	return types.VM{
		Meta:   MetaFromProto(v.GetMeta()),
		Spec:   vmSpecFromProto(v.GetSpec()),
		Status: VMStatusFromProto(v.GetStatus()),
	}
}

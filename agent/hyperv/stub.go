package hyperv

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/joshua-fourie/ballast/api/types"
)

// Stub is an in-memory implementation of Interface for developing and testing
// the agent off a real Hyper-V host. It models switch and vNIC state so the
// ensure-operations behave like the real ones: idempotent, returning
// OutcomeUnchanged on a second identical apply. It never touches the host.
//
// It is compiled into every build but only selected when the real
// implementation is unavailable or explicitly overridden, so the rest of the
// agent runs unchanged on a developer machine.
type Stub struct {
	// Inventory, if set, overrides the default fixture. Lets tests drive the
	// agent with specific hardware.
	Inventory *types.HostInventory

	// FailSwitch / FailVNIC, when set, make the matching Ensure call return an
	// error. They exist so tests can exercise the reconciler's failure paths.
	FailSwitch string
	FailVNIC   string

	// EditionAsked / EditionKeySeen record what EnsureWindowsEdition was asked for,
	// so a test can prove the key reached it and was not logged. FailEdition drives
	// the failure path.
	EditionAsked   string
	EditionKeySeen string
	FailEdition    bool

	// WindowsLicence overrides the reported edition/activation, so tests can drive
	// the evaluation and grace-period paths.
	WindowsLicence *WindowsLicence

	// ISCSIOccupiedSerials names LUNs the stub should treat as already carrying a
	// filesystem, so the refusal-to-destroy path can be tested.
	ISCSIOccupiedSerials []string
	// ISCSIAdopted records which volumes have been adopted, so a second pass is
	// the no-op idempotency requires.
	ISCSIAdopted map[string]bool
	// CSVMountNote / FailCSVMount / CSVMountWant drive and record
	// EnsureCSVMountPoints, so the reconciler's advisory and not-yet-applied paths
	// can be exercised without a cluster.
	CSVMountNote string
	FailCSVMount bool
	CSVMountWant map[string]string

	// ISCSIUnconnected names targets EnsureISCSI should report as discovered but
	// not logged in — the state an array in reach but not granting this initiator
	// produces, which has a different remedy from an unreachable array.
	ISCSIUnconnected []string

	// HyperVInstalled / RebootPending seed GetHostRoleState. Default false (a
	// host where the role is not yet installed); set HyperVInstalled true to
	// model a ready host.
	HyperVInstalled bool
	RebootPending   bool

	// Identity: ComputerName / Domain seed GetHostIdentity. RenameCalled records
	// that the reconciler renamed the host. hostIPs models assigned NIC IPs.
	ComputerName string
	Domain       string
	RenameCalled bool
	JoinCalled   bool
	hostIPs      map[string]string

	vmHostVMPath  string
	vmHostVHDPath string
	liveMigration string

	// EnsureRoleCalled / RebootCalled record that the reconciler drove these, so
	// tests can assert reboot governance.
	EnsureRoleCalled bool
	RebootCalled     bool
	ShutdownCalled   bool

	// Clustering: ClusteringInstalled seeds the feature state; ClusterExists and
	// ClusterMembers model an existing cluster. FormCalled records that the
	// reconciler formed one, so tests can assert the former actually acted.
	ClusteringInstalled bool
	ClusterFirewallOpen bool
	ClusterExists       bool
	ClusterName         string
	ClusterMembers      []string
	FormCalled          bool

	// Witness models the cluster's observed quorum configuration; WitnessCalls
	// and WitnessErr drive and record EnsureClusterWitness.
	Witness      *ClusterWitness
	WitnessCalls []types.WitnessSpec
	WitnessErr   error

	// Storage: S2DEnabled seeds the S2D state; CSVs models existing volumes.
	// EnableS2DCalled records that the reconciler enabled it.
	S2DEnabled      bool
	EnableS2DCalled bool
	CSVs            []string

	// MaintenancePaused models whether this stub node is paused, so tests can
	// drive the reconciler's maintenance decision in both directions.
	MaintenancePaused bool

	// FailVM, when set, makes EnsureVM for the matching VM name return an error,
	// so tests can exercise the reconciler's VM failure path.
	FailVM string

	// FailVMState, when set, makes GetVMState for the matching VM name return an
	// error — a host whose Hyper-V cannot be read. Tests use it to prove the VM is
	// not reported as a healthy VM with no observed state.
	FailVMState string

	// FailVMHostPaths, when true, makes EnsureVMHostPaths return an error, so tests
	// can exercise the reconciler's best-effort (advisory, non-degrading) path.
	FailVMHostPaths bool

	// NetworkProfilePublic, when true, makes EnsureNetworkProfilesPrivate report it
	// had to flip a NIC (OutcomeUpdated); FailNetworkProfile makes it error. Both
	// let tests drive the reconciler's network-profile hygiene step.
	NetworkProfilePublic bool
	FailNetworkProfile   bool

	mu       sync.Mutex
	switches map[string]types.VirtualSwitchSpec
	vnics    map[string]types.ManagementVNICSpec
	vms      map[string]*stubVM
}

// stubVM models a VM's configuration and power state in the stub.
type stubVM struct {
	spec  types.VMSpec
	power types.VMPowerState
}

// CollectInventory returns the configured or default fixture inventory.
func (s *Stub) CollectInventory(_ context.Context) (types.HostInventory, error) {
	if s.Inventory != nil {
		return *s.Inventory, nil
	}
	return types.HostInventory{
		PhysicalAdapters: []types.PhysicalAdapter{
			{Name: "NIC1", MAC: "00:15:5D:00:00:01", LinkSpeedBps: 25_000_000_000, Up: true},
			{Name: "NIC2", MAC: "00:15:5D:00:00:02", LinkSpeedBps: 25_000_000_000, Up: true},
		},
		PhysicalDisks: []types.PhysicalDisk{
			{DeviceID: "0", SizeBytes: 1_920_383_410_176, MediaType: "SSD", CanPool: true},
			{DeviceID: "1", SizeBytes: 1_920_383_410_176, MediaType: "SSD", CanPool: true},
		},
		TotalMemoryBytes: 137_438_953_472, // 128 GiB
		LogicalCPUs:      32,
	}, nil
}

// CollectMetrics returns a plausible fixture so the console shows non-zero
// utilisation when developing against the stub.
func (s *Stub) CollectMetrics(_ context.Context) (types.HostMetrics, error) {
	return types.HostMetrics{
		CPUUsagePercent:  7,
		MemoryInUseBytes: 24 * 1024 * 1024 * 1024, // 24 GiB
		UptimeSeconds:    4 * 24 * 3600,           // 4 days
	}, nil
}

// CollectResources returns fixtures (plus any switches the stub has created) so
// the console shows real-looking choices when developing against the stub.
func (s *Stub) CollectResources(_ context.Context) (types.HostResources, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switches := make([]string, 0, len(s.switches))
	for name := range s.switches {
		switches = append(switches, name)
	}
	return types.HostResources{
		Switches: switches,
		Volumes: []types.StorageVolume{
			{Name: "CSV01-Perf", Path: `C:\ClusterStorage\CSV01-Perf`},
			{Name: "CSV02-Cap", Path: `C:\ClusterStorage\CSV02-Cap`},
		},
		ISOs: []string{`C:\ClusterStorage\CSV01-Perf\ISOs\WinServer2025.iso`},
	}, nil
}

func (s *Stub) EnsureSwitch(_ context.Context, spec types.VirtualSwitchSpec) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.FailSwitch != "" && s.FailSwitch == spec.Name {
		return OutcomeUnchanged, fmt.Errorf("stub: forced failure ensuring switch %q", spec.Name)
	}
	if s.switches == nil {
		s.switches = make(map[string]types.VirtualSwitchSpec)
	}
	cur, ok := s.switches[spec.Name]
	switch {
	case !ok:
		s.switches[spec.Name] = spec
		return OutcomeCreated, nil
	case reflect.DeepEqual(cur, spec):
		return OutcomeUnchanged, nil
	default:
		s.switches[spec.Name] = spec
		return OutcomeUpdated, nil
	}
}

func (s *Stub) EnsureMgmtVNIC(_ context.Context, spec types.ManagementVNICSpec) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.FailVNIC != "" && s.FailVNIC == spec.Name {
		return OutcomeUnchanged, fmt.Errorf("stub: forced failure ensuring vNIC %q", spec.Name)
	}
	// A management vNIC cannot exist without its switch; mirror that ordering
	// constraint so tests catch a reconciler that ensures vNICs first.
	if _, ok := s.switches[spec.SwitchName]; !ok {
		return OutcomeUnchanged, fmt.Errorf("stub: switch %q for vNIC %q does not exist", spec.SwitchName, spec.Name)
	}
	if s.vnics == nil {
		s.vnics = make(map[string]types.ManagementVNICSpec)
	}
	cur, ok := s.vnics[spec.Name]
	switch {
	case !ok:
		s.vnics[spec.Name] = spec
		return OutcomeCreated, nil
	case reflect.DeepEqual(cur, spec):
		return OutcomeUnchanged, nil
	default:
		s.vnics[spec.Name] = spec
		return OutcomeUpdated, nil
	}
}

// EnsureMgmtVNICs mirrors the real batched path: the observation is shared, the
// decisions are per vNIC. Deliberately delegates rather than reimplementing the
// create/update rules, so the stub cannot drift from the single-vNIC semantics
// the tests already pin.
func (s *Stub) EnsureMgmtVNICs(ctx context.Context, specs []types.ManagementVNICSpec) ([]Outcome, []error) {
	outs := make([]Outcome, len(specs))
	errs := make([]error, len(specs))
	for i, spec := range specs {
		outs[i], errs[i] = s.EnsureMgmtVNIC(ctx, spec)
	}
	return outs, errs
}

// GetVMLiveStates delegates to the per-VM read, so the stub cannot drift from
// single-VM semantics — the same approach EnsureMgmtVNICs takes. A VM the stub
// does not know is simply absent from the map, as on a real host.
func (s *Stub) GetVMLiveStates(ctx context.Context, names []string) (map[string]VMLive, error) {
	out := make(map[string]VMLive, len(names))
	for _, n := range names {
		st, err := s.GetVMState(ctx, n)
		if err != nil {
			return nil, err
		}
		if !st.Exists {
			continue
		}
		out[strings.ToLower(n)] = VMLive{
			Exists:              true,
			ID:                  st.ID,
			PowerState:          st.PowerState,
			AssignedMemoryBytes: st.AssignedMemoryBytes,
			MemoryDemandBytes:   st.MemoryDemandBytes,
			MemoryStatus:        st.MemoryStatus,
			CPUUsagePercent:     st.CPUUsagePercent,
			UptimeSeconds:       st.UptimeSeconds,
			IPAddress:           st.IPAddress,
			Replication:         st.Replication,
		}
	}
	return out, nil
}

// ClusterVMRolesPresent mirrors the stub's EnsureClusterVMRole, which treats
// every role as already existing. Reporting the same thing here keeps the
// batched and per-VM paths indistinguishable to existing tests, which is the
// point: the batch is an optimisation, not a behaviour change.
func (s *Stub) ClusterVMRolesPresent(_ context.Context, names []string) (map[string]bool, error) {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func (s *Stub) GetHostIdentity(_ context.Context) (HostIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := s.ComputerName
	if name == "" {
		name = "WIN-UNCONFIGURED"
	}
	return HostIdentity{ComputerName: name, Domain: s.Domain, PartOfDomain: s.Domain != ""}, nil
}

func (s *Stub) RenameComputer(_ context.Context, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RenameCalled = true
	s.ComputerName = newName
	s.RebootPending = true // takes effect on reboot
	return nil
}

func (s *Stub) JoinDomain(_ context.Context, domain, _ouPath, _user, _pass string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.JoinCalled = true
	s.Domain = domain
	s.RebootPending = true // takes effect on reboot
	return nil
}

func (s *Stub) EnsureHostIP(_ context.Context, spec types.PhysicalNICConfig) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostIPs == nil {
		s.hostIPs = make(map[string]string)
	}
	if s.hostIPs[spec.AdapterName] == spec.IPConfig.Address {
		return OutcomeUnchanged, nil
	}
	s.hostIPs[spec.AdapterName] = spec.IPConfig.Address
	return OutcomeUpdated, nil
}

func (s *Stub) EnsureVMHostPaths(_ context.Context, vmPath, vhdPath string) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailVMHostPaths {
		return OutcomeUnchanged, fmt.Errorf("stub: forced failure setting VM host paths")
	}
	if s.vmHostVMPath == vmPath && s.vmHostVHDPath == vhdPath {
		return OutcomeUnchanged, nil
	}
	s.vmHostVMPath, s.vmHostVHDPath = vmPath, vhdPath
	return OutcomeUpdated, nil
}

func (s *Stub) RemoveSwitch(_ context.Context, _ string) error       { return nil }
func (s *Stub) RemoveVM(_ context.Context, _ string) error           { return nil }
func (s *Stub) FormatDisk(_ context.Context, _ string) error         { return nil }
func (s *Stub) FormatDiskDrive(_ context.Context, _, _ string) error { return nil }
func (s *Stub) DestroyCluster(_ context.Context) error               { return nil }

func (s *Stub) EnsureLiveMigration(_ context.Context, spec types.LiveMigrationSpec) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%v|%s|%d|%v", spec.Enabled, spec.AuthenticationType, spec.MaxConcurrent, spec.Networks)
	if s.liveMigration == key {
		return OutcomeUnchanged, nil
	}
	s.liveMigration = key
	return OutcomeUpdated, nil
}

func (s *Stub) GetHostRoleState(_ context.Context) (HostRoleState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return HostRoleState{HyperVInstalled: s.HyperVInstalled, RebootPending: s.RebootPending}, nil
}

func (s *Stub) EnsureHyperVRole(_ context.Context) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.HyperVInstalled {
		return OutcomeUnchanged, nil
	}
	// Install stages the role; it becomes active only after a reboot.
	s.EnsureRoleCalled = true
	s.RebootPending = true
	return OutcomeCreated, nil
}

func (s *Stub) RebootHost(_ context.Context, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RebootCalled = true
	// Model the reboot completing the install.
	if s.RebootPending {
		s.HyperVInstalled = true
		s.RebootPending = false
	}
	return nil
}

func (s *Stub) ShutdownHost(_ context.Context, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ShutdownCalled = true
	return nil
}

func (s *Stub) EnableRDP(_ context.Context) error { return nil }

func (s *Stub) GetClusterState(_ context.Context) (ClusterState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ClusterState{Exists: s.ClusterExists, Known: true, Name: s.ClusterName,
		Members: s.ClusterMembers, Witness: s.Witness}, nil
}

func (s *Stub) EnsureFailoverClusteringFeature(_ context.Context) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ClusteringInstalled {
		return OutcomeUnchanged, nil
	}
	s.ClusteringInstalled = true
	return OutcomeCreated, nil
}

func (s *Stub) EnsureClusterFirewall(_ context.Context) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ClusterFirewallOpen {
		return OutcomeUnchanged, nil
	}
	s.ClusterFirewallOpen = true
	return OutcomeUpdated, nil
}

func (s *Stub) FormCluster(_ context.Context, f ClusterFormation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FormCalled = true
	s.ClusterExists = true
	s.ClusterName = f.Name
	s.ClusterMembers = append([]string(nil), f.Members...)
	return nil
}

func (s *Stub) GetStorageState(_ context.Context) (StorageState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StorageState{S2DEnabled: s.S2DEnabled, S2DKnown: true, Volumes: append([]string(nil), s.CSVs...)}, nil
}

func (s *Stub) EnableS2D(_ context.Context) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.S2DEnabled {
		return OutcomeUnchanged, nil
	}
	s.EnableS2DCalled = true
	s.S2DEnabled = true
	return OutcomeCreated, nil
}

func (s *Stub) EnsureS2DPoolDisks(_ context.Context) (Outcome, error) {
	return OutcomeUnchanged, nil
}

func (s *Stub) EnsureCSV(_ context.Context, spec CSVProvision) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.CSVs {
		if v == spec.Name {
			return OutcomeUnchanged, nil
		}
	}
	s.CSVs = append(s.CSVs, spec.Name)
	return OutcomeCreated, nil
}

func (s *Stub) RemoveCSV(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.CSVs[:0]
	for _, v := range s.CSVs {
		if v != name {
			out = append(out, v)
		}
	}
	s.CSVs = out
	return nil
}

func (s *Stub) RepairStoragePool(_ context.Context) (string, error) {
	return "NOOP pool is Healthy (stub)", nil
}

func (s *Stub) UpdateClusterFunctionalLevel(_ context.Context) (string, error) {
	return "cluster functional level is already 12 (no change) (stub)", nil
}

// EnsureISCSI reports a connected, single-path array so a stub-backed reconcile
// exercises the success path without a network.
func (s *Stub) EnsureISCSI(_ context.Context, spec types.ISCSIStorageSpec, _, _ string, _ bool) (ISCSIState, Outcome, error) {
	st := ISCSIState{
		InitiatorIQN:   "iqn.1991-05.com.microsoft:stub.lab.local",
		ServiceRunning: true,
		Portals:        spec.Portals,
		MPIOInstalled:  true,
	}
	for _, t := range spec.Targets {
		connected := true
		for _, u := range s.ISCSIUnconnected {
			if u == t {
				connected = false
			}
		}
		st.Sessions = append(st.Sessions, ISCSISessionState{
			TargetIQN: t, Connected: connected, Persistent: connected, Paths: map[bool]int{true: 1, false: 0}[connected],
		})
	}
	return st, OutcomeUnchanged, nil
}

// AdoptISCSIDisk succeeds for any identified LUN, and refuses one the test has
// declared as carrying data — the refusal is the behaviour worth exercising.
func (s *Stub) AdoptISCSIDisk(_ context.Context, a ISCSIAdoption) (string, string, Outcome, error) {
	if a.Source.SerialNumber == "" && a.Source.TargetIQN == "" {
		return "", "", OutcomeUnchanged, fmt.Errorf("volume %q does not say which LUN it is (stub)", a.Name)
	}
	for _, occupied := range s.ISCSIOccupiedSerials {
		if occupied == a.Source.SerialNumber && !a.Wipe {
			return "", "", OutcomeUnchanged, fmt.Errorf("the LUN (serial %s) already contains an NTFS volume. Adopting it formats it, so Ballast will not do that to a disk with contents (stub)", occupied)
		}
	}
	serial := a.Source.SerialNumber
	if serial == "" {
		serial = "STUBSERIAL0001"
	}
	if s.ISCSIAdopted == nil {
		s.ISCSIAdopted = map[string]bool{}
	}
	if s.ISCSIAdopted[a.Name] {
		return serial, "", OutcomeUnchanged, nil
	}
	s.ISCSIAdopted[a.Name] = true
	return serial, "", OutcomeUpdated, nil
}

// EnsureCSVMountPoints reports every declared volume as already correctly
// mounted, so a stub-backed reconcile does not invent a rename.
func (s *Stub) EnsureCSVMountPoints(_ context.Context, want map[string]string) (Outcome, string, error) {
	s.CSVMountWant = want
	if s.FailCSVMount {
		return OutcomeUnchanged, "", fmt.Errorf("stub: forced failure naming CSV mount points")
	}
	return OutcomeUnchanged, s.CSVMountNote, nil
}

// GetWindowsLicence reports an activated Datacenter host, so a stub-backed
// reconcile does not show a fleet in evaluation. WindowsLicence overrides it.
func (s *Stub) GetWindowsLicence(_ context.Context) (WindowsLicence, error) {
	if s.WindowsLicence != nil {
		return *s.WindowsLicence, nil
	}
	return WindowsLicence{
		Edition: "ServerDatacenter", Description: "Microsoft Windows Server 2025 Datacenter",
		Status: "Licensed", Channel: "Volume:MAK", PartialProductKey: "ABCDE",
	}, nil
}

// EnsureWindowsEdition records the request and reports a staged conversion, so
// the reconciler's reboot governance can be exercised without a host.
func (s *Stub) EnsureWindowsEdition(_ context.Context, targetEdition, productKey string) (Outcome, bool, error) {
	s.EditionAsked, s.EditionKeySeen = targetEdition, productKey
	if s.FailEdition {
		return OutcomeUnchanged, false, fmt.Errorf("stub: forced failure converting edition")
	}
	cur := "ServerDatacenter"
	if s.WindowsLicence != nil && s.WindowsLicence.Edition != "" {
		cur = s.WindowsLicence.Edition
	}
	if cur == targetEdition {
		return OutcomeUnchanged, false, nil
	}
	return OutcomeUpdated, true, nil
}

func (s *Stub) CheckISOLibrary(_ context.Context, path string) (ISOLibraryState, error) {
	if path == "" {
		return ISOLibraryState{}, nil
	}
	ok := true
	return ISOLibraryState{Path: path, Readable: true, MachineReadable: &ok, ISOs: []string{"stub.iso"}}, nil
}

func (s *Stub) GetNetworkProfile(_ context.Context) (string, error) {
	return "DomainAuthenticated", nil
}

func (s *Stub) PruneManagementVNICs(_ context.Context, _, _ []string) (Outcome, error) {
	return OutcomeUnchanged, nil
}

func (s *Stub) RemoveMgmtVNIC(_ context.Context, _ string) error { return nil }

func (s *Stub) ResetPoolDisks(_ context.Context) (string, error) {
	return "RESULT wiped=0 nowPoolable=0 skipped=0", nil
}

func (s *Stub) ConvergedNetworkReady(_ context.Context, _ []string) (bool, error) { return true, nil }

func (s *Stub) RebuildStoragePool(_ context.Context) (string, error) {
	return "REBUILT S2D Pool Healthy (stub)", nil
}

func (s *Stub) GetVMState(_ context.Context, name string) (VMState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailVMState != "" && s.FailVMState == name {
		return VMState{}, fmt.Errorf("stub: forced failure reading VM state for %q", name)
	}
	vm, ok := s.vms[name]
	if !ok {
		return VMState{}, nil
	}
	return VMState{Exists: true, PowerState: vm.power}, nil
}

func (s *Stub) ListObservedVMs(_ context.Context) ([]types.ObservedVM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.ObservedVM, 0, len(s.vms))
	for name, vm := range s.vms {
		out = append(out, types.ObservedVM{Name: name, PowerState: vm.power})
	}
	return out, nil
}

// WatchVMState is a no-op subscription for the stub: it emits no events and
// simply blocks until the context is cancelled, so the supervising loop treats
// it as a live-but-idle watcher rather than a failed one to restart.
func (s *Stub) WatchVMState(ctx context.Context, _ func()) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *Stub) EnsureVM(_ context.Context, vm types.VM) (VMEnsureResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name := vm.Meta.Name
	if s.FailVM != "" && s.FailVM == name {
		return VMEnsureResult{}, fmt.Errorf("stub: forced failure ensuring VM %q", name)
	}
	// A VM adapter cannot attach to a switch that does not exist; mirror that so
	// tests catch a reconciler that ensures VMs before their switches.
	for _, a := range vm.Spec.NetworkAdapters {
		if _, ok := s.switches[a.SwitchName]; !ok {
			return VMEnsureResult{}, fmt.Errorf("stub: switch %q for VM %q adapter %q does not exist", a.SwitchName, name, a.Name)
		}
	}
	if s.vms == nil {
		s.vms = make(map[string]*stubVM)
	}
	cur, ok := s.vms[name]
	switch {
	case !ok:
		// New VMs come up Off; SetVMPowerState drives them to desired.
		s.vms[name] = &stubVM{spec: vm.Spec, power: types.VMPowerOff}
		return VMEnsureResult{Outcome: OutcomeCreated}, nil
	case reflect.DeepEqual(cur.spec, vm.Spec):
		return VMEnsureResult{Outcome: OutcomeUnchanged}, nil
	default:
		// Processor count and static startup memory cannot change while running;
		// defer them and report PendingPowerOff, mirroring Hyper-V.
		sizingChanged := cur.spec.ProcessorCount != vm.Spec.ProcessorCount ||
			cur.spec.MemoryStartupBytes != vm.Spec.MemoryStartupBytes ||
			!reflect.DeepEqual(cur.spec.DynamicMemory, vm.Spec.DynamicMemory)
		if cur.power == types.VMPowerRunning && sizingChanged {
			// Apply the online-mutable parts, keep the running VM's sizing as-is.
			applied := cur.spec
			applied.ProcessorCount = cur.spec.ProcessorCount
			applied.MemoryStartupBytes = cur.spec.MemoryStartupBytes
			applied.DynamicMemory = cur.spec.DynamicMemory
			applied.Disks = vm.Spec.Disks
			applied.NetworkAdapters = vm.Spec.NetworkAdapters
			applied.DesiredPowerState = vm.Spec.DesiredPowerState
			cur.spec = applied
			return VMEnsureResult{Outcome: OutcomeUpdated, PendingPowerOff: true}, nil
		}
		cur.spec = vm.Spec
		return VMEnsureResult{Outcome: OutcomeUpdated}, nil
	}
}

func (s *Stub) SetVMPowerState(_ context.Context, name string, desired types.VMPowerState) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.vms[name]
	if !ok {
		return OutcomeUnchanged, fmt.Errorf("stub: VM %q does not exist", name)
	}
	if vm.power == desired {
		return OutcomeUnchanged, nil
	}
	vm.power = desired
	return OutcomeUpdated, nil
}

func (s *Stub) RestartVM(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vms[name]; !ok {
		return fmt.Errorf("stub: VM %q does not exist", name)
	}
	return nil
}

// GetVMScreen returns no screenshot in the stub.
func (s *Stub) GetVMScreen(_ context.Context, _ string) ([]byte, error) { return nil, nil }

// CreateVMCheckpoint / ExportVM / AddClusterNode / EvictClusterNode record the job ran.
func (s *Stub) CreateVMCheckpoint(_ context.Context, _, _ string) error { return nil }
func (s *Stub) ExportVM(_ context.Context, _, _ string) error           { return nil }
func (s *Stub) CloneVM(_ context.Context, _, _, _ string) error         { return nil }

func (s *Stub) CaptureTemplate(_ context.Context, _, _ string, _, _ bool, _, _ string) (uint64, error) {
	return 0, nil
}

func (s *Stub) DeployFromTemplate(_ context.Context, _, _, _ string) error { return nil }
func (s *Stub) DiscardVMSavedState(_ context.Context, _ string) error      { return nil }

// EnsureNodeMaintenance models pausing/resuming this node. A stub not in a
// cluster reports no membership, mirroring a standalone host where maintenance
// has nothing to enforce locally.
func (s *Stub) EnsureNodeMaintenance(_ context.Context, _ string, intent MaintenanceIntent) (Outcome, NodeMaintenanceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ClusterExists {
		return OutcomeUnchanged, NodeMaintenanceState{}, nil
	}
	want := s.MaintenancePaused // Observe changes nothing
	switch intent {
	case MaintenanceEnter:
		want = true
	case MaintenanceExit:
		want = false
	}
	if s.MaintenancePaused == want {
		return OutcomeUnchanged, NodeMaintenanceState{IsMember: true, Paused: want}, nil
	}
	s.MaintenancePaused = want
	return OutcomeUpdated, NodeMaintenanceState{IsMember: true, Paused: want}, nil
}
func (s *Stub) MoveVMStorage(_ context.Context, vm, folder string, _ ProgressFunc) (string, error) {
	return "moved " + vm + " storage to " + folder, nil
}
func (s *Stub) FetchISO(_ context.Context, _, _ string) (string, error)             { return "", nil }
func (s *Stub) GuestJoinDomain(_ context.Context, _, _, _, _, _, _, _ string) error { return nil }
func (s *Stub) GuestSetIP(_ context.Context, _, _, _, _, _, _, _ string) error      { return nil }
func (s *Stub) ApplyVMCheckpoint(_ context.Context, _, _ string) error              { return nil }
func (s *Stub) RemoveVMCheckpoint(_ context.Context, _, _ string) error             { return nil }

func (s *Stub) AddClusterNode(_ context.Context, node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ClusterExists {
		s.ClusterMembers = append(s.ClusterMembers, node)
	}
	return nil
}

func (s *Stub) EvictClusterNode(_ context.Context, node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.ClusterMembers[:0]
	for _, m := range s.ClusterMembers {
		if m != node {
			out = append(out, m)
		}
	}
	s.ClusterMembers = out
	return nil
}

func (s *Stub) DrainNode(_ context.Context, _ string) error  { return nil }
func (s *Stub) ResumeNode(_ context.Context, _ string) error { return nil }

func (s *Stub) EnsureClusterVMRole(_ context.Context, _ string) (Outcome, error) {
	return OutcomeUnchanged, nil
}
func (s *Stub) MoveClusterGroup(_ context.Context, _, _ string) error        { return nil }
func (s *Stub) MoveClusterSharedVolume(_ context.Context, _, _ string) error { return nil }
func (s *Stub) MoveClusterVM(_ context.Context, _, _ string, onProgress ProgressFunc) error {
	onProgress.emit("live migration 100%")
	return nil
}
func (s *Stub) MigrateVM(_ context.Context, vm, destHost, _ string, onProgress ProgressFunc) (string, error) {
	onProgress.emit("live migration 100%")
	return "migrated " + vm + " to " + destHost + " (stub)", nil
}
func (s *Stub) ValidateCluster(_ context.Context, _, _ []string) (string, error) {
	return "validation ok (stub)", nil
}

func (s *Stub) ClusterLog(_ context.Context, _, _ string) (string, error) {
	return "cluster log (stub)", nil
}

func (s *Stub) EnsureReplicaServer(_ context.Context, _ types.ReplicaServerSpec) (Outcome, error) {
	return OutcomeUnchanged, nil
}

func (s *Stub) EnsureReplicaBroker(_ context.Context, _ types.ReplicaBrokerSpec) (Outcome, error) {
	return OutcomeUnchanged, nil
}

// WitnessCalls records every EnsureClusterWitness call, so tests can assert not
// only what was asked for but that a non-former asked for nothing at all.
// WitnessErr, when set, models a witness that cannot be applied (an unreachable
// share, or permissions missing on the cluster computer object).
func (s *Stub) EnsureClusterWitness(_ context.Context, w types.WitnessSpec, _ types.ClusterStorageKind) (Outcome, error) {
	s.WitnessCalls = append(s.WitnessCalls, w)
	if s.WitnessErr != nil {
		return OutcomeUnchanged, s.WitnessErr
	}
	// Model the real thing: applying a witness that already matches is a no-op.
	if s.Witness != nil && s.Witness.Type == string(w.Type) && s.Witness.Path == w.FileSharePath {
		return OutcomeUnchanged, nil
	}
	s.Witness = &ClusterWitness{Type: string(w.Type), Path: w.FileSharePath,
		State: "Online", QuorumType: "NodeAndFileShareMajority"}
	return OutcomeUpdated, nil
}

func (s *Stub) RemoveReplicaBroker(_ context.Context, _ string) (string, error) {
	return "no Hyper-V Replica Broker in this cluster (stub)", nil
}

func (s *Stub) EnsureVMReplication(_ context.Context, _ string, _ types.VMReplicationSpec) (Outcome, error) {
	return OutcomeUnchanged, nil
}

func (s *Stub) TestFailover(_ context.Context, vm, _ string) (string, error) {
	return "test failover of " + vm + " (stub)", nil
}

func (s *Stub) StopTestFailover(_ context.Context, _ string) error { return nil }

func (s *Stub) PlannedFailover(_ context.Context, vm, primaryHost, _ string) (string, error) {
	return "planned failover of " + vm + " from " + primaryHost + " (stub)", nil
}

func (s *Stub) Failover(_ context.Context, vm, _, _ string) (string, error) {
	return "unplanned failover of " + vm + " (stub)", nil
}

func (s *Stub) CancelFailover(_ context.Context, _ string) error { return nil }

func (s *Stub) ReverseReplication(_ context.Context, vm string) (string, error) {
	return "reversed replication of " + vm + " (stub)", nil
}

func (s *Stub) RemoveReplicaVM(_ context.Context, _ string) error { return nil }

func (s *Stub) RepairHostDNS(_ context.Context, _ string) (string, error) {
	return "dns repaired (stub)", nil
}

func (s *Stub) RepairNetworkProfile(_ context.Context) (string, error) {
	return "network profiles ok (stub)", nil
}

func (s *Stub) EnsureNetworkProfilesPrivate(_ context.Context) (Outcome, error) {
	if s.FailNetworkProfile {
		return OutcomeUnchanged, fmt.Errorf("stub: forced failure setting network profile")
	}
	if s.NetworkProfilePublic {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

func (s *Stub) EnsureHostDNS(_ context.Context, _ []string) (Outcome, error) {
	return OutcomeUnchanged, nil
}

func (s *Stub) EnsureMigrationDelegation(_ context.Context, _ []string) (Outcome, error) {
	return OutcomeUnchanged, nil
}

// HasVM reports whether the stub currently models a VM by that name.
func (s *Stub) HasVM(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.vms[name]
	return ok
}

// HasSwitch reports whether the stub currently models a switch by that name.
func (s *Stub) HasSwitch(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.switches[name]
	return ok
}

// HasMgmtVNIC reports whether the stub currently models a vNIC by that name.
func (s *Stub) HasMgmtVNIC(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.vnics[name]
	return ok
}

// MgmtVNIC returns the spec the reconciler actually applied, so a test can check
// what was written rather than only that something was.
func (s *Stub) MgmtVNIC(name string) (types.ManagementVNICSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vnics[name]
	return v, ok
}

// compile-time assertion that Stub satisfies the interface.
var _ Interface = (*Stub)(nil)

package hyperv

import (
	"context"
	"fmt"
	"reflect"
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

	// HyperVInstalled / RebootPending seed GetHostRoleState. Default false (a
	// host where the role is not yet installed); set HyperVInstalled true to
	// model a ready host.
	HyperVInstalled bool
	RebootPending   bool

	// EnsureRoleCalled / RebootCalled record that the reconciler drove these, so
	// tests can assert reboot governance.
	EnsureRoleCalled bool
	RebootCalled     bool

	// Clustering: ClusteringInstalled seeds the feature state; ClusterExists and
	// ClusterMembers model an existing cluster. FormCalled records that the
	// reconciler formed one, so tests can assert the former actually acted.
	ClusteringInstalled bool
	ClusterExists       bool
	ClusterName         string
	ClusterMembers      []string
	FormCalled          bool

	mu       sync.Mutex
	switches map[string]types.VirtualSwitchSpec
	vnics    map[string]types.ManagementVNICSpec
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

func (s *Stub) RebootHost(_ context.Context) error {
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

func (s *Stub) GetClusterState(_ context.Context) (ClusterState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ClusterState{Exists: s.ClusterExists, Name: s.ClusterName, Members: s.ClusterMembers}, nil
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

func (s *Stub) FormCluster(_ context.Context, f ClusterFormation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FormCalled = true
	s.ClusterExists = true
	s.ClusterName = f.Name
	s.ClusterMembers = append([]string(nil), f.Members...)
	return nil
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

// compile-time assertion that Stub satisfies the interface.
var _ Interface = (*Stub)(nil)

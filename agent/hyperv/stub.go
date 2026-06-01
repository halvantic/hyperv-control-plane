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

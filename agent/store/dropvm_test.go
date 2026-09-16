package store

import (
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// A successful RemoveVM must change what the agent WANTS, not only what the host
// has. Until the next pull the cached set still names the deleted VM, and the
// reconcile loop creates whatever that set names — which is a VM coming back
// minutes after an operator deleted it.
func TestDropDesiredVM(t *testing.T) {
	vm := func(n string) types.VM { return types.VM{Meta: types.ObjectMeta{Name: n}} }

	cases := []struct {
		name string
		set  []types.VM
		drop string
		want []string
	}{
		{
			name: "removes the named VM and leaves the rest",
			set:  []types.VM{vm("Tes"), vm("Windows"), vm("Linux")},
			drop: "Tes",
			want: []string{"Windows", "Linux"},
		},
		{
			// Names are case-insensitive everywhere else in the product (Windows and
			// AD names are), and the job's param carries whatever case the operator
			// or the cluster used. A case-sensitive drop would silently keep the VM.
			name: "matches the name case-insensitively",
			set:  []types.VM{vm("Tes"), vm("Windows")},
			drop: "TES",
			want: []string{"Windows"},
		},
		{
			name: "a name that is not in the set is a no-op",
			set:  []types.VM{vm("Windows")},
			drop: "Tes",
			want: []string{"Windows"},
		},
		{
			name: "dropping the last VM leaves an empty set, not an absent one",
			set:  []types.VM{vm("Tes")},
			drop: "Tes",
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			if err := s.SaveDesiredVMs(tc.set); err != nil {
				t.Fatalf("save: %v", err)
			}
			if err := s.DropDesiredVM(tc.drop); err != nil {
				t.Fatalf("drop: %v", err)
			}
			got, ok, err := s.LoadDesiredVMs()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			// An empty set is intent ("the centre wants no VMs here"), so it must
			// still read as a set the agent has been given, not as never having
			// heard from the centre.
			if !ok {
				t.Fatal("the set must still be present after a drop")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, names(got))
			}
			for i, n := range tc.want {
				if got[i].Meta.Name != n {
					t.Fatalf("want %v, got %v", tc.want, names(got))
				}
			}
		})
	}
}

// Dropping before the agent has ever been given a set must not author one.
// Persisting an empty set here would tell a restarted agent the centre had said
// "no VMs", which is a different claim from "we have not heard yet".
func TestDropDesiredVMBeforeAnyPull(t *testing.T) {
	s := openTemp(t)
	if err := s.DropDesiredVM("Tes"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, ok, err := s.LoadDesiredVMs(); err != nil || ok {
		t.Fatalf("no set must have been authored: ok=%v err=%v", ok, err)
	}
}

func names(vms []types.VM) []string {
	out := make([]string, 0, len(vms))
	for _, vm := range vms {
		out = append(out, vm.Meta.Name)
	}
	return out
}

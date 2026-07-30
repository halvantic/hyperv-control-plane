package hyperv

import (
	"context"
	"strings"
	"testing"
)

// A NIC stuck on Public is what silently blocks cluster and SMB traffic, so the
// reconcile-driven heal has to actually clear it. Set-NetConnectionProfile
// refuses while NLA is still identifying the network ("network marked
// 'Identifying...'"), which on an isolated storage/live-migration VLAN with no
// gateway or DNS can persist indefinitely — a heal that only calls Set gives up
// on every pass and the NIC stays Public. Observed live: a Storage vNIC sat
// Public through repeated passes, blocking the SMB traffic carrying CSV I/O and
// taking a volume degraded, while the operator-run repair fixed it in one go
// precisely because it restarts NLA.
func TestEnsureNetworkProfilesPrivateRestartsNLAWhenIdentifying(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("changed=0 failed=0")}}
	if _, err := newTestPS(f).EnsureNetworkProfilesPrivate(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]
	for _, want := range []string{
		"Set-NetConnectionProfile -InterfaceIndex $prof.InterfaceIndex -NetworkCategory Private",
		// The transient identification state is recognised rather than treated as
		// a permanent failure.
		"-match 'Identifying'",
		// And answered with the one thing that clears it.
		"Restart-Service NlaSvc -Force",
		// Then retried, so the pass reports what it actually achieved.
		"$failed = @()",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("network profile heal missing %q\n---\n%s", want, s)
		}
	}
}

// The outcome still has to reflect reality: a Set that failed is an error the
// reconciler records as an advisory condition, not a silent success.
func TestEnsureNetworkProfilesPrivateReportsFailures(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("changed=0 failed=1 :: vEthernet (Storage): denied")}}
	out, err := newTestPS(f).EnsureNetworkProfilesPrivate(context.Background())
	if err == nil {
		t.Fatal("a failed profile change must surface, not report success")
	}
	if out != OutcomeUnchanged {
		t.Fatalf("nothing changed, got %v", out)
	}
	if !strings.Contains(err.Error(), "vEthernet (Storage)") {
		t.Fatalf("the error should name the interface, got %v", err)
	}
}

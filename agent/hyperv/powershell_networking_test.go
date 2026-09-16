package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// The reported network location must describe the domain-facing NIC, not the
// weakest profile on the host. A converged host's storage and live-migration
// vNICs are correctly Private (isolated subnets, no DC to authenticate against),
// so a weakest-wins rollup across every profile reported Private on every
// properly built host and hid the management NIC's real category.
func TestGetNetworkProfileScopesToGatewayNICs(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("DomainAuthenticated\r\n")}}
	got, err := newTestPS(f).GetNetworkProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "DomainAuthenticated" {
		t.Fatalf("profile = %q, want DomainAuthenticated", got)
	}

	script := f.calls[0]
	// Selection must be restricted to interfaces carrying a default route.
	if !strings.Contains(script, `-DestinationPrefix '0.0.0.0/0'`) {
		t.Error("script must find gateway-bearing interfaces via the default route")
	}
	if !strings.Contains(script, "$gwIdx -contains [int]$_.InterfaceIndex") {
		t.Error("script must filter connection profiles to gateway-bearing interfaces")
	}
	// A host mid-build with no gateway anywhere must still report something.
	if !strings.Contains(script, "if ($sel.Count -eq 0) { $sel = $profs }") {
		t.Error("script must fall back to all profiles when none has a gateway")
	}
	// Weakest-wins ordering is retained within the selected set: a management NIC
	// stuck on Public is the case the pill exists to surface.
	pub := strings.Index(script, "'Public'")
	priv := strings.Index(script, "'Private'")
	dom := strings.Index(script, "'DomainAuthenticated'")
	if !(pub < priv && priv < dom) {
		t.Error("category precedence must stay Public, then Private, then DomainAuthenticated")
	}
}

// An empty result is passed through as empty rather than becoming a category.
func TestGetNetworkProfileEmpty(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("\r\n")}}
	got, err := newTestPS(f).GetNetworkProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("profile = %q, want empty", got)
	}
}

/* A NIC swap ghosts the old adapters, and a ghost keeps its static address in
   the registry where Get-NetIPAddress cannot see it. New-NetIPAddress then
   refuses the address as already owned. Observed on the rig 2026-08-05: the
   fabric vNICs came back unaddressed, the cluster storage network partitioned,
   and the agent reported "exit status 1:" with nothing after the colon. */

func TestApplyIPReleasesAGhostAdapterAndSaysWhatFailed(t *testing.T) {
	s := applyIPScript("Storage", "10.0.60.12", 24, &types.IPConfig{Address: "10.0.60.12/24"})

	if !strings.Contains(s, "Clear-BallastGhostAddress") {
		t.Fatal("a ghost adapter holding the address must be released, or the apply can never succeed")
	}
	// Only non-present adapters may be touched. Without this guard the cleanup
	// would strip addresses from live NICs.
	if !strings.Contains(s, "if ($present -contains [string]$k.PSChildName) { continue }") {
		t.Error("the cleanup must skip adapters that still exist")
	}
	// The error text is the whole point: a bare "exit status 1:" told the
	// operator nothing, which is why this took a registry dig to diagnose.
	if !strings.Contains(s, "could not apply ") {
		t.Error("a failure must carry its reason, not rely on stderr")
	}
	if !strings.Contains(s, "even after releasing it from removed adapter(s)") {
		t.Error("a retry that still fails must say the ghost was already released, so nobody chases it twice")
	}
	// It must still be one attempt, a release, then one retry — not a loop.
	if got := strings.Count(s, "New-NetIPAddress -InterfaceAlias $alias"); got != 2 {
		t.Errorf("want exactly one attempt and one retry, got %d New-NetIPAddress calls", got)
	}
}

// The gateway variant must get the same treatment; a management vNIC that
// cannot take its address strands the host entirely.
func TestApplyIPGhostCleanupCoversTheGatewayForm(t *testing.T) {
	s := applyIPScript("ConvergedSwitch", "192.168.1.71", 24,
		&types.IPConfig{Address: "192.168.1.71/24", Gateway: "192.168.1.1", DNSServers: []string{"192.168.1.168"}})

	if !strings.Contains(s, "Clear-BallastGhostAddress") {
		t.Fatal("the gateway form must release a ghost's claim too")
	}
	if !strings.Contains(s, "-DefaultGateway") {
		t.Error("the gateway must survive being moved into the try/catch")
	}
	if got := strings.Count(s, "-DefaultGateway"); got != 2 {
		t.Errorf("the retry must apply the gateway as well, got %d", got)
	}
}

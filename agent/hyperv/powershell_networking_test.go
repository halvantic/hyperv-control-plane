package hyperv

import (
	"context"
	"strings"
	"testing"
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

package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Two DNS reconcilers on one interface. EnsureHostDNS wrote the host-level list
   onto every routed adapter, the management vNIC's own spec declared a different
   list, and the vNIC reconciler "corrected" it by re-applying the whole IP
   config — removing and re-adding the host's address every pass. HV01/HV02
   2026-11-18: ~5s off the network every ~55s after a second DNS server was added
   in Settings. */

func TestEnsureHostDNSLeavesVNICsWithTheirOwnDNSAlone(t *testing.T) {
	s := ensureHostDNSScript([]string{"192.168.1.80", "192.168.1.81"}, []string{"Management"})
	if !strings.Contains(s, "function Get-BallastVNICAlias") {
		t.Fatal("the vNIC must be resolved the same way the vNIC reconciler resolves it")
	}
	if !strings.Contains(s, "foreach ($v in @('Management'))") {
		t.Fatalf("the vNIC that owns its DNS must be named to the script:\n%s", s)
	}
	skip := strings.Index(s, "if ($ownIdx -contains [int]$n.ifIndex) { continue }")
	set := strings.Index(s, "Set-DnsClientServerAddress -InterfaceIndex $n.ifIndex -ServerAddresses $servers")
	if skip == -1 || set == -1 || skip > set {
		t.Fatal("an interface owned by a vNIC's own DNS must be skipped before any DNS is written to it")
	}
}

func TestEnsureHostDNSWithNoOwnersSkipsNothing(t *testing.T) {
	s := ensureHostDNSScript([]string{"192.168.1.80"}, nil)
	if !strings.Contains(s, "foreach ($v in @())") {
		t.Fatalf("no owners must render as an empty list, not a malformed one:\n%s", s)
	}
}

func TestADNSOnlyDifferenceDoesNotTouchTheAddress(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	desired := &types.IPConfig{Address: "192.168.1.71/24", Gateway: "192.168.1.1", DNSServers: []string{"192.168.1.80", "192.168.1.81"}}
	obs := ipObservation{Address: "192.168.1.71/24", Gateway: "192.168.1.1", DNSServers: []string{"192.168.1.80"}, Registers: true}

	changed, err := newTestPS(f).reconcileVNICIP(context.Background(), "Management", desired, &obs)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a DNS difference is still a change and must be applied")
	}
	if len(f.calls) != 1 {
		t.Fatalf("want one script, got %d", len(f.calls))
	}
	s := f.calls[0]
	for _, bad := range []string{"Remove-NetIPAddress", "New-NetIPAddress", "Remove-NetRoute", "-Dhcp Disabled"} {
		if strings.Contains(s, bad) {
			t.Fatalf("a DNS-only change must not touch the address or routes, but the script runs %s", bad)
		}
	}
	if !strings.Contains(s, "-ServerAddresses '192.168.1.80','192.168.1.81'") {
		t.Fatalf("both servers must be set, in order:\n%s", s)
	}
}

func TestARegistrationOnlyDifferenceDoesNotTouchTheAddress(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	desired := &types.IPConfig{Address: "10.0.60.10/24"}
	obs := ipObservation{Address: "10.0.60.10/24", Registers: true}

	if _, err := newTestPS(f).reconcileVNICIP(context.Background(), "Storage", desired, &obs); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]
	if strings.Contains(s, "Remove-NetIPAddress") {
		t.Fatal("turning DNS registration off must not remove the address")
	}
	if !strings.Contains(s, "-RegisterThisConnectionsAddress $false") {
		t.Fatal("an isolated vNIC must stop registering")
	}
}

func TestAnAddressDifferenceStillRunsTheFullApply(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	desired := &types.IPConfig{Address: "192.168.1.72/24", Gateway: "192.168.1.1", DNSServers: []string{"192.168.1.80"}}
	obs := ipObservation{Address: "192.168.1.71/24", Gateway: "192.168.1.1", DNSServers: []string{"192.168.1.80"}, Registers: true}

	if _, err := newTestPS(f).reconcileVNICIP(context.Background(), "Management", desired, &obs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[0], "New-NetIPAddress") {
		t.Fatal("a changed address needs the full apply")
	}
}

func TestRepairHostDNSSplitsAListOfServers(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("no change")}}
	if _, err := newTestPS(f).RepairHostDNS(context.Background(), "192.168.1.80, 192.168.1.81"); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]
	if !strings.Contains(s, "-ServerAddresses @($dc.Split(','))") {
		t.Fatal("the DC list must reach -ServerAddresses as an array; as one string Windows rejects it")
	}
	if strings.Contains(s, "catch {} }\n") && strings.Contains(s, "-ServerAddresses $dc ") {
		t.Fatal("a failed DNS write must be reported, not swallowed")
	}
}

// The witness script takes its credential from the environment and splats it,
// on BOTH the first apply and the retry after clearing a stuck resource. A
// retry that dropped the credential would fail on a NAS share for a reason
// that had nothing to do with the stuck resource it had just cleared.
func TestWitnessScriptAppliesTheCredentialOnEveryAttempt(t *testing.T) {
	s := witnessScript
	if strings.Contains(s, "Set-ClusterQuorum -FileShareWitness $desired") {
		t.Fatal("every file share apply must go through the splatted arguments, so the credential is never left off")
	}
	if strings.Count(s, "Set-ClusterQuorum @wargs") != 2 {
		t.Fatalf("want the credential-carrying apply on both attempts, got %d", strings.Count(s, "Set-ClusterQuorum @wargs"))
	}
	if !strings.Contains(s, "$env:BALLAST_WITNESS_USER") || strings.Contains(s, "password =") {
		t.Fatal("the credential must arrive through the environment, never the script text")
	}
}

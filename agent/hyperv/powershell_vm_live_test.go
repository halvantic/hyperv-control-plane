package hyperv

import (
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* GetVMState reads everything about one VM — power, memory, CPU, IPs, guest OS
   via KVP/XML, checkpoints, a Get-VHD per disk, a VLAN query per adapter,
   memory config and replication — at ~1.4s a time, and it ran per VM per pass.
   vmReconcile was 15s of a ~43s cycle on a two-VM host and scaled with VM count.

   The live half is the part that can actually change while a VM keeps running,
   and it comes from host-wide queries. These pin that it stays that way: the
   saving only exists if the work is flat in the number of VMs. */

func TestVMLiveScriptQueriesTheHostOncePerThingNotOncePerVM(t *testing.T) {
	s := psCode(vmLiveScript([]string{"Windows", "Linux", "App01"}))

	// Each of these is a module load and a CIM round trip. One apiece, hoisted
	// above the loop — a copy inside it would restore the per-VM cost.
	for cmd, want := range map[string]int{
		"Get-VMReplication":         1,
		"Get-VMNetworkAdapter":      1,
		"Get-VMSnapshot":            0, // checkpoints belong to the full read
		"Msvm_KvpExchangeComponent": 0, // guest OS via KVP is the expensive part
		"Get-VHD":                   0, // per-disk sizing belongs to the full read
	} {
		if got := strings.Count(s, cmd); got != want {
			t.Errorf("%s: found %d, want %d", cmd, got, want)
		}
	}
	if i, j := strings.Index(s, "Get-VMReplication"), strings.Index(s, "foreach ($vm in"); i > j {
		t.Error("the replication query must be hoisted above the per-VM loop")
	}
}

func TestVMLiveScriptCoversEveryNameAndQuotesThem(t *testing.T) {
	s := vmLiveScript([]string{"Windows", "It's Odd"})
	for _, n := range []string{"Windows", "It's Odd"} {
		if !strings.Contains(s, psQuote(n)) {
			t.Fatalf("every VM must be named and quoted through psQuote; %q missing from %s", n, s)
		}
	}
	// Names are matched case-insensitively: Hyper-V reports a VM's name in its
	// own casing, which need not match how desired state spells it.
	if !strings.Contains(s, ".ToLower()") {
		t.Error("name matching must be case-insensitive")
	}
}

// A VM the host does not have is simply absent from the result, and a host with
// no VMs must not produce a script that reports on every VM it can find.
func TestVMLiveScriptWithNoNamesMatchesNothing(t *testing.T) {
	s := vmLiveScript(nil)
	if !strings.Contains(s, "$names = @()") {
		t.Fatalf("an empty set must produce an empty name list, got: %s", s)
	}
}

// The live read carries what changes; the cached full read supplies the rest.
// Getting this backwards would either freeze the power state or wipe the config.
func TestMergeLiveOverlaysTheLiveFieldsOnly(t *testing.T) {
	cached := VMState{
		Exists: true, ID: "old-id", PowerState: "Off",
		GuestOS: "Windows Server 2022", GuestFQDN: "app01.lab",
		Observed:        &types.VMObserved{ProcessorCount: 4, Generation: 2},
		CPUUsagePercent: 0, UptimeSeconds: 0,
	}
	live := VMLive{
		Exists: true, ID: "live-id", PowerState: "Running",
		AssignedMemoryBytes: 4 << 30, CPUUsagePercent: 37, UptimeSeconds: 600,
		IPAddress: "192.168.1.90",
	}

	got := cached.MergeLive(live)

	// Live wins for everything it carries.
	if got.PowerState != "Running" || got.CPUUsagePercent != 37 || got.UptimeSeconds != 600 ||
		got.IPAddress != "192.168.1.90" || got.ID != "live-id" || got.AssignedMemoryBytes != 4<<30 {
		t.Fatalf("the live reading must win for the fields it carries, got %+v", got)
	}
	// And the cached read supplies what the live one deliberately skips.
	if got.GuestOS != "Windows Server 2022" || got.GuestFQDN != "app01.lab" || got.Observed == nil {
		t.Fatalf("config and guest identity must come from the cached full read, got %+v", got)
	}
}

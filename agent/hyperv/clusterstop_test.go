package hyperv

import (
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* A clustered stop takes the role's Virtual Machine resource offline and leaves
   its configuration resource up, so the VM stays registered, Off, on its owner —
   and a change held until power-off can land. Taking the whole group offline
   deregistered it from every node (prod's dc, 2026-11-18). */
func TestAClusteredStopTakesOnlyTheVMResourceOffline(t *testing.T) {
	s := (&PowerShell{}).vmPowerScript("dc", types.VMPowerOff)

	res := strings.Index(s, "Stop-ClusterResource -InputObject $vmRes")
	grp := strings.Index(s, "Stop-ClusterGroup -Name")
	if res < 0 {
		t.Fatal("a clustered stop must take the VM resource offline, not the group")
	}
	if grp >= 0 && grp < res {
		t.Fatal("the group may only be stopped as a fallback, after the VM-resource path")
	}
	if !strings.Contains(s, "[string]$_.ResourceType -eq 'Virtual Machine'") {
		t.Fatal("the resource must be found by its type, not its name")
	}
	// Already stopped is judged on the VM resource: with the configuration
	// resource up, the group is never plainly Offline, and judging on the group
	// would re-stop a stopped VM on every request.
	if !strings.Contains(s, "'Off' -eq 'Off' -and $vmRes -and [string]$vmRes.State -eq 'Offline'") {
		t.Fatal("an Offline VM resource must count as already stopped")
	}
}

// Start is unchanged: the whole group comes online, which brings the VM resource
// back whether the configuration resource was up or not.
func TestAClusteredStartStillStartsTheGroup(t *testing.T) {
	s := (&PowerShell{}).vmPowerScript("dc", types.VMPowerRunning)
	if !strings.Contains(s, "Start-ClusterGroup -Name") {
		t.Fatal("a clustered start must start the group")
	}
	if !strings.Contains(s, "'Running' -eq 'Off' -and $vmRes") {
		t.Fatal("the VM-resource stop must be gated to the Off request")
	}
}

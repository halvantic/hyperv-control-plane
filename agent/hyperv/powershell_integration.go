package hyperv

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Hyper-V's guest integration services, as declared state.

   Six per-VM settings with real consequences and nothing in the console that
   could see or set them. Shutdown off means a host drain has no way to stop the
   guest except by pulling its power; VSS off means no application-consistent
   backup; Key-Value Pair Exchange off means the guest's IP addresses and OS
   build stop reaching Ballast at all.

   NIL IS UNMANAGED, and that is the whole shape of this. Hyper-V's defaults
   differ per service — Guest Service Interface ships off, the others ship on —
   so a spec of plain bools would read as "disable all of these" for every VM
   nobody had ever been asked about, and the first save would quietly turn off
   backup and graceful shutdown across a fleet. Only an explicit value is ever
   applied.
*/

// serviceIDs map the schema's fields to Hyper-V's own service names, which are
// what Get-VMIntegrationService prints and what an operator will recognise.
var serviceIDs = []struct {
	Name string
	Get  func(*types.VMIntegrationServices) *bool
}{
	{"Guest Service Interface", func(s *types.VMIntegrationServices) *bool { return s.GuestServiceInterface }},
	{"Heartbeat", func(s *types.VMIntegrationServices) *bool { return s.Heartbeat }},
	{"Key-Value Pair Exchange", func(s *types.VMIntegrationServices) *bool { return s.KeyValuePairExchange }},
	{"Shutdown", func(s *types.VMIntegrationServices) *bool { return s.Shutdown }},
	{"Time Synchronization", func(s *types.VMIntegrationServices) *bool { return s.TimeSynchronisation }},
	{"VSS", func(s *types.VMIntegrationServices) *bool { return s.VSS }},
}

// IntegrationServiceState is one service as the host reports it.
type IntegrationServiceState struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

/*
integrationPlan works out which services differ from what is declared.

	Pure, and separate from the applying, because the whole risk here is
	deciding to change something that was never asked about. Returns the
	services to enable and to disable, both empty when everything already
	matches — which is every pass after the first.
*/
func integrationPlan(want *types.VMIntegrationServices, have []IntegrationServiceState) (enable, disable []string) {
	if want == nil {
		return nil, nil
	}
	actual := map[string]bool{}
	known := map[string]bool{}
	for _, s := range have {
		actual[strings.ToLower(s.Name)] = s.Enabled
		known[strings.ToLower(s.Name)] = true
	}
	for _, svc := range serviceIDs {
		v := svc.Get(want)
		if v == nil {
			continue // unmanaged: left exactly as it is
		}
		k := strings.ToLower(svc.Name)
		// A service the host does not report is not assumed absent OR present.
		// Older guests and Linux VMs expose different sets, and acting on a
		// service that is not there fails the whole pass for nothing.
		if !known[k] {
			continue
		}
		if actual[k] == *v {
			continue
		}
		if *v {
			enable = append(enable, svc.Name)
		} else {
			disable = append(disable, svc.Name)
		}
	}
	sort.Strings(enable)
	sort.Strings(disable)
	return enable, disable
}

// GetIntegrationServices reads what the VM's services are set to.
func (p *PowerShell) GetIntegrationServices(ctx context.Context, vm string) ([]IntegrationServiceState, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$out = @(Get-VMIntegrationService -VMName %[1]s -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; enabled = [bool]$_.Enabled }
})
ConvertTo-Json -Compress -Depth 3 -InputObject @($out)
`, psQuote(vm))
	out, err := p.run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("read integration services for %s: %w", vm, err)
	}
	var got []IntegrationServiceState
	if derr := decodeJSON(out, &got); derr != nil {
		return nil, fmt.Errorf("read integration services for %s: %w", vm, derr)
	}
	return got, nil
}

/*
EnsureIntegrationServices brings a VM's guest services to what is declared.

	Idempotent: reads first and writes only what differs, so a settled VM costs
	one query and no change. Nothing is touched that the spec does not name.

	These apply to a running VM without stopping it, which is why they are
	reconciled rather than queued behind a power cycle like Secure Boot.
*/
func (p *PowerShell) EnsureIntegrationServices(ctx context.Context, vm string, want *types.VMIntegrationServices) (Outcome, error) {
	if want == nil {
		return OutcomeUnchanged, nil
	}
	have, err := p.GetIntegrationServices(ctx, vm)
	if err != nil {
		return OutcomeUnchanged, err
	}
	enable, disable := integrationPlan(want, have)
	if len(enable) == 0 && len(disable) == 0 {
		return OutcomeUnchanged, nil
	}
	if err := p.run2(ctx, integrationScript(vm, enable, disable)); err != nil {
		return OutcomeUnchanged, fmt.Errorf("set integration services on %s: %w", vm, err)
	}
	return OutcomeUpdated, nil
}

func integrationScript(vm string, enable, disable []string) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	for _, n := range enable {
		fmt.Fprintf(&b, "Enable-VMIntegrationService -VMName %s -Name %s -ErrorAction Stop\n", psQuote(vm), psQuote(n))
	}
	for _, n := range disable {
		fmt.Fprintf(&b, "Disable-VMIntegrationService -VMName %s -Name %s -ErrorAction Stop\n", psQuote(vm), psQuote(n))
	}
	return b.String()
}

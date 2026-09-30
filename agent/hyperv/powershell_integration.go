package hyperv

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/halvantic/hyperv-control-plane/api/types"
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
	return p.ApplyIntegrationServices(ctx, vm, want, have)
}

/*
integrationBatchScript reads the named VMs' services in one process, one
query per named VM.

	Named rather than -VMName *, because the cmdlet's cost is per VM it reads
	even inside one process: ~286ms a VM for *, ~440ms a VM by name, on
	HVNEW06. * reads EVERY VM on the host, so one VM declaring services made
	the pass pay for all of them, and nearly every VM declares none. By name
	the cost follows what is declared.

	Each name in its own try, so a declared VM that is not on the host yet is
	just absent from the result and reads for itself, rather than failing the
	batch for the rest.
*/
func integrationBatchScript(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, psQuote(n))
	}
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$names = @(%[1]s)
$out = @{}
foreach ($n in $names) {
  try {
    foreach ($s in @(Get-VMIntegrationService -VMName $n -ErrorAction Stop)) {
      $k = ([string]$s.VMName).ToLower()
      if (-not $out.ContainsKey($k)) { $out[$k] = @() }
      $out[$k] += [pscustomobject]@{ name = [string]$s.Name; enabled = [bool]$s.Enabled }
    }
  } catch {}
}
$out | ConvertTo-Json -Compress -Depth 4
`, strings.Join(quoted, ","))
}

// GetIntegrationServicesBatch reads the named VMs' services in one invocation.
func (p *PowerShell) GetIntegrationServicesBatch(ctx context.Context, names []string) (map[string][]IntegrationServiceState, error) {
	if len(names) == 0 {
		return map[string][]IntegrationServiceState{}, nil
	}
	out, err := p.run(ctx, integrationBatchScript(names))
	if err != nil {
		return nil, fmt.Errorf("read integration services: %w", err)
	}
	var got map[string][]IntegrationServiceState
	if derr := decodeJSON(out, &got); derr != nil {
		return nil, fmt.Errorf("read integration services: %w", derr)
	}
	res := make(map[string][]IntegrationServiceState, len(got))
	for k, v := range got {
		res[strings.ToLower(k)] = v
	}
	return res, nil
}

// ApplyIntegrationServices writes what differs between want and have.
func (p *PowerShell) ApplyIntegrationServices(ctx context.Context, vm string, want *types.VMIntegrationServices, have []IntegrationServiceState) (Outcome, error) {
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

package hyperv

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

// This file is the PowerShell backing for day-0 host identity: reading the OS
// computer name / domain, renaming the host, and assigning a static IP to a
// physical adapter. As with every agent script, only single-quoted strings are
// used (powershell.exe -Command mangles embedded double quotes).

type identityObservation struct {
	ComputerName string `json:"computerName"`
	Domain       string `json:"domain"`
	PartOfDomain bool   `json:"partOfDomain"`
}

const identityScript = `
$ErrorActionPreference = 'Stop'
$cs = Get-CimInstance Win32_ComputerSystem
[pscustomobject]@{
  computerName = [string]$cs.Name
  domain       = [string]$cs.Domain
  partOfDomain = [bool]$cs.PartOfDomain
} | ConvertTo-Json -Compress
`

func (p *PowerShell) GetHostIdentity(ctx context.Context) (HostIdentity, error) {
	out, err := p.run(ctx, identityScript)
	if err != nil {
		return HostIdentity{}, fmt.Errorf("get host identity: %w", err)
	}
	var o identityObservation
	if err := decodeJSON(out, &o); err != nil {
		return HostIdentity{}, fmt.Errorf("get host identity: %w", err)
	}
	return HostIdentity{ComputerName: o.ComputerName, Domain: o.Domain, PartOfDomain: o.PartOfDomain}, nil
}

// RenameComputer renames the OS without rebooting (the reconciler reboots per
// RebootPolicy).
func (p *PowerShell) RenameComputer(ctx context.Context, newName string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Rename-Computer -NewName %s -Force | Out-Null", psQuote(newName))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("rename computer to %q: %w", newName, err)
	}
	return nil
}

// EnsureHostIP assigns the static IPv4 in spec to its physical adapter. It is a
// no-op when the address is already present; otherwise it clears the adapter's
// existing IPv4 and sets the desired address, gateway and DNS servers.
func (p *PowerShell) EnsureHostIP(ctx context.Context, spec types.PhysicalNICConfig) (Outcome, error) {
	ip, prefix, err := splitCIDR(spec.IPConfig.Address)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure host ip: %w", err)
	}
	adapter := psQuote(spec.AdapterName)
	gwLine := ""
	if spec.IPConfig.Gateway != "" {
		gwLine = fmt.Sprintf(" -DefaultGateway %s", psQuote(spec.IPConfig.Gateway))
	}
	dnsLine := ""
	if len(spec.IPConfig.DNSServers) > 0 {
		dnsLine = fmt.Sprintf("Set-DnsClientServerAddress -InterfaceAlias %s -ServerAddresses %s\n", adapter, psStringList(spec.IPConfig.DNSServers))
	}
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$cur = Get-NetIPAddress -InterfaceAlias %[1]s -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -eq %[2]s }
if ($cur) { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
Get-NetIPAddress -InterfaceAlias %[1]s -AddressFamily IPv4 -ErrorAction SilentlyContinue | Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue
New-NetIPAddress -InterfaceAlias %[1]s -IPAddress %[2]s -PrefixLength %[3]d%[4]s | Out-Null
%[5]s[pscustomobject]@{ changed = $true } | ConvertTo-Json -Compress
`, adapter, psQuote(ip), prefix, gwLine, dnsLine)

	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure host ip on %q: %w", spec.AdapterName, err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure host ip on %q: %w", spec.AdapterName, err)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// splitCIDR splits "10.0.0.5/24" into ip and prefix length.
func splitCIDR(cidr string) (string, int, error) {
	parts := strings.SplitN(cidr, "/", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("address %q is not CIDR (expected a.b.c.d/len)", cidr)
	}
	prefix, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid prefix in %q: %w", cidr, err)
	}
	return parts[0], prefix, nil
}

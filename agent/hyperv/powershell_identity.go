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

// JoinDomain joins the host to domain using the given account, without
// rebooting. The credentials are passed through environment variables to the
// child powershell.exe (never interpolated into the script or the command
// line) so they do not appear in process listings or logs.
func (p *PowerShell) JoinDomain(ctx context.Context, domain, ouPath, username, password string) error {
	ou := ""
	if ouPath != "" {
		ou = fmt.Sprintf(" -OUPath %s", psQuote(ouPath))
	}
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$sec = ConvertTo-SecureString $env:BALLAST_JOIN_PW -AsPlainText -Force
$cred = New-Object System.Management.Automation.PSCredential($env:BALLAST_JOIN_USER, $sec)
Add-Computer -DomainName %s -Credential $cred%s -Force | Out-Null
`, psQuote(domain), ou)

	env := []string{"BALLAST_JOIN_USER=" + username, "BALLAST_JOIN_PW=" + password}
	if err := p.runWithEnv(ctx, script, env); err != nil {
		return fmt.Errorf("join domain %q: %w", domain, err)
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
Get-NetIPAddress -IPAddress %[2]s -AddressFamily IPv4 -ErrorAction SilentlyContinue | Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue
Get-NetIPAddress -InterfaceAlias %[1]s -AddressFamily IPv4 -ErrorAction SilentlyContinue | Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue
Get-NetRoute -InterfaceAlias %[1]s -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Remove-NetRoute -Confirm:$false -ErrorAction SilentlyContinue
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

// EnsureVMHostPaths sets the host's default VM config and VHD directories,
// idempotently — only the ones that differ are changed. Distinct output tokens
// avoid the "unchanged" / "changed" substring trap.
func (p *PowerShell) EnsureVMHostPaths(ctx context.Context, vmPath, vhdPath string) (Outcome, error) {
	if vmPath == "" && vhdPath == "" {
		return OutcomeUnchanged, nil
	}
	var b strings.Builder
	b.WriteString("$ErrorActionPreference='Stop'; $h=Get-VMHost; $u=$false; ")
	// Create each target directory first — Set-VMHost rejects a path that does
	// not exist, and on a CSV the path is most reliably set to a real folder.
	if vmPath != "" {
		b.WriteString(fmt.Sprintf("if (-not (Test-Path %s)) { New-Item -ItemType Directory -Path %s -Force | Out-Null }; ", psQuote(vmPath), psQuote(vmPath)))
		b.WriteString(fmt.Sprintf("if ($h.VirtualMachinePath -ne %s) { Set-VMHost -VirtualMachinePath %s; $u=$true }; ", psQuote(vmPath), psQuote(vmPath)))
	}
	if vhdPath != "" {
		b.WriteString(fmt.Sprintf("if (-not (Test-Path %s)) { New-Item -ItemType Directory -Path %s -Force | Out-Null }; ", psQuote(vhdPath), psQuote(vhdPath)))
		b.WriteString(fmt.Sprintf("if ($h.VirtualHardDiskPath -ne %s) { Set-VMHost -VirtualHardDiskPath %s; $u=$true }; ", psQuote(vhdPath), psQuote(vhdPath)))
	}
	b.WriteString("if ($u) {'RESULT=UPDATED'} else {'RESULT=NOOP'}")
	out, err := p.run(ctx, b.String())
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("set vm host paths: %w", err)
	}
	if strings.Contains(string(out), "RESULT=UPDATED") {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// EnsureLiveMigration configures host live migration idempotently: enable/auth/
// concurrency via Set-VMHost, and (when networks are given) restrict migration
// to those subnets — exactly the fix for a bad IPv6 migration listener.
func (p *PowerShell) EnsureLiveMigration(ctx context.Context, spec types.LiveMigrationSpec) (Outcome, error) {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference='Stop'; $u=$false; $h=Get-VMHost; ")
	if spec.Enabled {
		b.WriteString("if (-not $h.VirtualMachineMigrationEnabled) { Enable-VMMigration; $u=$true }; ")
	} else {
		b.WriteString("if ($h.VirtualMachineMigrationEnabled) { Disable-VMMigration; $u=$true }; ")
	}
	if spec.AuthenticationType != "" {
		b.WriteString(fmt.Sprintf("if ($h.VirtualMachineMigrationAuthenticationType -ne %s) { Set-VMHost -VirtualMachineMigrationAuthenticationType %s; $u=$true }; ",
			psQuote(spec.AuthenticationType), psQuote(spec.AuthenticationType)))
	}
	if spec.MaxConcurrent > 0 {
		b.WriteString(fmt.Sprintf("if ($h.MaximumVirtualMachineMigrations -ne %d) { Set-VMHost -MaximumVirtualMachineMigrations %d; $u=$true }; ", spec.MaxConcurrent, spec.MaxConcurrent))
	}
	if len(spec.Networks) > 0 {
		// Compare desired vs current migration subnets by sorted join (not
		// Compare-Object, which rejects a null DifferenceObject when no migration
		// networks exist yet). Setting UseAnyNetworkForMigration=$false without a
		// configured network leaves migration with nowhere to run, so the add must
		// be robust on a host that has none.
		b.WriteString("if ($h.UseAnyNetworkForMigration) { Set-VMHost -UseAnyNetworkForMigration $false; $u=$true }; ")
		b.WriteString(fmt.Sprintf("$want=@(%s); $cur=@(Get-VMMigrationNetwork -ErrorAction SilentlyContinue | ForEach-Object { $_.Subnet }); "+
			"if ((($want | Sort-Object) -join ',') -ne (($cur | Sort-Object) -join ',')) { Get-VMMigrationNetwork -ErrorAction SilentlyContinue | Remove-VMMigrationNetwork -ErrorAction SilentlyContinue; "+
			"foreach ($n in $want) { Add-VMMigrationNetwork -Subnet $n | Out-Null }; $u=$true }; ", psStringList(spec.Networks)))
	}
	b.WriteString("if ($u) {'RESULT=UPDATED'} else {'RESULT=NOOP'}")
	out, err := p.run(ctx, b.String())
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("configure live migration: %w", err)
	}
	if strings.Contains(string(out), "RESULT=UPDATED") {
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

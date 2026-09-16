package hyperv

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/halvantic/hyperv-control-plane/api/types"
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

type computerOUObservation struct {
	DistinguishedName string `json:"distinguishedName"`
}

// computerOUScript looks up this host's own AD computer object and returns
// its distinguishedName, from which the caller derives the OU (everything
// after the leading CN=<name>,).
//
// Uses [adsisearcher], not the ActiveDirectory module, for the same reason
// EnsureMigrationDelegation avoids it in powershell_cluster.go: it does not
// depend on AD Web Services (port 9389), which is often absent on a small
// lab DC. Unlike that function this is read-only and does not need to
// locate a specific DC by hand -- [adsisearcher]'s default SearchRoot
// resolves the domain the ordinary way (the same DC-locator path DNS/Kerberos
// already need to work at all), which is enough for a best-effort diagnostic
// read that costs a stale reading, not a fault, when it fails.
const computerOUScript = `
$ErrorActionPreference = 'Stop'
$searcher = [adsisearcher]"(&(objectClass=computer)(sAMAccountName=$env:COMPUTERNAME$))"
$searcher.PropertiesToLoad.Add('distinguishedName') | Out-Null
$result = $searcher.FindOne()
if (-not $result) { throw "computer object for $env:COMPUTERNAME not found in AD" }
[pscustomobject]@{
  distinguishedName = [string]$result.Properties['distinguishedname'][0]
} | ConvertTo-Json -Compress
`

// ouFromDN strips the leading CN=<name>, off a computer object's
// distinguishedName, leaving the OU (or container) it sits in -- e.g.
// "CN=HV01,OU=BallastHosts,DC=ballast,DC=local" becomes
// "OU=BallastHosts,DC=ballast,DC=local". Returns "" if dn has no comma (not
// a valid computer-object DN) so the caller can tell "parsed empty" apart
// from "genuinely at the domain root", which is a comma-free DC=... string
// and never reaches this function's empty-string return.
//
// Deliberately simple: splits on the first unescaped-looking comma. A CN
// containing a literal comma (escaped in LDAP as "\,") is not handled --
// computer names cannot contain a comma at all, so this does not arise for
// what this function is actually used on.
func ouFromDN(dn string) string {
	idx := strings.Index(dn, ",")
	if idx < 0 || idx == len(dn)-1 {
		return ""
	}
	return dn[idx+1:]
}

func (p *PowerShell) GetComputerOU(ctx context.Context) (string, error) {
	out, err := p.run(ctx, computerOUScript)
	if err != nil {
		return "", fmt.Errorf("get computer OU: %w", err)
	}
	var o computerOUObservation
	if err := decodeJSON(out, &o); err != nil {
		return "", fmt.Errorf("get computer OU: %w", err)
	}
	ou := ouFromDN(o.DistinguishedName)
	if ou == "" {
		return "", fmt.Errorf("get computer OU: could not parse an OU from distinguishedName %q", o.DistinguishedName)
	}
	return ou, nil
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
// rebooting.
//
// The credential travels over the child powershell.exe's STDIN, one line
// each for username then password -- never interpolated into the script
// text, never a command-line argument, and deliberately not an environment
// variable either. An environment variable set on a child process is
// readable by any local administrator inspecting that process (its
// environment block is not privileged the way a memory-protected secret
// would be) and is captured whole in a crash dump of either process; stdin
// is consumed once by the reader on the other end and is never something
// the OS keeps around for a third party to inspect afterwards.
//
// The byte slice actually written to the pipe is zeroed once the process
// exits, best-effort: this reduces how long the plaintext sits in Ballast's
// own memory after use, but cannot reach (and does not attempt to reach)
// the original username/password strings this function was called with --
// Go strings are immutable, so the caller's copies are cleared only by the
// garbage collector's own schedule, same as any other Go string. This is a
// real limitation, not a gap in this function specifically.
func (p *PowerShell) JoinDomain(ctx context.Context, domain, ouPath, username, password string) error {
	ou := ""
	if ouPath != "" {
		ou = fmt.Sprintf(" -OUPath %s", psQuote(ouPath))
	}
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$joinUser = [Console]::In.ReadLine()
$joinPass = [Console]::In.ReadLine()
$sec = ConvertTo-SecureString $joinPass -AsPlainText -Force
$cred = New-Object System.Management.Automation.PSCredential($joinUser, $sec)
Add-Computer -DomainName %s -Credential $cred%s -Force | Out-Null
`, psQuote(domain), ou)

	stdin := []byte(username + "\n" + password + "\n")
	err := p.runWithStdin(ctx, script, stdin)
	for i := range stdin {
		stdin[i] = 0
	}
	if err != nil {
		// err can only carry the child's stdout/stderr (see psFailureDetail) --
		// neither ever contains what was written to stdin, so this cannot leak
		// the credential into a log line or an error message the operator sees.
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
	b.WriteString("$ErrorActionPreference='Stop'; $h=Get-VMHost; $u=$false; $skip=@(); ")
	// Best-effort per path: only set it once the directory exists. Try to create
	// it, but if that fails — e.g. the path targets a CSV (C:\ClusterStorage\...)
	// that has not been provisioned yet, where you cannot create a folder at the
	// namespace root — skip that path instead of failing the whole reconcile. The
	// host keeps its current default until the CSV exists.
	setPath := func(prop, flag, path string) {
		q := psQuote(path)
		b.WriteString(fmt.Sprintf("$ok=Test-Path %s; if (-not $ok) { try { New-Item -ItemType Directory -Path %s -Force -ErrorAction Stop | Out-Null; $ok=$true } catch { $ok=$false } }; ", q, q))
		b.WriteString(fmt.Sprintf("if ($ok) { if ($h.%s -ne %s) { try { Set-VMHost -%s %s -ErrorAction Stop; $u=$true } catch { $skip += %s } } } else { $skip += %s }; ",
			prop, q, flag, q, q, q))
	}
	if vmPath != "" {
		setPath("VirtualMachinePath", "VirtualMachinePath", vmPath)
	}
	if vhdPath != "" {
		setPath("VirtualHardDiskPath", "VirtualHardDiskPath", vhdPath)
	}
	b.WriteString("if ($skip.Count) {'RESULT=SKIP ' + ($skip -join ',')} elseif ($u) {'RESULT=UPDATED'} else {'RESULT=NOOP'}")
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

type privilegeCheckObservation struct {
	IsLocalAdmin           bool   `json:"isLocalAdmin"`
	ClusterApplicable      bool   `json:"clusterApplicable"`
	ClusterAccessOK        bool   `json:"clusterAccessOK"`
	ADDelegationApplicable bool   `json:"adDelegationApplicable"`
	ADDelegationOK         bool   `json:"adDelegationOK"`
	ADDelegationErr        string `json:"adDelegationErr"`
}

// checkPrivilegesScript is one PowerShell call answering all three questions
// CheckPrivileges asks, so a registration-time check costs one process spawn
// rather than three.
//
// Local admin: WindowsPrincipal.IsInRole, unambiguous.
//
// Cluster access: only asked when the ClusSvc service is actually running
// (this host is a member) — Get-Cluster either succeeds or throws, no
// heuristic involved.
//
// AD delegation: the one genuinely approximate check here, and worth being
// honest about. It reads the OU's own ACL (DirectoryEntry.ObjectSecurity, the
// same System.DirectoryServices path GetComputerOU and
// EnsureMigrationDelegation already use, so no new dependency) and looks for
// an ALLOW rule naming the running identity or one of its groups that grants
// WriteProperty, CreateChild AND DeleteChild. It does NOT verify those rights
// are scoped to the specific msDS-AllowedToDelegateTo property or the
// "computer" object class the way provision-ballast-agent-account.ps1's
// dsacls calls actually scope them — doing that precisely means matching
// ActiveDirectoryAccessRule.ObjectType against the exact schema GUIDs, which
// needs a schema lookup this check does not perform. A broader ALLOW rule
// covering the same identity would read as sufficient here even if the real
// grant is narrower or broader than what provisioning actually set up. That
// makes this check meaningfully better than nothing (it will catch "no
// delegation at all", the common case) without being a substitute for
// actually trying the operation — which is exactly why a shortfall here is
// reported, never acted on.
const checkPrivilegesScript = `
$ErrorActionPreference = 'Stop'
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)

$clusterApplicable = $false
$clusterOK = $false
try {
  $svc = Get-Service -Name ClusSvc -ErrorAction SilentlyContinue
  if ($svc -and $svc.Status -eq 'Running') {
    $clusterApplicable = $true
    try { Get-Cluster -ErrorAction Stop | Out-Null; $clusterOK = $true } catch { $clusterOK = $false }
  }
} catch {}

$adApplicable = $false
$adOK = $false
$adErr = ''
$ouDn = %s
if ($ouDn) {
  $adApplicable = $true
  try {
    $de = New-Object System.DirectoryServices.DirectoryEntry("LDAP://$ouDn")
    $sd = $de.ObjectSecurity
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $sids = @($id.User.Value) + @($id.Groups | ForEach-Object { $_.Value })
    $rules = $sd.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])
    $hasWrite = $false
    $hasCreate = $false
    $hasDelete = $false
    foreach ($r in $rules) {
      if ($r.AccessControlType -ne 'Allow') { continue }
      if ($sids -notcontains $r.IdentityReference.Value) { continue }
      $rights = $r.ActiveDirectoryRights
      if ($rights -band [System.DirectoryServices.ActiveDirectoryRights]::WriteProperty) { $hasWrite = $true }
      if ($rights -band [System.DirectoryServices.ActiveDirectoryRights]::CreateChild) { $hasCreate = $true }
      if ($rights -band [System.DirectoryServices.ActiveDirectoryRights]::DeleteChild) { $hasDelete = $true }
    }
    $adOK = $hasWrite -and $hasCreate -and $hasDelete
  } catch {
    $adErr = $_.Exception.Message
  }
}

[pscustomobject]@{
  isLocalAdmin            = [bool]$isAdmin
  clusterApplicable       = [bool]$clusterApplicable
  clusterAccessOK         = [bool]$clusterOK
  adDelegationApplicable  = [bool]$adApplicable
  adDelegationOK          = [bool]$adOK
  adDelegationErr         = [string]$adErr
} | ConvertTo-Json -Compress
`

func (p *PowerShell) CheckPrivileges(ctx context.Context, ouDN string) (PrivilegeCheck, error) {
	script := fmt.Sprintf(checkPrivilegesScript, psQuote(ouDN))
	out, err := p.run(ctx, script)
	if err != nil {
		return PrivilegeCheck{}, fmt.Errorf("check privileges: %w", err)
	}
	var o privilegeCheckObservation
	if err := decodeJSON(out, &o); err != nil {
		return PrivilegeCheck{}, fmt.Errorf("check privileges: %w", err)
	}
	return PrivilegeCheck{
		IsLocalAdmin:           o.IsLocalAdmin,
		ClusterApplicable:      o.ClusterApplicable,
		ClusterAccessOK:        o.ClusterAccessOK,
		ADDelegationApplicable: o.ADDelegationApplicable,
		ADDelegationOK:         o.ADDelegationOK,
		ADDelegationErr:        o.ADDelegationErr,
	}, nil
}

package hyperv

import (
	"context"
	"fmt"
)

// Guest configuration runs inside the VM's guest OS via PowerShell Direct
// (Invoke-Command -VMName), which reaches the guest over VMBus — no guest network
// is required and the session survives the guest's own networking changes. It
// needs a guest-local administrator credential to authenticate into the guest;
// domain join additionally needs a domain credential to authorise Add-Computer.
// All credentials are passed via environment variables to the child powershell
// (never the command line or script text), and forwarded into the guest scriptblock
// as Invoke-Command arguments so they never appear in logs or process listings.

// GuestJoinDomain joins the VM's guest OS to domain, then reboots the guest. The
// guest credential authenticates PowerShell Direct; the domain credential
// authorises the join. ouPath is optional.
func (p *PowerShell) GuestJoinDomain(ctx context.Context, vmName, domain, ouPath, guestUser, guestPass, domainUser, domainPass string) error {
	script := `
$ErrorActionPreference = 'Stop'
$gsec = ConvertTo-SecureString $env:BALLAST_GUEST_PW -AsPlainText -Force
$gcred = New-Object System.Management.Automation.PSCredential($env:BALLAST_GUEST_USER, $gsec)
Invoke-Command -VMName $env:BALLAST_GUEST_VM -Credential $gcred -ArgumentList $env:BALLAST_DOM_USER,$env:BALLAST_DOM_PW,$env:BALLAST_GUEST_DOMAIN,$env:BALLAST_GUEST_OU -ScriptBlock {
  param($du, $dp, $dom, $ou)
  $ds = ConvertTo-SecureString $dp -AsPlainText -Force
  $dc = New-Object System.Management.Automation.PSCredential($du, $ds)

  # "Could not be contacted" from Add-Computer is almost always a domain-locator
  # failure: the guest must resolve the domain's SRV records and reach a DC. Probe
  # those first so the reported error names the real cause instead of being vague.
  $dnsServers = (Get-DnsClientServerAddress -AddressFamily IPv4 |
    Where-Object { $_.ServerAddresses } | ForEach-Object { $_.ServerAddresses }) -join ','
  $srvName = '_ldap._tcp.dc._msdcs.' + $dom
  try {
    $srv = Resolve-DnsName -Name $srvName -Type SRV -DnsOnly -ErrorAction Stop
  } catch {
    throw "domain locator failed: cannot resolve $srvName via DNS server(s) [$dnsServers]. " +
          "The guest's DNS server must be an AD DNS server for '$dom' and the guest must have an L2 path to it. ($($_.Exception.Message))"
  }
  $dcHost = ($srv | Where-Object { $_.Type -eq 'SRV' -and $_.NameTarget } | Select-Object -First 1).NameTarget
  if (-not $dcHost) { throw "domain locator failed: no SRV target returned for $srvName via DNS server(s) [$dnsServers]." }
  if (-not (Test-NetConnection -ComputerName $dcHost -Port 389 -InformationLevel Quiet)) {
    throw "domain controller $dcHost resolved but is unreachable on LDAP/389 from the guest (check VLAN/firewall between the guest's dvport and the DC)."
  }

  if ($ou) { Add-Computer -DomainName $dom -Credential $dc -OUPath $ou -Force }
  else { Add-Computer -DomainName $dom -Credential $dc -Force }
  Restart-Computer -Force
}
`
	env := []string{
		"BALLAST_GUEST_VM=" + vmName,
		"BALLAST_GUEST_USER=" + guestUser,
		"BALLAST_GUEST_PW=" + guestPass,
		"BALLAST_DOM_USER=" + domainUser,
		"BALLAST_DOM_PW=" + domainPass,
		"BALLAST_GUEST_DOMAIN=" + domain,
		"BALLAST_GUEST_OU=" + ouPath,
	}
	if err := p.runWithEnv(ctx, script, env); err != nil {
		return fmt.Errorf("guest join domain %q on %q: %w", domain, vmName, err)
	}
	return nil
}

// GuestSetIP sets a static IPv4 on the guest's network adapter via PowerShell
// Direct. addr is CIDR (e.g. 192.168.1.50/24); iface is the adapter name (empty
// picks the first connected adapter); gateway and dns (comma-separated) are
// optional.
func (p *PowerShell) GuestSetIP(ctx context.Context, vmName, iface, addr, gateway, dns, guestUser, guestPass string) error {
	script := `
$ErrorActionPreference = 'Stop'
$gsec = ConvertTo-SecureString $env:BALLAST_GUEST_PW -AsPlainText -Force
$gcred = New-Object System.Management.Automation.PSCredential($env:BALLAST_GUEST_USER, $gsec)
Invoke-Command -VMName $env:BALLAST_GUEST_VM -Credential $gcred -ArgumentList $env:BALLAST_IP_IFACE,$env:BALLAST_IP_ADDR,$env:BALLAST_IP_GW,$env:BALLAST_IP_DNS -ScriptBlock {
  param($iface, $addr, $gw, $dns)
  $parts = $addr.Split('/')
  $ip = $parts[0]
  $prefix = [int]$parts[1]
  if (-not $iface) { $iface = (Get-NetAdapter | Where-Object { $_.Status -eq 'Up' } | Select-Object -First 1).Name }
  if (-not $iface) { throw 'no connected network adapter found in guest' }
  Get-NetIPAddress -InterfaceAlias $iface -AddressFamily IPv4 -ErrorAction SilentlyContinue | Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue
  Remove-NetRoute -InterfaceAlias $iface -DestinationPrefix ('0.0.0.0/' + '0') -Confirm:$false -ErrorAction SilentlyContinue
  Set-NetIPInterface -InterfaceAlias $iface -Dhcp Disabled -ErrorAction SilentlyContinue
  if ($gw) { New-NetIPAddress -InterfaceAlias $iface -IPAddress $ip -PrefixLength $prefix -DefaultGateway $gw | Out-Null }
  else { New-NetIPAddress -InterfaceAlias $iface -IPAddress $ip -PrefixLength $prefix | Out-Null }
  if ($dns) { Set-DnsClientServerAddress -InterfaceAlias $iface -ServerAddresses ($dns.Split(',')) }
}
`
	env := []string{
		"BALLAST_GUEST_VM=" + vmName,
		"BALLAST_GUEST_USER=" + guestUser,
		"BALLAST_GUEST_PW=" + guestPass,
		"BALLAST_IP_IFACE=" + iface,
		"BALLAST_IP_ADDR=" + addr,
		"BALLAST_IP_GW=" + gateway,
		"BALLAST_IP_DNS=" + dns,
	}
	if err := p.runWithEnv(ctx, script, env); err != nil {
		return fmt.Errorf("guest set ip on %q: %w", vmName, err)
	}
	return nil
}

package hyperv

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// ISCSIState is one node's observed iSCSI connection state, mirroring
// types.ISCSIStatus without the schema dependency in the hyperv layer.
type ISCSIState struct {
	// InitiatorIQN is what the array grants the LUN to — the first thing anyone
	// configuring iSCSI needs, and knowable only on the host.
	InitiatorIQN   string
	ServiceRunning bool
	Portals        []string
	Sessions       []ISCSISessionState
	MPIOInstalled  bool
	// MPIOClaimed reports that MPIO is actually claiming iSCSI devices. Installed
	// is not the same thing: the feature can be present with no bus type claimed,
	// which protects nothing while looking configured.
	MPIOClaimed bool
	// MPIOEffective is multipath actually protecting this host: claiming iSCSI AND
	// the bus driver running, which only happens after the restart. OBSERVED on the
	// host rather than derived here — deriving it from what a pass happened to do
	// reported a host that had never restarted as protected.
	MPIOEffective bool
	Disks         []ISCSIDiskState
	// RebootRequired is set when installing MPIO asked for one. Multipath claim
	// does not take effect until then, so a cluster is NOT safe to put multipath
	// storage under until the node has restarted.
	RebootRequired bool
	Message        string
}

type ISCSISessionState struct {
	TargetIQN  string
	Connected  bool
	Persistent bool
	Paths      int
}

type ISCSIDiskState struct {
	SerialNumber string
	Number       int
	SizeBytes    uint64
	TargetIQN    string
	LUN          int
	Clustered    bool
	Offline      bool
	// Contents is what the LUN already carries, and ContentsKnown whether anyone
	// looked. See the probe in the script: blank and unprobed read identically as
	// "" and mean opposite things when the next step is a format.
	Contents      string
	ContentsKnown bool
}

// iscsiScript connects this node to the declared portals and targets and reports
// what it can see. It is deliberately ADDITIVE: it registers portals and logs in,
// and never disconnects a session or removes a portal.
//
// Removing an iSCSI login is not the inverse of adding one. A session that is
// carrying a clustered disk takes the disk away with it, and Ballast cannot tell
// from the spec alone whether a target it no longer lists is one the operator
// removed or one another application on the host depends on. Tearing down shared
// storage as a side effect of an edit is not a risk worth taking for tidiness, so
// removal is a separate, explicit act.
//
// Logins are made PERSISTENT. A non-persistent session works perfectly until the
// node reboots and then simply does not come back, which on a cluster member
// means its disks do not arrive and the roles it owned fail over — with nothing
// anywhere saying why.
func iscsiScript(spec types.ISCSIStorageSpec, wantMPIO, shared bool, storageAddresses []string) string {
	var b strings.Builder
	b.WriteString(`$ErrorActionPreference = 'Stop'
$out = [ordered]@{ initiatorIQN=''; serviceRunning=$false; portals=@(); sessions=@(); mpioInstalled=$false; mpioClaimed=$false; mpioEffective=$false; disks=@(); rebootRequired=$false; message='' }
$pathErrs = @()
$persistErrs = @{}   # keyed by target IQN, resolved against the END state
$changed = $false

# WHERE THE TIME WENT, inside this script.
#
# The pass timer names "iscsi" as the slow step and stops there. On Secondary,
# 2026-08-25, that read "clusterReconcile 4m15s (iscsi 4m3s)" on both members —
# enough to know the cluster never formed because the pass was cut off at five
# minutes, and not enough to know which call was blocking. Several iSCSI cmdlets
# can hang for minutes against a portal that answers on 3260 but does not
# complete the operation, and a TCP probe cannot tell them apart.
#
# So the script times itself. Guessing which cmdlet is slow has been wrong
# repeatedly today; asking it is cheap.
$phases = [ordered]@{}
$swTotal = [System.Diagnostics.Stopwatch]::StartNew()
$swPhase = [System.Diagnostics.Stopwatch]::StartNew()
function Mark-Phase([string]$n) {
  $swPhase.Stop()
  if ($phases.Contains($n)) { $phases[$n] = [int]$phases[$n] + [int]$swPhase.ElapsedMilliseconds }
  else { $phases[$n] = [int]$swPhase.ElapsedMilliseconds }
  $swPhase.Restart()
}

# The initiator service is set to start ON DEMAND by default on Windows Server,
# which is not enough for a cluster member: the logins have to be re-established
# at boot before the cluster asks for its disks. Automatic is a prerequisite for
# persistent sessions meaning anything.
$svc = Get-Service MSiSCSI -ErrorAction SilentlyContinue
if ($svc) {
  if ($svc.StartType -ne 'Automatic') { Set-Service MSiSCSI -StartupType Automatic; $changed = $true }
  if ($svc.Status -ne 'Running') { Start-Service MSiSCSI; $changed = $true; Start-Sleep -Seconds 2 }
  $out.serviceRunning = ((Get-Service MSiSCSI).Status -eq 'Running')
} else {
  $out.message = 'the Microsoft iSCSI Initiator service (MSiSCSI) is not present on this host'
}

# The initiator name, read AFTER the service is up because Get-InitiatorPort
# returns nothing without it. The registry holds it either way, so fall back
# there rather than reporting nothing: this is the value an operator needs
# BEFORE anything can work, so it must survive the case where nothing works yet.

try { $out.initiatorIQN = [string]@(Get-InitiatorPort -ErrorAction Stop | Where-Object { $_.ConnectionType -eq 'iSCSI' })[0].NodeAddress } catch {}
if (-not $out.initiatorIQN) {
  try { $out.initiatorIQN = [string](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\iSCSI' -ErrorAction Stop).NodeName } catch {}
}
`)

	// On a CLUSTER MEMBER a newly arrived shared LUN must not be brought online
	// automatically.
	//
	// Windows' default new-disk policy mounts a LUN read/write on every node that
	// can see it, and a shared disk online on two nodes at once is the state
	// clustering exists to prevent — so Get-ClusterAvailableDisk does not offer it,
	// and Add-ClusterDisk silently has nothing to add. On the rig both DRCluster
	// members reported both LUNs online simultaneously, and the adoption failed
	// with the cluster declining to take a disk that was in front of it.
	//
	// A standalone host is the opposite case: its LUN SHOULD come online, because
	// it is provisioned there like any other local disk. Hence the flag.
	if shared {
		b.WriteString(`
try {
  $pol = [string](Get-StorageSetting -ErrorAction SilentlyContinue).NewDiskPolicy
  if ($pol -and $pol -ne 'OfflineShared' -and $pol -ne 'OfflineAll') {
    Set-StorageSetting -NewDiskPolicy OfflineShared -ErrorAction Stop
    $changed = $true
  }
} catch {}
`)
	}

	// MPIO before any login: claiming multipath devices after sessions already
	// exist leaves the duplicates already presented.
	if wantMPIO {
		b.WriteString(`
# MPIO must be in place BEFORE the second path logs in. Claim it afterwards and
# the duplicate disks Windows has already presented stay presented, which is the
# state a cluster must never see: the same LUN as two devices it believes are
# unrelated.
$mp = Get-WindowsFeature -Name Multipath-IO -ErrorAction SilentlyContinue
if ($mp -and -not $mp.Installed) {
  $r = Install-WindowsFeature -Name Multipath-IO
  $changed = $true
  if ($r.RestartNeeded -ne 'No') { $out.rebootRequired = $true }
}
$out.mpioInstalled = [bool](Get-WindowsFeature -Name Multipath-IO -ErrorAction SilentlyContinue).Installed

# Whether MPIO is claiming iSCSI comes from the automatic-claim SETTINGS.
#
# Get-MSDSMSupportedHW lists vendor/product pairs and carries no BusType at all,
# so filtering it on one matched nothing on every host, for ever: the claim read
# back as absent however many times it had been enabled, mpioEffective could
# never become true, and no number of restarts cleared "restart required".
# Observed on the rig 2026-08-09 on HVNEW05, restarted and still asking.
function Read-ISCSIClaim {
  try {
    $s = Get-MSDSMAutomaticClaimSettings -ErrorAction Stop
    if ($null -eq $s) { return $false }
    # The cmdlet returns a hashtable keyed by bus type on current builds; tolerate
    # a plain object with a property, because being wrong about this silently is
    # exactly what happened last time.
    if ($s -is [System.Collections.IDictionary]) {
      foreach ($k in $s.Keys) { if ([string]$k -eq 'iSCSI') { return [bool]$s[$k] } }
      return $false
    }
    return [bool]$s.iSCSI
  } catch { return $false }
}

# Whether multipath is IN EFFECT is observed, never inferred from what this pass
# happened to do.
#
# rebootRequired only ever described THIS pass: a pass that installed nothing and
# claimed nothing left it false, so a host that had never restarted since MPIO was
# installed reported multipath as in effect — which HVNEW04 did, on one path, with
# three portals declared. The driver either loaded at boot or it did not, and that
# is a fact about the host that any pass can read.
function Read-MPIOEffective {
  if (-not (Read-ISCSIClaim)) { return $false }
  # The MPIO bus driver is what actually coalesces the paths. Installed but not
  # restarted into means the service exists and is not running.
  $svc = Get-Service -Name 'mpio' -ErrorAction SilentlyContinue
  if (-not $svc -or [string]$svc.Status -ne 'Running') { return $false }
  return $true
}

if ($out.mpioInstalled) {
  $out.mpioClaimed = Read-ISCSIClaim
  if (-not $out.mpioClaimed) {
    # Attempted in the SAME pass as the install, not deferred until after the
    # restart. Enabling the claim needs a restart of its own, so deferring it
    # costs two restarts where one would do — and the node spends the gap on a
    # single path with its storage unprotected.
    try {
      Enable-MSDSMAutomaticClaim -BusType iSCSI -ErrorAction Stop
      $changed = $true
      $out.mpioClaimed = Read-ISCSIClaim
      # The setting takes effect at boot, so a claim enabled now still leaves the
      # host unprotected until it restarts.
      $out.rebootRequired = $true
    } catch {
      # The MSDSM cmdlets arrive with the feature and may not be usable until it
      # has been restarted into. That is not a failure — the next pass claims it.
    }
  }
}
`)
	} else {
		b.WriteString("$out.mpioInstalled = [bool](Get-WindowsFeature -Name Multipath-IO -ErrorAction SilentlyContinue).Installed\n")
	}

	// Portals. New-IscsiTargetPortal on an existing portal throws, so check.
	b.WriteString("\n$portals = @(" + psStringList(spec.Portals) + ")\n")

	// Which local address each portal is reached through, resolved here rather
	// than on the host: only the desired state knows which vNICs are for storage,
	// and the host cannot tell a storage vNIC from a management one.
	//
	// Used for BOTH the discovery portal and the login. Discovery was left
	// unbound on the argument that a pinned portal keeps a source address the
	// host may lose — but that failure is already detected below and repaired by
	// the RepairISCSIPortals job, while leaving it unbound has a failure of its
	// own that nothing catches. On a host with two storage subnets the routing
	// table answers with ONE interface, so discovery to the second portal leaves
	// through the first vNIC. The array is then asked about a target it does not
	// advertise on that path, and answers "the target name is not found or is
	// marked as hidden from login" — pointing squarely at the array, where
	// nothing is wrong.
	//
	// HVNEW01, 2026-08-24: it failed on 10.0.61.52 while HVNEW03 succeeded on the
	// same portal, and setting Initiator IP to 10.0.61.71 by hand in iscsicpl
	// fixed it immediately.
	b.WriteString("$initiatorFor = @{}\n")
	for _, portal := range spec.Portals {
		addr := types.InitiatorFor(portal, storageAddresses)
		if addr == "" {
			continue
		}
		host := portal
		if h, _, err := net.SplitHostPort(strings.TrimSpace(portal)); err == nil {
			host = h
		}
		fmt.Fprintf(&b, "$initiatorFor[%s] = %s\n", psQuote(host), psQuote(addr))
	}

	// DISCOVERY IS TRIED WITHOUT CHAP FIRST.
	//
	// iSCSI has two session types and they authenticate INDEPENDENTLY: the
	// discovery (SendTargets) session, and the normal session that logs in to a
	// target. An array can require CHAP on one, both, or neither.
	//
	// Synology — and most arrays — configure CHAP PER TARGET, so it applies to the
	// normal session. Discovery is unauthenticated and masked by the target's
	// allowed-initiator list instead. Sending CHAP to a discovery portal that does
	// not expect it is not ignored: the array rejects it, and
	// New-IscsiTargetPortal fails with "Authentication Failure". That is what all
	// three members of Primary1 hit on 2026-08-25 — every portal refused, no
	// targets discovered, nothing logged in.
	//
	// This code previously sent CHAP on discovery unconditionally, and the reason
	// recorded here was wrong. The evidence was an operator connecting a host by
	// hand through iscsicpl with CHAP filled in — but that was the CONNECT dialog,
	// which is the normal-session login, and the thing that actually fixed it was
	// the initiator-address binding beside it. The operator later added a
	// discovery portal with NO password and it worked immediately, which is the
	// direct disproof.
	//
	// So: no CHAP on discovery, and fall back to CHAP only if the array refuses —
	// an array that genuinely requires discovery CHAP then still works, and one
	// that does not is never handed a credential it will reject.
	if spec.CredentialSecret != "" {
		portalAuth := "ONEWAYCHAP"
		if spec.MutualCHAP {
			portalAuth = "MUTUALCHAP"
		}
		auth := "@{ AuthenticationType = '" + portalAuth + "'; " +
			"ChapUsername = $env:BALLAST_CHAP_USER; ChapSecret = $env:BALLAST_CHAP_SECRET }\n"
		// DiscoveryAndTarget presents the credential on the FIRST attempt, for an
		// array that requires it or a policy that mandates it. Anything else holds
		// it back — see CHAPScope.
		if spec.CHAPScope.AuthenticatesDiscovery() {
			b.WriteString("$portalAuthFirst = " + auth)
			b.WriteString("$portalAuthFallback = @{}\n")
		} else {
			b.WriteString("$portalAuthFirst = @{}\n")
			// TargetOnly is a statement that the array does not want CHAP on
			// discovery, so retrying with it anyway would be the console overruling
			// the operator. Only Auto earns the credential by being refused first.
			if spec.CHAPScope.MayRetryDiscoveryWithCHAP() {
				b.WriteString("$portalAuthFallback = " + auth)
			} else {
				b.WriteString("$portalAuthFallback = @{}\n")
			}
		}
		// Whether a credential was supplied, for the empty-discovery diagnosis
		// further down. It read $usedChap and NOTHING EVER SET IT — an undefined
		// variable is $null, which is falsy, so that diagnosis told every host on
		// every pass that the spec set no CHAP credential. Primary1 declared one
		// throughout and the message sent the operator to add what was already
		// there (2026-08-24). The other branch had never once been reached.
		b.WriteString("$usedChap = $true\n")
		// The same credential the login uses, for making an EXISTING session
		// persistent. Register-IscsiSession writes the persistent entry with what
		// it is handed, and an empty secret fails its own length check.
		b.WriteString("$sessionChap = @{ ChapUsername = $env:BALLAST_CHAP_USER; ChapSecret = $env:BALLAST_CHAP_SECRET }\n")
	} else {
		b.WriteString("$portalAuthFirst = @{}\n")
		b.WriteString("$portalAuthFallback = @{}\n")
		b.WriteString("$usedChap = $false\n")
		b.WriteString("$sessionChap = @{}\n")
	}
	b.WriteString(`# This host's own addresses, to judge whether a portal's source binding is
# still real. Read once rather than per portal.
$myIPs = @()
try { $myIPs = @(Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop | ForEach-Object { [string]$_.IPAddress }) } catch {}
$stalePortals = @()
$portalErrs = @()

foreach ($p in $portals) {
  # UInt16 throughout: the port is part of the CIM key for these cmdlets.
  $addr = $p; $port = [uint16]3260
  if ($p -match '^(.+):(\d+)$') { $addr = $Matches[1]; $port = [uint16]$Matches[2] }
  $have = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object { $_.TargetPortalAddress -eq $addr -and [int]$_.TargetPortalPortNumber -eq $port })

  # A PORTAL BOUND TO A SOURCE ADDRESS THIS HOST NO LONGER HAS.
  #
  # The existence check is address+port, so a leftover entry in the iSCSI
  # Initiator control panel — one made by hand, or by an earlier configuration,
  # carrying an InitiatorPortalAddress that pins discovery to a particular source
  # IP — satisfies it. Ballast adopts that entry and never looks inside it.
  # Discovery then runs out of an address the host does not have, returns nothing
  # at all, and every login fails with "the target name is not found or is marked
  # as hidden from login" — a message that points squarely at the array, where
  # nothing is wrong.
  #
  # Seen on the rig 2026-08-23: HVNEW01 discovered ZERO targets on the same three
  # portals where its peers discovered three, with its IQN present on the NAS.
  #
  # This pass only REPORTS it. Re-registering a portal means removing it first,
  # and the reconcile is strictly additive — see the header, and
  # TestISCSIReconcileNeverDisconnects, which enforces it. The repair is the
  # RepairISCSIPortals job instead: operator-initiated, like RepairPool and
  # RemoveCSV, because deciding to pull a portal entry is a decision, not a
  # cadence.
  foreach ($h in $have) {
    $bound = [string]$h.InitiatorPortalAddress
    if (-not $bound -or $bound -eq '0.0.0.0' -or ($myIPs -contains $bound)) { continue }
    $stalePortals += ($addr + ' is pinned to source address ' + $bound + ', which this host no longer has')
  }

  $have = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object { $_.TargetPortalAddress -eq $addr -and [int]$_.TargetPortalPortNumber -eq $port })
  if ($have.Count -eq 0) {
    # DO NOT REGISTER A PORTAL THAT IS NOT ANSWERING.
    #
    # New-IscsiTargetPortal performs discovery, and against an address that does
    # not answer it blocks until the iSCSI layer gives up — minutes, per portal.
    # Three of those inside a reconcile is most of a cycle, and on the rig
    # 2026-08-24 the cluster pass sat stalled for 22 minutes doing exactly this.
    # A three-second TCP probe answers the same question first, and costs nothing
    # when the array is healthy.
    $reach = $false
    try {
      $sock = New-Object System.Net.Sockets.TcpClient
      $iar = $sock.BeginConnect($addr, $port, $null, $null)
      if ($iar.AsyncWaitHandle.WaitOne(3000, $false)) { $sock.EndConnect($iar); $reach = $true }
      $sock.Close()
    } catch {}
    if (-not $reach) {
      $portalErrs += ($addr + ':' + $port + ' did not answer on the iSCSI port, so it was not registered')
    } else {
      # AND a failure here must not take the pass with it.
      #
      # This ran with -ErrorAction Stop and no catch, under an $ErrorActionPreference
      # of Stop — so one portal refusing ("New-IscsiTargetPortal : Target Error")
      # aborted the WHOLE script. The login loop, the sessions, the disks and every
      # diagnosis after it never ran, and the host reported no iSCSI state at all:
      # not a fault it could describe, but silence. HVNEW02 and HVNEW05 sat like
      # that for a day while the console had nothing to show.
      #
      # Losing one portal is the ordinary iSCSI fault. The remaining paths are
      # exactly what the node keeps working on, which is the same reasoning the
      # login loop below already applies.
      try {
        # Discovery leaves through the storage vNIC on the portal's own subnet,
        # for the same reason the session does. Unbound, the routing table picks
        # one interface for every portal, so the second subnet is discovered out
        # of the first vNIC and the array answers about a target it does not
        # advertise on that path. Absent from the table means no storage vNIC
        # shares that subnet, and then nothing is bound rather than guessed.
        $pa = @{}
        if ($initiatorFor.ContainsKey($addr)) { $pa['InitiatorPortalAddress'] = $initiatorFor[$addr] }
        # UNAUTHENTICATED FIRST. Discovery and target login authenticate
        # independently, and most arrays — Synology included — put CHAP on the
        # target, not on discovery. Handing a credential to a discovery portal
        # that does not want one is refused outright with "Authentication
        # Failure", which is what stopped all three members of Primary1 on
        # 2026-08-25.
        New-IscsiTargetPortal -TargetPortalAddress $addr -TargetPortalPortNumber $port @pa @portalAuthFirst -ErrorAction Stop | Out-Null
        $changed = $true
      } catch {
        $plain = ([string]$_.Exception.Message).Trim()
        # An array that genuinely requires CHAP for DISCOVERY refuses the
        # unauthenticated attempt. Only then is the credential offered, so both
        # kinds of array work and neither is sent something it will reject.
        if ($portalAuthFallback.Count -gt 0) {
          try {
            New-IscsiTargetPortal -TargetPortalAddress $addr -TargetPortalPortNumber $port @pa @portalAuthFallback -ErrorAction Stop | Out-Null
            $changed = $true
          } catch {
            $portalErrs += ($addr + ':' + $port + ' - ' + $plain +
              ' (and again with the CHAP credential: ' + ([string]$_.Exception.Message).Trim() + ')')
          }
        } else {
          $portalErrs += ($addr + ':' + $port + ' - ' + $plain)
        }
      }
    }
  }
}
Mark-Phase 'portals'
# Refresh so newly registered portals advertise their targets before we log in.
try { Update-IscsiTarget -ErrorAction SilentlyContinue } catch {}
$out.portals = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.TargetPortalAddress + ':' + [string]$_.TargetPortalPortNumber })

# NOTHING ADVERTISED AT ALL is a different fault from a login being refused, and
# it has to be said first — the login error that follows names the target and
# points at the array, when the real answer is that discovery came back empty.
$discovered = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.TargetPortalAddress })
$advertisedNow = @(Get-IscsiTarget -ErrorAction SilentlyContinue)
# A HOST THAT HOLDS A SESSION HAS PLAINLY BEEN OFFERED A TARGET.
#
# Get-IscsiTarget can come back empty for a moment — after Update-IscsiTarget, or
# on a WMI hiccup — while the sessions built from those targets are up and
# serving. Read on its own it says "the array advertised nothing", which on such
# a host is simply false, and it was: HVNEW04 reported Connected, 1 of 1 targets,
# 3 paths and 2 disks, directly above a message saying no target had been
# advertised to it (rig, 2026-08-24). Two statements from one pass, contradicting
# each other, one of them invented.
#
# An absent reading is not a zero. The sessions are the evidence that outranks it.
$liveSessions = @(Get-IscsiSession -ErrorAction SilentlyContinue)
if ($advertisedNow.Count -eq 0 -and $discovered.Count -gt 0 -and $liveSessions.Count -eq 0) {
  # "No path" and "not permitted" look identical from here and have completely
  # different remedies — one is a network fault on this host, the other is a line
  # in the array's masking list. Ballast can tell them apart, so it does: if the
  # portal answers on its port the wire is fine and the array is choosing not to
  # advertise; if it does not answer, the array was never reached at all.
  $reachable = @()
  $unreachable = @()
  foreach ($pp in @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue)) {
    $pa = [string]$pp.TargetPortalAddress
    $pn = 3260
    try { $pn = [int]$pp.TargetPortalPortNumber } catch {}
    $okPort = $false
    try {
      $c = New-Object System.Net.Sockets.TcpClient
      $iar = $c.BeginConnect($pa, $pn, $null, $null)
      if ($iar.AsyncWaitHandle.WaitOne(3000, $false)) { $c.EndConnect($iar); $okPort = $true }
      $c.Close()
    } catch {}
    if ($okPort) { $reachable += ($pa + ':' + $pn) } else { $unreachable += ($pa + ':' + $pn) }
  }

  if ($reachable.Count -eq 0) {
    $out.message = 'no path to the array: ' + ($unreachable -join ', ') + ' did not answer on the iSCSI port from this host. ' +
      'Nothing was discovered because nothing was reached, so any login error that follows names a target this host never saw. ' +
      'The fault is HERE, not on the array — check this host''s storage network adapter and its route to those addresses.'
  } elseif ($unreachable.Count -gt 0) {
    $out.message = 'the array answered on ' + ($reachable -join ', ') + ' but advertised NO target to this host, and ' +
      ($unreachable -join ', ') + ' did not answer at all. The portals that did answer prove the wire works, so the array is choosing not to advertise: ' +
      'add this host''s initiator, ' + $out.initiatorIQN + ', to the allowed-initiator list of the SPECIFIC target. Those lists are per target on most arrays.'
  } else {
    $out.message = 'the array answered on every portal (' + ($reachable -join ', ') + ') but advertised NO target to this host. ' +
      'The wire is fine and this is not a login being refused — nothing was offered for a name to match. Two things do this. ' +
      'The array may require CHAP for DISCOVERY, not only for login' +
      $(if ($usedChap) { ' — this spec does set a CHAP credential, so check the username and secret are the ones the array expects' } else { ' — and this spec sets NO CHAP credential, so add one to the storage configuration if the array wants it' }) +
      '. Or this host''s initiator, ' + $out.initiatorIQN + ', is not on the allowed-initiator list of the SPECIFIC target; ' +
      'those lists are per target, so an initiator already permitted on another target still has to be added to this one.'
  }
}
# A stale binding outranks everything else this pass could say: it explains the
# empty discovery AND the login errors that follow from it, and it is the only
# one of the three an operator can act on directly.
# A portal that could not be registered explains a missing target, and is the
# first thing to say: everything after it is a consequence.
if ($portalErrs.Count -gt 0) {
  $out.message = 'could not register ' + ($portalErrs -join '; ') +
    '. Any target reached only through those portals will be missing, and the paths through them are not available. ' +
    $out.message
}
if ($stalePortals.Count -gt 0) {
  $out.message = 'this host cannot discover through ' + ($stalePortals -join '; ') +
    '. A discovery portal pinned to an address the host does not have returns nothing, which then fails every login with "the target name is not found" and points at the array, where nothing is wrong. ' +
    'Run Repair iSCSI portals on this host to re-register them unbound. ' + $out.message
}
`)

	// Registering the portals is safe; logging in through more than one of them is
	// not, until MPIO is actually in effect.
	//
	// "Before" in script order is not "before" in reality. Installing the feature
	// asks for a restart, and until that restart it protects nothing — so a first
	// pass on a fresh node would install MPIO, skip the claim, then log in through
	// every portal and leave Windows holding the same LUN as several devices it
	// believes are unrelated. That is the exact state the comment above says must
	// never happen, and reporting it afterwards does not undo it.
	//
	// So while multipath is required but not yet effective, log in through ONE
	// portal only. A single path cannot produce duplicates, the node gets working
	// storage meanwhile, and once the restart lands the next pass adds the
	// remaining paths — which MPIO then coalesces into the one disk.
	if wantMPIO {
		b.WriteString(`
$mpioEffective = ($out.mpioInstalled -and (Read-MPIOEffective) -and -not $out.rebootRequired)
$out.mpioEffective = $mpioEffective
$restrictPortal = ''
if (-not $mpioEffective -and $portals.Count -gt 1) {
  $first = $portals[0]
  if ($first -match '^(.+):(\d+)$') { $first = $Matches[1] }
  $restrictPortal = $first
  $out.message = 'multipath is not in effect yet, so this node is logged in through ' + $restrictPortal + ' only; the remaining paths are added once the node has restarted'
}
`)
	} else {
		// A single declared portal is a single path, so multipath is neither
		// required nor claimed — but the variable must still exist, or the login
		// below reads it as absent and quietly never declares a session multipath.
		b.WriteString("$restrictPortal = ''\n$mpioEffective = $false\n")
	}

	// Targets: explicit list, or everything advertised.
	b.WriteString("\n$wanted = @(" + psStringList(spec.Targets) + ")\n")
	b.WriteString(`if ($wanted.Count -eq 0) {
  # No explicit list: log in to everything the portals advertise. Convenient for
  # a dedicated array and wrong for a shared one, which is why the spec makes it
  # a deliberate choice rather than a default anybody arrives at by accident.
  $wanted = @(Get-IscsiTarget -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.NodeAddress })
}
# A session PER PORTAL, not one per target.
#
# "Is this target logged in at all?" was the wrong question. With three portals
# and one session the answer is yes, so the loop skipped the target and the node
# stayed on a single path for ever — multipath in effect, MPIO claiming, and
# nothing to claim, because the other two paths were never established. Seen on
# the rig with DRCluster: mpioEffective true on both members, both on one path.
#
# The portals a session already covers come from its CONNECTIONS; a session does
# not carry the portal it was made through.
foreach ($t in $wanted) {
  $existing = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object { $_.TargetNodeAddress -eq $t })
  # A session that is NOT persistent vanishes at the next reboot and takes this
  # node's disks with it, so make it persistent rather than leaving a
  # working-until-restarted state in place.
  foreach ($s in $existing) {
    if (-not $s.IsPersistent) {
      # The reason is KEPT. Swallowing it left the console saying "Not
      # persistent — a login here is not restored at boot, so this node loses
      # its disks on the next restart" with nothing about WHY, and no way to
      # act: HVNEW03, 2026-08-24. A state that costs a node its storage at the
      # next reboot has to name its own cause.
      # THE CREDENTIAL GOES WITH IT.
      #
      # Register-IscsiSession takes -ChapUsername and -ChapSecret, and writes the
      # persistent entry with whatever it is given. Called without them it writes
      # an EMPTY secret, and Windows validates that against its own length rule:
      # "Target CHAP secret given is invalid. Maximum size of CHAP secret is 16
      # bytes. Minimum size is 12 bytes if IPSec is not used." The complaint is
      # about the nothing we passed, not about the operator's credential.
      #
      # It read as a bad secret, and it is not: HVNEW01 and HVNEW02 failed here
      # on 2026-08-25 with a credential that logs in perfectly. HVNEW03 was clean
      # for the reason that proves it — its session was made FRESH by
      # Connect-IscsiTarget, which carries IsPersistent and CHAP together and so
      # never needs this call. Deleting the discovery portal by hand fixed the
      # other two for the same reason.
      $reg = @{ SessionIdentifier = $s.SessionIdentifier }
      # Matched to the session, so registering does not quietly change what it is.
      if ($s.IsMultipathEnabled) { $reg['IsMultipathEnabled'] = $true }
      $reg += $sessionChap
      try { Register-IscsiSession @reg -ErrorAction Stop; $changed = $true }
      catch { $persistErrs[([string]$s.TargetNodeAddress).ToLower()] = ([string]$_.Exception.Message).Trim() }
    }
  }
  # Which portals already carry a session, taken through the session's own
  # connection association rather than by filtering all connections on a
  # SessionIdentifier property — the connection objects do not reliably expose
  # one, so the filter matched nothing and every portal looked uncovered. That is
  # why .52 was retried on a node already logged in through it.
  $covered = @()
  foreach ($s in $existing) {
    foreach ($cn in @($s | Get-IscsiConnection -ErrorAction SilentlyContinue)) {
      $covered += [string]$cn.TargetAddress
    }
  }
  # While multipath is not in effect this is deliberately one portal, which is
  # what stops duplicate disks appearing before MPIO can coalesce them.
  $wantPortals = @()
  if ($restrictPortal) { $wantPortals = @($restrictPortal) }
  else {
    foreach ($p in $portals) { $a = $p; if ($p -match '^(.+):(\d+)$') { $a = $Matches[1] }; $wantPortals += $a }
  }
  foreach ($addr in $wantPortals) {
    if ($covered -contains $addr) { continue }
`)
	// The connect call, with CHAP only when a credential was supplied. The secret
	// arrives via the environment so it is never in the script text or a log.
	// Splatted rather than concatenated so the per-portal address is one key
	// instead of a second copy of the whole call with its CHAP arguments — two
	// spellings of the same login is how they drift apart.
	// IsMultipathEnabled is what permits a SECOND session to the same target.
	//
	// Without it Windows refuses one outright — "The target has already been logged
	// in via an iSCSI session" — which is exactly what every extra path hit on the
	// rig, leaving both members on one path with MPIO genuinely in effect and
	// nothing to coalesce. It is set only when multipath really is in effect,
	// because declaring a session multipath while MPIO is not claiming is how the
	// same LUN arrives twice as unrelated disks.
	connect := "    $c = @{ NodeAddress = $t; IsPersistent = $true; TargetPortalAddress = $addr }\n" +
		"    if ($mpioEffective) { $c['IsMultipathEnabled'] = $true }\n" +
		// Bind the session to the storage vNIC on the portal's own subnet.
		//
		// Unbound, the initiator asks the routing table, and the routing table
		// answers with ONE interface. That is how three declared portals became
		// three sessions out of a single vNIC on the rig 2026-08-24: MPIO in
		// effect, three paths reported, one cable carrying all of them. Only the
		// binding makes the paths distinct.
		//
		// Absent from the table means no storage vNIC shares that subnet, and
		// then nothing is bound rather than something guessed — the behaviour a
		// host without declared storage vNICs has always had.
		"    if ($initiatorFor.ContainsKey($addr)) { $c['InitiatorPortalAddress'] = $initiatorFor[$addr] }\n"
	if spec.CredentialSecret != "" {
		auth := "ONEWAYCHAP"
		if spec.MutualCHAP {
			auth = "MUTUALCHAP"
		}
		// One -ChapSecret, whichever direction. Mutual CHAP additionally needs the
		// initiator's own secret set once on the host (Set-IscsiChapSecret); it is
		// NOT a second -ChapSecret here, which the cmdlet rejects outright.
		connect += "    $c['AuthenticationType'] = '" + auth + "'\n" +
			"    $c['ChapUsername'] = $env:BALLAST_CHAP_USER\n" +
			"    $c['ChapSecret'] = $env:BALLAST_CHAP_SECRET\n"
	}
	// One portal being unreachable must not fail the pass or abandon the others:
	// losing a path is the ordinary iSCSI fault, and the remaining paths are
	// exactly what the node keeps working on.
	b.WriteString(connect + `    try { Connect-IscsiTarget @c -ErrorAction Stop | Out-Null; $changed = $true }
    catch {
      # "Already logged in" means the path exists — the session simply was not
      # matched above. Reporting it as a failure would put a permanent error on a
      # node whose paths are all present.
      if ([string]$_.Exception.Message -notmatch 'already been logged in') {
        $pathErrs += ($addr + ': ' + ([string]$_.Exception.Message).Trim())
      }
    }
  }
}
if ($pathErrs.Count -gt 0 -and -not $out.message) {
  $out.message = 'could not log in through ' + ($pathErrs -join '; ')
}
# The persistence note is composed AFTER the sessions are re-read, below, and
# only for targets that are STILL not persistent. See there for why.
`)

	b.WriteString(`
Mark-Phase 'logins'
$out.sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Group-Object TargetNodeAddress | ForEach-Object {
  $first = $_.Group[0]
  [pscustomobject]@{
    targetIQN  = [string]$_.Name
    connected  = $true
    persistent = [bool]@($_.Group | Where-Object { $_.IsPersistent })[0]
    paths      = [int]($_.Group | Measure-Object).Count
  }
})
# A target that is known but not logged in is reported too: "discovered but not
# connected" is a different problem from "not discovered", and they have
# different remedies.
foreach ($t in @(Get-IscsiTarget -ErrorAction SilentlyContinue)) {
  $iqn = [string]$t.NodeAddress
  if (-not ($out.sessions | Where-Object { $_.targetIQN -eq $iqn })) {
    $out.sessions += [pscustomobject]@{ targetIQN = $iqn; connected = $false; persistent = $false; paths = 0 }
  }
}
# A registration that FAILED and was then superseded is not a fault.
#
# Register-IscsiSession is tried on a session that is not persistent; if the same
# pass then makes a fresh session through Connect-IscsiTarget, that one carries
# IsPersistent itself and the target ends up persistent regardless. The old code
# composed the note from the ATTEMPT, before re-reading the sessions two lines
# later, so a node that had ended in exactly the state asked of it reported a
# fault it no longer had.
#
# HVNEW03, 2026-09-01: two paths, persistent true, and "a session could not be
# made persistent, so it will not be restored at boot". Both from the same pass.
# On the same fleet HVNEW01 and HVNEW02 were genuinely affected, so the note is
# not noise to suppress — it is a verdict that has to be taken at the end.
$stillNotPersistent = @()
foreach ($k in @($persistErrs.Keys)) {
  $now = @($out.sessions | Where-Object { ([string]$_.targetIQN).ToLower() -eq $k })[0]
  if ($now -and $now.persistent) { continue }   # superseded; the end state is right
  $stillNotPersistent += ($k + ': ' + [string]$persistErrs[$k])
}
if ($stillNotPersistent.Count -gt 0) {
  $note = 'a session could not be made persistent, so it will not be restored at boot: ' + ($stillNotPersistent -join '; ')
  if ($out.message) { $out.message = $out.message + '. ' + $note } else { $out.message = $note }
}

# Disks arriving over iSCSI. The SERIAL is what identifies a LUN cluster-wide;
# the disk number is per-node and moves across reboots, so it is reported for
# display only and never used to bind a volume.
#
# The enumeration is NOT silently swallowed. -ErrorAction SilentlyContinue on its
# own turns "the Storage service did not answer" into an empty list, which is
# reported as "this host sees no iSCSI disks" — a different and much more
# alarming statement than "I could not tell". Absent is not zero.
$diskReadFailed = ''
$rawDisks = @()
try {
  $rawDisks = @(Get-Disk -ErrorAction Stop)
} catch {
  $diskReadFailed = ([string]$_.Exception.Message).Trim()
}
# Read-BallastDiskContents describes what a LUN already carries, in the words the
# adoption refusal uses — one probe, one vocabulary, so the console and the
# refusal cannot disagree about the same disk.
function Read-BallastDiskContents($d) {
  try {
    if ([string]$d.PartitionStyle -eq 'RAW') { return '' }
    $what = @()
    foreach ($p in @(Get-Partition -DiskNumber ([int]$d.Number) -ErrorAction SilentlyContinue | Where-Object { $_.Type -ne 'Reserved' })) {
      $v = Get-Volume -Partition $p -ErrorAction SilentlyContinue
      if ($v -and $v.FileSystem) {
        $label = [string]$v.FileSystemLabel
        $used = [math]::Round(($v.Size - $v.SizeRemaining)/1GB,1)
        $tot = [math]::Round($v.Size/1GB,1)
        $what += ('a ' + [string]$v.FileSystem + ' volume' + $(if ($label) { ' labelled "' + $label + '"' } else { '' }) + ', ' + $used + 'GB used of ' + $tot + 'GB')
      } elseif ($v) {
        $what += 'an unformatted partition'
      } else {
        $what += ('a ' + [string]$p.Type + ' partition whose filesystem could not be read')
      }
    }
    return ($what -join ' and ')
  } catch { return '' }
}

$out.disks = @($rawDisks | Where-Object { $_.BusType -eq 'iSCSI' } | ForEach-Object {
  [pscustomobject]@{
    serialNumber = ([string]$_.SerialNumber).Trim()
    number       = [int]$_.Number
    sizeBytes    = [uint64]$_.Size
    targetIQN    = ''
    lun          = -1
    clustered    = [bool]$_.IsClustered
    offline      = [bool]$_.IsOffline
    # WHAT IS ALREADY ON IT, so the console can say what adopting would destroy
    # BEFORE an operator chooses — rather than after the reconcile has refused.
    # The same probe the adoption itself does; it was computed there, used to
    # refuse, and thrown away, so nothing upstream could ever show it.
    #
    # contentsKnown separates "blank" from "nobody looked". Both read as an empty
    # string and they have opposite consequences for a wipe, which is the
    # absent-is-not-zero trap in the one place it costs data.
    contents      = Read-BallastDiskContents $_
    contentsKnown = $true
  }
})

# LOGGED IN AND NOTHING BEHIND IT.
#
# A session with paths and no LUNs behind it is a real, specific and very
# diagnosable condition, and Ballast said NOTHING about it: no login failed, so
# no message was set, and the host reported a healthy-looking iSCSI block with an
# empty disk list. On the rig 2026-08-23 both DRCluster members sat like this —
# three paths each, zero disks — while the cluster's two CSVs went Offline and
# the only thing naming a problem was a CSV alarm one layer up.
#
# The initiator side is fine here by definition: the login succeeded and the
# paths are up. What is missing is on the array — a LUN mapped to the target, and
# this initiator permitted to see it. That is outside what Ballast administers,
# so the message names the one step rather than implying the host is at fault.
if (-not $out.message) {
  $connected = @($out.sessions | Where-Object { $_.connected })
  if ($diskReadFailed) {
    # Say which it is. "Could not enumerate" and "there are none" look identical
    # in an empty list and mean opposite things.
    $out.message = 'could not read this host''s disks, so whether the iSCSI LUNs arrived is UNKNOWN rather than none: ' + $diskReadFailed
  } elseif ($connected.Count -gt 0 -and $out.disks.Count -eq 0) {
    $paths = [int](($connected | Measure-Object -Property paths -Sum).Sum)
    $names = (@($connected | ForEach-Object { [string]$_.targetIQN }) -join ', ')
    $out.message = 'logged in to ' + $names + ' over ' + $paths + ' path(s), but the array is presenting no LUNs on it. ' +
      'The initiator side is working — the login succeeded and the paths are up — so this is on the storage device: ' +
      'check that a LUN is mapped to that target and that this host''s initiator, ' + $out.initiatorIQN +
      ', is permitted to see it. Any cluster volume on those LUNs will be Offline until it is.'
  }
}
Mark-Phase 'report'
$out.phases = $phases
$out.elapsedMs = [int]$swTotal.ElapsedMilliseconds
$out.changed = $changed
[pscustomobject]$out | ConvertTo-Json -Compress -Depth 5
`)
	return b.String()
}

// EnsureISCSI connects this node to the cluster's iSCSI storage and reports what
// it sees. Additive only — it never disconnects a session or removes a portal.
func (p *PowerShell) EnsureISCSI(ctx context.Context, spec types.ISCSIStorageSpec, chapUser, chapSecret string, shared bool, storageAddresses []string) (ISCSIState, Outcome, error) {
	var st ISCSIState
	if len(spec.Portals) == 0 {
		return st, OutcomeUnchanged, fmt.Errorf("ensure iscsi: at least one portal is required")
	}
	wantMPIO, _ := spec.MPIORequired()

	env := []string{}
	if spec.CredentialSecret != "" {
		if chapUser == "" || chapSecret == "" {
			return st, OutcomeUnchanged, fmt.Errorf("ensure iscsi: credential %q was not delivered to this host, so CHAP login cannot be attempted", spec.CredentialSecret)
		}
		env = append(env, "BALLAST_CHAP_USER="+chapUser, "BALLAST_CHAP_SECRET="+chapSecret)
	}

	out, err := p.runWithEnvOut(ctx, iscsiScript(spec, wantMPIO, shared, storageAddresses), env)
	if err != nil {
		return st, OutcomeUnchanged, fmt.Errorf("ensure iscsi: %w", err)
	}
	var res struct {
		// Where the time went inside the script, so a slow pass names the cmdlet
		// rather than just the step. See the Mark-Phase notes above.
		Phases    map[string]int `json:"phases"`
		ElapsedMs int            `json:"elapsedMs"`

		InitiatorIQN   string   `json:"initiatorIQN"`
		ServiceRunning bool     `json:"serviceRunning"`
		Portals        []string `json:"portals"`
		MPIOInstalled  bool     `json:"mpioInstalled"`
		MPIOClaimed    bool     `json:"mpioClaimed"`
		MPIOEffective  bool     `json:"mpioEffective"`
		RebootRequired bool     `json:"rebootRequired"`
		Message        string   `json:"message"`
		Changed        bool     `json:"changed"`
		Sessions       []struct {
			TargetIQN  string `json:"targetIQN"`
			Connected  bool   `json:"connected"`
			Persistent bool   `json:"persistent"`
			Paths      int    `json:"paths"`
		} `json:"sessions"`
		Disks []struct {
			SerialNumber string `json:"serialNumber"`
			Number       int    `json:"number"`
			SizeBytes    uint64 `json:"sizeBytes"`
			TargetIQN    string `json:"targetIQN"`
			LUN          int    `json:"lun"`
			Clustered    bool   `json:"clustered"`
			Offline      bool   `json:"offline"`
		} `json:"disks"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return st, OutcomeUnchanged, fmt.Errorf("ensure iscsi: %w", err)
	}
	st.InitiatorIQN = res.InitiatorIQN
	st.ServiceRunning, st.Portals = res.ServiceRunning, res.Portals
	st.MPIOInstalled, st.MPIOClaimed, st.RebootRequired, st.Message = res.MPIOInstalled, res.MPIOClaimed, res.RebootRequired, res.Message
	// A slow pass has to name the cmdlet, not just the step. Appended only when
	// this actually took long enough to matter — on a healthy host it is noise,
	// and a message that always carries timings is one nobody reads.
	if slow := slowISCSIPhases(res.Phases, res.ElapsedMs); slow != "" {
		if st.Message != "" {
			st.Message += ". "
		}
		st.Message += slow
	}
	st.MPIOEffective = res.MPIOEffective
	for _, s := range res.Sessions {
		st.Sessions = append(st.Sessions, ISCSISessionState{
			TargetIQN: s.TargetIQN, Connected: s.Connected, Persistent: s.Persistent, Paths: s.Paths,
		})
	}
	for _, d := range res.Disks {
		st.Disks = append(st.Disks, ISCSIDiskState{
			SerialNumber: d.SerialNumber, Number: d.Number, SizeBytes: d.SizeBytes,
			TargetIQN: d.TargetIQN, LUN: d.LUN, Clustered: d.Clustered, Offline: d.Offline,
		})
	}
	if res.Changed {
		return st, OutcomeUpdated, nil
	}
	return st, OutcomeUnchanged, nil
}

// RepairISCSIPortals re-registers discovery portals whose source binding names
// an address this host no longer has.
//
// A portal created with an InitiatorPortalAddress pins discovery to one source
// IP. When that IP goes — a NIC replaced, a converged switch rebuilt, a host
// re-addressed — the entry stays behind and discovery through it silently
// returns nothing. Every login then fails with "the target name is not found or
// is marked as hidden from login", which sends the operator to the array, where
// nothing is wrong. The reconcile detects and reports this; it does not fix it,
// because fixing it means REMOVING a portal entry and the reconcile is strictly
// additive.
//
// So this is a job: deliberate, operator-initiated, and reported. It is the same
// shape as RepairPool and RemoveCSV — the destructive half of a recovery, kept
// out of the loop that runs every cycle.
//
// It is narrow on purpose. It touches ONLY portals whose bound address is absent
// from this host: a binding to a real, present NIC is somebody's deliberate
// choice about which path reaches the array, and is left exactly alone. It never
// disconnects a session; removing a discovery portal does not drop existing
// logins, and the reconcile re-registers anything the spec still wants.
/*
RediscoverISCSI clears this host's discovery portals so the reconcile rebuilds
them from the spec.

	Windows keeps its OWN discovered-target database alongside the portals, and it
	goes stale: an entry that no longer matches what the array advertises makes
	Connect-IscsiTarget refuse with "The target name is not found or is marked as
	hidden from login" — a message that reads like the array refusing the node,
	and is not.

	On the rig, 2026-09-01, that message had two of three nodes on one path each
	and the third on two. Deleting every discovery portal in iscsicpl and letting
	the next reconcile re-add them fixed it outright; the array was never touched.
	That is a recovery an operator had to reach by opening a GUI on the host,
	which CLAUDE.md calls a defect in Ballast rather than a runbook step — and it
	is the messy-recovery-after-something-went-wrong case the brief warns is
	hardest to be disciplined about.

	Distinct from RepairISCSIPortals, which is narrow BY DESIGN because it runs
	inside the reconcile loop: it touches only portals bound to an address the
	host does not have, since a binding to a present NIC is somebody's deliberate
	choice about which path reaches the array. That narrowness is right for an
	automatic pass and wrong as the only thing an operator can reach for.

	Distinct also from ResetISCSIInitiator, which disconnects sessions and drops
	persistent logins. This does neither: removing a discovery portal does not
	drop an existing session, so a node keeps its disks throughout and the worst
	case is that the next reconcile re-adds exactly what was there.
*/
func (p *PowerShell) RediscoverISCSI(ctx context.Context) (string, error) {
	script := `$ErrorActionPreference = 'Stop'
$before = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue)
$targetsBefore = @(Get-IscsiTarget -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.NodeAddress })
if ($before.Count -eq 0) {
  'RESULT=there are no discovery portals on this host to clear. The spec adds them on the next reconcile.'
  return
}
$attempted = @(); $failed = @()
foreach ($h in $before) {
  $addr = [string]$h.TargetPortalAddress
  $attempted += $addr
  # Piped, never named. -TargetPortalPortNumber fails with "Type mismatch for
  # parameter" on this cmdlet whatever is put in it, and the object carries the
  # identity the initiator itself assigned.
  # A retry naming the address was written here and removed: naming this portal
  # rather than piping it fails on this cmdlet, which is a finding already
  # recorded above and pinned by a test. The end-state check below is what makes
  # the difference, and it does not depend on the call reporting anything.
  try { $h | Remove-IscsiTargetPortal -Confirm:$false -ErrorAction Stop }
  catch { $failed += ($addr + ': ' + ([string]$_.Exception.Message).Trim()) }
}
# Sessions are deliberately left alone. Removing a discovery portal does not drop
# one, so the node keeps its disks while this runs.
$sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue).Count

# JUDGED ON THE END STATE, not on whether the calls returned.
#
# HVNEW04 and HVNEW05 reported "none of the 2 discovery portal(s) could be
# removed: The specified portal was not found" — for portals that had just been
# listed. Whether that means they were already gone, or the remove could not
# match them, the question an operator has is the same one: are they there now.
# A cmdlet complaining is not an answer to it, and reporting Failed for a host
# that ended in exactly the wanted state sends somebody to fix nothing.
$after = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.TargetPortalAddress })
$removed = @($attempted | Where-Object { $after -notcontains $_ })
$stillThere = @($attempted | Where-Object { $after -contains $_ })

if ($stillThere.Count -gt 0) {
  throw ([string]$stillThere.Count + ' discovery portal(s) are still on this host after being asked to go (' +
    ($stillThere -join ', ') + ')' + $(if ($failed.Count) { ': ' + ($failed -join '; ') } else { ', and the removal reported no error, which means something else is holding them' }))
}
$msg = 'cleared ' + [string]$removed.Count + ' discovery portal(s) (' + ($removed -join ', ') + ')'
if ($failed.Count -gt 0) {
  # Gone, but not without complaint. Worth saying: the end state is right and
  # the next person reading a journal should know it was not a clean removal.
  $msg += ' — they are gone, though the removal reported: ' + ($failed -join '; ')
}
$msg += '. ' + [string]$sessions + ' existing session(s) left connected. The next reconcile re-adds the portals from the spec and logs in again'
'RESULT=' + $msg`

	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("rediscover iSCSI: %w", err)
	}
	msg := strings.TrimSpace(string(out))
	if i := strings.Index(msg, "RESULT="); i >= 0 {
		msg = strings.TrimSpace(msg[i+len("RESULT="):])
	}
	return msg, nil
}

func (p *PowerShell) RepairISCSIPortals(ctx context.Context) (string, error) {
	script := `$ErrorActionPreference = 'Stop'
$myIPs = @()
try { $myIPs = @(Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop | ForEach-Object { [string]$_.IPAddress }) } catch {}
$fixed = @()
$failed = @()
$checked = 0
foreach ($h in @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue)) {
  $checked++
  $addr = [string]$h.TargetPortalAddress
  # UInt16: the port is part of the CIM key and an Int32 fails the lookup.
  $port = [uint16]3260
  try { $port = [uint16]$h.TargetPortalPortNumber } catch {}
  $bound = [string]$h.InitiatorPortalAddress
  # Unbound, or bound to an address this host really has: leave it alone. The
  # second case is a deliberate choice about which path reaches the array.
  if (-not $bound -or $bound -eq '0.0.0.0' -or ($myIPs -contains $bound)) { continue }
  try {
    # Piped, and the port is not named: -TargetPortalPortNumber fails with "Type
    # mismatch for parameter" on this cmdlet whatever is put in it. See the note
    # in powershell_iscsireset.go. The re-add still names the port, because
    # New-IscsiTargetPortal takes it happily (UInt16 there, Int32 here — they do
    # not agree, which is half the reason the parameter was never usable).
    $h | Remove-IscsiTargetPortal -Confirm:$false -ErrorAction Stop
    New-IscsiTargetPortal -TargetPortalAddress $addr -TargetPortalPortNumber $port -ErrorAction Stop | Out-Null
    $fixed += ($addr + ':' + $port + ' (was pinned to ' + $bound + ')')
  } catch {
    $failed += ($addr + ':' + $port + ' - ' + ([string]$_.Exception.Message).Trim())
  }
}
# Re-run discovery so the result of the repair is visible immediately rather
# than on the next reconcile.
try { Update-IscsiTarget -ErrorAction SilentlyContinue } catch {}
$seen = @(Get-IscsiTarget -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.NodeAddress })
$res = [ordered]@{ checked = $checked; fixed = $fixed; failed = $failed; targets = $seen }
'RESULT=' + ($res | ConvertTo-Json -Compress -Depth 4)`

	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("repair iscsi portals: %w", err)
	}
	var res struct {
		Checked int      `json:"checked"`
		Fixed   []string `json:"fixed"`
		Failed  []string `json:"failed"`
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal([]byte(resultJSON(string(out))), &res); err != nil {
		return "", fmt.Errorf("repair iscsi portals: could not read the result: %w", err)
	}
	if len(res.Failed) > 0 {
		return "", fmt.Errorf("could not re-register %s", strings.Join(res.Failed, "; "))
	}
	// Say what discovery sees NOW. A repair that reports only what it changed
	// leaves the operator to go and look for whether it worked.
	found := "discovery now sees no targets — the portals are reachable but this host's initiator is not on any target's allowed list, or there is no path to them"
	if len(res.Targets) > 0 {
		found = "discovery now sees " + strings.Join(res.Targets, ", ")
	}
	if len(res.Fixed) == 0 {
		return "checked " + strconv.Itoa(res.Checked) + " discovery portals; none was pinned to a missing address, so nothing needed re-registering. " + found, nil
	}
	return "re-registered " + strings.Join(res.Fixed, "; ") + ". " + found, nil
}

// resultJSON pulls the RESULT= payload out of a script's stdout, ignoring any
// other lines a cmdlet decided to print.
func resultJSON(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "RESULT="); found {
			return v
		}
	}
	return "{}"
}

// DisconnectISCSITarget logs this host out of one target and forgets it, so the
// login is not restored at the next boot.
//
// The reconcile will not do this. It is strictly additive on purpose: changing
// the target in a spec adds the new login and leaves the old one, because
// Ballast cannot tell from a spec edit whether a target it no longer lists is
// one the operator retired or one something else on the host still depends on.
// Deciding that is the operator's, so this is a job.
//
// It REFUSES while the target's disks are in use. A session carrying a clustered
// disk, or an online one, is carrying something: disconnecting it takes the disk
// away from whatever has it open, and "the volume went away" is not a failure
// anybody can trace back to a button. The refusal names the disks so the answer
// is actionable rather than a flat no.
func (p *PowerShell) DisconnectISCSITarget(ctx context.Context, targetIQN string) (string, error) {
	if strings.TrimSpace(targetIQN) == "" {
		return "", fmt.Errorf("disconnect iscsi target: a target name is required")
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$t = %[1]s
# PERSISTENT LOGINS OUTLIVE THE SESSION.
#
# Unregister-IscsiSession needs a live session to unregister, and
# Disconnect-IscsiTarget ends a connection without touching the persistent
# registration behind it. So a target that has been DELETED on the array leaves
# an entry the initiator retries for ever — roughly once a minute, logged by the
# array every time. Seen on the rig 2026-08-24: HVNEW02 held no session at all
# and the Synology recorded "tried to login into a non-existent iSCSI iqn"
# minute after minute, while Ballast reported nothing wrong.
#
# There is no cmdlet for this. iscsicli is the supported route, so it is parsed
# rather than guessed at, and every removal is reported.
function Remove-PersistentTarget([string]$want) {
  $removed = @()
  $errs = @()
  $out = @()
  try { $out = @(& iscsicli ListPersistentTargets 2>&1 | ForEach-Object { [string]$_ }) } catch { return @{ removed = $removed; errors = @('could not list persistent targets: ' + $_.Exception.Message) } }
  $cur = @{}
  $records = @()
  foreach ($line in $out) {
    $kv = $line -split '\s*:\s*', 2
    if ($kv.Count -ne 2) { continue }
    $k = $kv[0].Trim(); $v = $kv[1].Trim()
    # A new "Target Name" starts a new record; flush the one before it.
    if ($k -eq 'Target Name') {
      if ($cur.ContainsKey('target')) { $records += ,$cur }
      $cur = @{ target = $v }
    }
    elseif ($k -eq 'Initiator Name') { $cur['initiator'] = $v }
    elseif ($k -eq 'Port Number') { $cur['port'] = $v }
    elseif ($k -like 'Address and Socket*') { $cur['addr'] = $v }
  }
  if ($cur.ContainsKey('target')) { $records += ,$cur }

  foreach ($rec in $records) {
    if ([string]$rec['target'] -ne $want) { continue }
    $init = [string]$rec['initiator']; if (-not $init) { $init = 'ROOT' + [char]92 + 'ISCSIPRT' + [char]92 + '0000_0' }
    $port = [string]$rec['port']
    # "<Any Port>" is iscsicli's wildcard; it is passed back as *.
    if (-not $port -or $port -like '*Any*') { $port = '*' }
    $addr = ''; $sock = '3260'
    $parts = ([string]$rec['addr']) -split '\s+' | Where-Object { $_ }
    if ($parts.Count -ge 1) { $addr = $parts[0] }
    if ($parts.Count -ge 2) { $sock = $parts[1] }
    if (-not $addr) { $errs += ('no portal address recorded for ' + $want); continue }
    try {
      $r = & iscsicli RemovePersistentTarget $init $want $port $addr $sock 2>&1
      if ($LASTEXITCODE -eq 0) { $removed += ($addr + ':' + $sock) }
      else { $errs += ($addr + ':' + $sock + ' - ' + (($r | ForEach-Object { [string]$_ }) -join ' ')) }
    } catch { $errs += ($addr + ':' + $sock + ' - ' + $_.Exception.Message) }
  }
  return @{ removed = $removed; errors = $errs }
}
$persist = Remove-PersistentTarget $t
# -eq is case-insensitive, which is what we want: Get-IscsiSession echoes the
# target name lower-cased while the operator sees the array's capitalisation.
$sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object { [string]$_.TargetNodeAddress -eq $t })
if ($sessions.Count -eq 0) {
  # Already gone. Still clear any persistent entry, or it returns at boot.
  try { Disconnect-IscsiTarget -NodeAddress $t -Confirm:$false -ErrorAction SilentlyContinue } catch {}
  'RESULT=' + (@{ removed = 0; absent = $true; persistRemoved = $persist.removed; persistErrors = $persist.errors } | ConvertTo-Json -Compress -Depth 3)
} else {
  # What this target is actually carrying. The association is the only reliable
  # way to attribute a disk to a session — the disk itself does not name its
  # target, which is why the reported LUN is -1.
  $inUse = @()
  foreach ($s in $sessions) {
    $disks = @()
    try { $disks = @($s | Get-Disk -ErrorAction SilentlyContinue) } catch {}
    foreach ($d in $disks) {
      if ([bool]$d.IsClustered) { $inUse += ('disk ' + [int]$d.Number + ' is a clustered disk') }
      elseif (-not [bool]$d.IsOffline) { $inUse += ('disk ' + [int]$d.Number + ' is online') }
    }
  }
  if ($inUse.Count -gt 0) {
    throw ('refusing to disconnect ' + $t + ': ' + (($inUse | Sort-Object -Unique) -join '; ') +
      '. Disconnecting takes those disks away from whatever has them open. Take the volume offline (or remove it from the cluster) first, then disconnect.')
  }
  $n = 0
  foreach ($s in $sessions) {
    # Unregister first: a persistent session that is merely disconnected comes
    # back at the next boot, which is a fix that lasts until the next restart.
    try { Unregister-IscsiSession -SessionIdentifier $s.SessionIdentifier -ErrorAction SilentlyContinue } catch {}
    try { Disconnect-IscsiTarget -NodeAddress $t -SessionIdentifier $s.SessionIdentifier -Confirm:$false -ErrorAction Stop; $n++ } catch {}
  }
  # Belt and braces: clear the target's persistent entry outright, so nothing
  # restores it.
  try { Disconnect-IscsiTarget -NodeAddress $t -Confirm:$false -ErrorAction SilentlyContinue } catch {}
  $left = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object { [string]$_.TargetNodeAddress -eq $t })
  if ($left.Count -gt 0) {
    throw ('disconnected ' + $n + ' session(s) from ' + $t + ' but ' + $left.Count + ' remain - something still holds it')
  }
  'RESULT=' + (@{ removed = $n; absent = $false; persistRemoved = $persist.removed; persistErrors = $persist.errors } | ConvertTo-Json -Compress -Depth 3)
}`, psQuote(targetIQN))

	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("disconnect iscsi target %q: %w", targetIQN, err)
	}
	var res struct {
		Removed        int      `json:"removed"`
		Absent         bool     `json:"absent"`
		PersistRemoved []string `json:"persistRemoved"`
		PersistErrors  []string `json:"persistErrors"`
	}
	if err := json.Unmarshal([]byte(resultJSON(string(out))), &res); err != nil {
		return "", fmt.Errorf("disconnect iscsi target %q: could not read the result: %w", targetIQN, err)
	}
	// The persistent registration is the half that matters for a target the array
	// has DELETED: with no session to end there is nothing to disconnect, and the
	// entry alone makes the initiator retry about once a minute for ever.
	persist := ""
	switch {
	case len(res.PersistErrors) > 0:
		persist = "; the persistent login could NOT be removed (" + strings.Join(res.PersistErrors, "; ") +
			"), so this host will keep retrying the target about once a minute"
	case len(res.PersistRemoved) > 0:
		persist = "; removed the persistent login through " + strings.Join(res.PersistRemoved, ", ") +
			", so it will not be retried again"
	default:
		persist = "; no persistent login for it was registered"
	}

	if res.Absent {
		return "this host held no session to " + targetIQN + persist, nil
	}
	return "disconnected " + strconv.Itoa(res.Removed) + " session(s) from " + targetIQN + persist, nil
}

// PruneISCSIPortals removes discovery portals this host holds that the spec does
// not declare.
//
// The reconcile is strictly additive, deliberately: it cannot tell a portal an
// operator retired from one something else on the host depends on. So a portal
// added by mistake, or left over from an earlier configuration, stays for ever.
// On the rig 2026-08-24 both members of Primary1 carried 10.0.60.53 and
// 10.0.60.54 from a config three edits old — they inflated the path count the
// console reports, and they kept advertising a target from a deleted cluster.
// The only ways out were iscsicpl on the host, or ResetISCSIInitiator, which is
// nuclear AND refuses while disks carry partitions, which is exactly when an
// operator needs this. Opening a PowerShell session on a host to fix a managed
// object is a defect, not a runbook step.
//
// Operator-initiated, like RepairPool and DisconnectISCSITarget, because
// deciding to pull a portal entry is a decision rather than a cadence.
//
// SAFETY: an undeclared portal that is CARRYING A SESSION is refused, not
// removed. Either the spec is missing a portal the host really uses, or a
// session exists nobody declared; both are for a person to resolve, and guessing
// either way drops a live storage path.
func (p *PowerShell) PruneISCSIPortals(ctx context.Context, declared, declaredTargets []string) (string, error) {
	if len(declared) == 0 {
		// Nothing declared means nothing to compare against. Pruning here would
		// remove every portal on the host, which is ResetISCSIInitiator wearing a
		// safer-sounding name.
		return "", fmt.Errorf("prune iscsi portals: no portals are declared for this host, so there is nothing to prune against — declaring none would mean removing them all, which is what Reset initiator is for")
	}
	if len(declaredTargets) == 0 {
		// Without them every favourite target on the host looks undeclared, and
		// the prune below would remove the lot — each node losing its storage at
		// the next reboot. Refused here as well as in the job dispatch: this is
		// the layer that does the removing, and a guard that only lives in the
		// caller is one a second caller will not have.
		return "", fmt.Errorf("prune iscsi: no declared targets were given, so every favourite target on this host would look undeclared and be removed. That is what Reset initiator does; this only removes what the spec does not name")
	}
	var quoted []string
	for _, d := range declared {
		h := strings.TrimSpace(d)
		if x, _, err := net.SplitHostPort(h); err == nil {
			h = x
		}
		quoted = append(quoted, psQuote(h))
	}
	// The declared TARGETS, lower-cased for the comparison. Without them the
	// persistent-login prune below would find $declaredTargets undefined, and
	// "$null -contains x" is false — so it would remove every favourite target on
	// the host, declared ones included, and each node would lose its storage at
	// the next reboot. An empty list here is a refusal, not a default.
	var tq []string
	for _, t := range declaredTargets {
		if v := strings.ToLower(strings.TrimSpace(t)); v != "" {
			tq = append(tq, psQuote(v))
		}
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$declared = @(%s)
$declaredTargets = @(`+strings.Join(tq, ",")+`)
$removed = @(); $kept = @(); $failed = @()
# Which portals carry a live connection, taken from the sessions' own
# connections — a session does not record the portal it was made through.
$busy = @()
foreach ($s in @(Get-IscsiSession -ErrorAction SilentlyContinue)) {
  foreach ($cn in @($s | Get-IscsiConnection -ErrorAction SilentlyContinue)) {
    $busy += [string]$cn.TargetAddress
  }
}
foreach ($h in @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue)) {
  $addr = [string]$h.TargetPortalAddress
  # UInt16: the port is part of the CIM key and an Int32 fails the lookup.
  $port = [uint16]3260
  try { $port = [uint16]$h.TargetPortalPortNumber } catch {}
  if ($declared -contains $addr) { continue }
  if ($busy -contains $addr) {
    $kept += ($addr + ':' + $port)
    continue
  }
  try {
    # Piped; see powershell_iscsireset.go for why the port is never named.
    $h | Remove-IscsiTargetPortal -Confirm:$false -ErrorAction Stop
    $removed += ($addr + ':' + $port)
  } catch {
    $failed += ($addr + ':' + $port + ' - ' + ([string]$_.Exception.Message).Trim())
  }
}
# STALE FAVOURITE TARGETS, which are what actually drag.
#
# Pruning discovery portals alone was not enough. A persistent login outlives the
# portal it was made through: the initiator keeps retrying a target that no
# longer exists, once a minute, for ever, and every one costs time on every pass.
# On Secondary 2026-08-25 the iSCSI step took 3m43s of a 5m pass, and the
# operator found the cause in iscsicpl — a pile of old entries — after this
# button had already claimed to have tidied up.
#
# Only targets the spec does not declare, and only where NO live session uses
# them. A persistent entry behind a working session is what brings that session
# back after a reboot; removing it is how a node silently loses its storage at
# the next restart.
$persistRemoved = @(); $persistKept = @()
$liveTargets = @(Get-IscsiSession -ErrorAction SilentlyContinue | ForEach-Object { ([string]$_.TargetNodeAddress).ToLower() })
$plisting = @()
try { $plisting = @(& iscsicli ListPersistentTargets 2>&1 | ForEach-Object { [string]$_ }) } catch {}
$pcur = @{}; $precords = @()
foreach ($line in $plisting) {
  $kv = $line -split '\s*:\s*', 2
  if ($kv.Count -ne 2) { continue }
  $k = $kv[0].Trim(); $v = $kv[1].Trim()
  if ($k -eq 'Target Name') { if ($pcur.ContainsKey('target')) { $precords += ,$pcur }; $pcur = @{ target = $v } }
  elseif ($k -eq 'Initiator Name') { $pcur['initiator'] = $v }
  elseif ($k -eq 'Port Number') { $pcur['port'] = $v }
  elseif ($k -like 'Address and Socket*') { $pcur['addr'] = $v }
}
if ($pcur.ContainsKey('target')) { $precords += ,$pcur }
foreach ($rec in $precords) {
  $tn = [string]$rec['target']
  if ($declaredTargets -contains $tn.ToLower()) { continue }
  if ($liveTargets -contains $tn.ToLower()) { $persistKept += $tn; continue }
  $init = [string]$rec['initiator']; if (-not $init) { $init = 'ROOT' + [char]92 + 'ISCSIPRT' + [char]92 + '0000_0' }
  $pport = [string]$rec['port']; if (-not $pport -or $pport -like '*Any*') { $pport = '*' }
  $paddr = ''; $psock = '3260'
  $pparts = @(([string]$rec['addr']) -split '\s+' | Where-Object { $_ })
  if ($pparts.Count -ge 1) { $paddr = $pparts[0] }
  if ($pparts.Count -ge 2) { $psock = $pparts[1] }
  if (-not $paddr) { continue }
  try {
    & iscsicli RemovePersistentTarget $init $tn $pport $paddr $psock | Out-Null
    if ($LASTEXITCODE -eq 0) { $persistRemoved += ($tn + ' via ' + $paddr) }
  } catch {}
}

# Re-run discovery so what survived is visible at once rather than next pass.
try { Update-IscsiTarget -ErrorAction SilentlyContinue } catch {}
$left = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.TargetPortalAddress })
$res = [ordered]@{ removed = $removed; kept = $kept; failed = $failed; left = $left; persistRemoved = $persistRemoved; persistKept = $persistKept }
'RESULT=' + ($res | ConvertTo-Json -Compress -Depth 4)`, strings.Join(quoted, ","))

	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("prune iscsi portals: %w", err)
	}
	var res struct {
		Removed        []string `json:"removed"`
		Kept           []string `json:"kept"`
		PersistRemoved []string `json:"persistRemoved"`
		PersistKept    []string `json:"persistKept"`
		Failed         []string `json:"failed"`
		Left           []string `json:"left"`
	}
	if err := json.Unmarshal([]byte(resultJSON(string(out))), &res); err != nil {
		return "", fmt.Errorf("prune iscsi portals: could not read the result: %w", err)
	}
	if len(res.Failed) > 0 {
		return "", fmt.Errorf("could not remove %s", strings.Join(res.Failed, "; "))
	}
	// Stale favourite targets are what actually drag: a persistent login outlives
	// the portal it was made through and the initiator retries it once a minute
	// for ever. Reported separately from portals because they are a different
	// leftover with a different cost.
	var extra []string
	if len(res.PersistRemoved) > 0 {
		extra = append(extra, "removed "+strconv.Itoa(len(res.PersistRemoved))+" stale favourite target(s): "+strings.Join(res.PersistRemoved, ", "))
	}
	if len(res.PersistKept) > 0 {
		extra = append(extra, strings.Join(res.PersistKept, ", ")+" were left: undeclared, but a live session is using them, and removing the persistent entry is how a node loses its storage at the next reboot")
	}
	if len(res.Removed) == 0 && len(res.Kept) == 0 && len(extra) == 0 {
		return "every discovery portal on this host is declared; nothing to prune", nil
	}
	note := ""
	if len(res.Removed) > 0 {
		note = "removed " + strconv.Itoa(len(res.Removed)) + " undeclared discovery portal(s): " + strings.Join(res.Removed, ", ")
	}
	// Reported, not hidden. A portal left behind is the one the operator most
	// needs to think about: it is undeclared AND in use.
	if len(res.Kept) > 0 {
		k := strings.Join(res.Kept, ", ") + " were left: they are not declared but are carrying a live session, so removing them would drop a storage path. Either add them to the spec or disconnect the target first"
		if note == "" {
			note = k
		} else {
			note += ". " + k
		}
	}
	if len(extra) > 0 {
		if note == "" {
			note = strings.Join(extra, ". ")
		} else {
			note += ". " + strings.Join(extra, ". ")
		}
	}
	return note, nil
}

// slowISCSIPhaseThreshold is when the iSCSI step is worth explaining. A pass is
// cut off at five minutes and the cluster reconcile runs after this, so a step
// taking tens of seconds is already eating someone else's budget.
const slowISCSIPhaseThreshold = 30000

// slowISCSIPhases names where a slow iSCSI step spent its time.
//
// The pass timer stops at "iscsi 4m3s" — enough to know the cluster never
// formed because the pass was cut off, and not enough to know which call
// blocked. Several of these cmdlets hang for minutes against a portal that
// answers on 3260 but does not complete the operation, and the TCP probe cannot
// tell those apart. Guessing which one has been wrong repeatedly; the script now
// times itself and this reports it.
//
// Returns "" for a pass fast enough not to care about.
func slowISCSIPhases(phases map[string]int, elapsed int) string {
	if elapsed < slowISCSIPhaseThreshold || len(phases) == 0 {
		return ""
	}
	type kv struct {
		name string
		ms   int
	}
	var all []kv
	for n, ms := range phases {
		all = append(all, kv{n, ms})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ms != all[j].ms {
			return all[i].ms > all[j].ms
		}
		return all[i].name < all[j].name // stable: a message that reorders reads as a new one
	})
	var parts []string
	for _, p := range all {
		if p.ms < 1000 {
			continue
		}
		parts = append(parts, p.name+" "+strconv.Itoa(p.ms/1000)+"s")
	}
	if len(parts) == 0 {
		return ""
	}
	return "this iSCSI step took " + strconv.Itoa(elapsed/1000) + "s, which is most of a reconcile pass and is why anything after it may not have run — " +
		strings.Join(parts, ", ")
}

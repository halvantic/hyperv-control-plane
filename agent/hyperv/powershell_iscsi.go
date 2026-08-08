package hyperv

import (
	"context"
	"fmt"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
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
	MPIOInstalled bool
	// MPIOClaimed reports that MPIO is actually claiming iSCSI devices. Installed
	// is not the same thing: the feature can be present with no bus type claimed,
	// which protects nothing while looking configured.
	MPIOClaimed bool
	Disks       []ISCSIDiskState
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
func iscsiScript(spec types.ISCSIStorageSpec, wantMPIO bool) string {
	var b strings.Builder
	b.WriteString(`$ErrorActionPreference = 'Stop'
$out = [ordered]@{ initiatorIQN=''; serviceRunning=$false; portals=@(); sessions=@(); mpioInstalled=$false; mpioClaimed=$false; disks=@(); rebootRequired=$false; message='' }
$changed = $false

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
try { $out.initiatorIQN = [string](Get-InitiatorPort -ErrorAction Stop | Where-Object { $_.ConnectionType -eq 'iSCSI' } | Select-Object -First 1).NodeAddress } catch {}
if (-not $out.initiatorIQN) {
  try { $out.initiatorIQN = [string](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\iSCSI' -ErrorAction Stop).NodeName } catch {}
}
`)

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
if ($out.mpioInstalled -and -not $out.rebootRequired) {
  # Claim iSCSI devices for MPIO. Idempotent: already-claimed is not an error
  # worth failing the pass for, and the reboot flag above is what actually gates
  # safety.
  try {
    $claimed = (Get-MSDSMSupportedHW -ErrorAction SilentlyContinue | Where-Object { $_.BusType -eq 'iSCSI' })
    if (-not $claimed) { Enable-MSDSMAutomaticClaim -BusType iSCSI -ErrorAction Stop; $changed = $true }
  } catch {}
}
try { $out.mpioClaimed = [bool](Get-MSDSMSupportedHW -ErrorAction SilentlyContinue | Where-Object { $_.BusType -eq 'iSCSI' }) } catch {}
`)
	} else {
		b.WriteString("$out.mpioInstalled = [bool](Get-WindowsFeature -Name Multipath-IO -ErrorAction SilentlyContinue).Installed\n")
	}

	// Portals. New-IscsiTargetPortal on an existing portal throws, so check.
	b.WriteString("\n$portals = @(" + psStringList(spec.Portals) + ")\n")
	b.WriteString(`foreach ($p in $portals) {
  $addr = $p; $port = 3260
  if ($p -match '^(.+):(\d+)$') { $addr = $Matches[1]; $port = [int]$Matches[2] }
  $have = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object { $_.TargetPortalAddress -eq $addr -and [int]$_.TargetPortalPortNumber -eq $port })
  if ($have.Count -eq 0) {
    New-IscsiTargetPortal -TargetPortalAddress $addr -TargetPortalPortNumber $port -ErrorAction Stop | Out-Null
    $changed = $true
  }
}
# Refresh so newly registered portals advertise their targets before we log in.
try { Update-IscsiTarget -ErrorAction SilentlyContinue } catch {}
$out.portals = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.TargetPortalAddress + ':' + [string]$_.TargetPortalPortNumber })
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
$mpioEffective = ($out.mpioInstalled -and $out.mpioClaimed -and -not $out.rebootRequired)
$restrictPortal = ''
if (-not $mpioEffective -and $portals.Count -gt 1) {
  $first = $portals[0]
  if ($first -match '^(.+):(\d+)$') { $first = $Matches[1] }
  $restrictPortal = $first
  $out.message = 'multipath is not in effect yet, so this node is logged in through ' + $restrictPortal + ' only; the remaining paths are added once the node has restarted'
}
`)
	} else {
		b.WriteString("$restrictPortal = ''\n")
	}

	// Targets: explicit list, or everything advertised.
	b.WriteString("\n$wanted = @(" + psStringList(spec.Targets) + ")\n")
	b.WriteString(`if ($wanted.Count -eq 0) {
  # No explicit list: log in to everything the portals advertise. Convenient for
  # a dedicated array and wrong for a shared one, which is why the spec makes it
  # a deliberate choice rather than a default anybody arrives at by accident.
  $wanted = @(Get-IscsiTarget -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.NodeAddress })
}
foreach ($t in $wanted) {
  $existing = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object { $_.TargetNodeAddress -eq $t })
  if ($existing.Count -gt 0) {
    # Already logged in. A session that is NOT persistent will vanish at the next
    # reboot and take this node's disks with it, so make it persistent rather
    # than leaving a working-until-restarted state in place.
    foreach ($s in $existing) {
      if (-not $s.IsPersistent) {
        try { Register-IscsiSession -SessionIdentifier $s.SessionIdentifier -ErrorAction Stop; $changed = $true } catch {}
      }
    }
    continue
  }
`)
	// The connect call, with CHAP only when a credential was supplied. The secret
	// arrives via the environment so it is never in the script text or a log.
	// Splatted rather than concatenated so the single-path restriction is one
	// optional key instead of a second copy of the whole call with its CHAP
	// arguments — two spellings of the same login is how they drift apart.
	connect := "  $c = @{ NodeAddress = $t; IsPersistent = $true }\n" +
		"  if ($restrictPortal) { $c['TargetPortalAddress'] = $restrictPortal }\n"
	if spec.CredentialSecret != "" {
		auth := "ONEWAYCHAP"
		if spec.MutualCHAP {
			auth = "MUTUALCHAP"
		}
		// One -ChapSecret, whichever direction. Mutual CHAP additionally needs the
		// initiator's own secret set once on the host (Set-IscsiChapSecret); it is
		// NOT a second -ChapSecret here, which the cmdlet rejects outright.
		connect += "  $c['AuthenticationType'] = '" + auth + "'\n" +
			"  $c['ChapUsername'] = $env:BALLAST_CHAP_USER\n" +
			"  $c['ChapSecret'] = $env:BALLAST_CHAP_SECRET\n"
	}
	b.WriteString(connect + "  Connect-IscsiTarget @c -ErrorAction Stop | Out-Null\n  $changed = $true\n}\n")

	b.WriteString(`
$out.sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Group-Object TargetNodeAddress | ForEach-Object {
  $first = $_.Group[0]
  [pscustomobject]@{
    targetIQN  = [string]$_.Name
    connected  = $true
    persistent = [bool]($_.Group | Where-Object { $_.IsPersistent } | Select-Object -First 1)
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

# Disks arriving over iSCSI. The SERIAL is what identifies a LUN cluster-wide;
# the disk number is per-node and moves across reboots, so it is reported for
# display only and never used to bind a volume.
$out.disks = @(Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.BusType -eq 'iSCSI' } | ForEach-Object {
  [pscustomobject]@{
    serialNumber = ([string]$_.SerialNumber).Trim()
    number       = [int]$_.Number
    sizeBytes    = [uint64]$_.Size
    targetIQN    = ''
    lun          = -1
    clustered    = [bool]$_.IsClustered
    offline      = [bool]$_.IsOffline
  }
})
$out.changed = $changed
[pscustomobject]$out | ConvertTo-Json -Compress -Depth 5
`)
	return b.String()
}

// EnsureISCSI connects this node to the cluster's iSCSI storage and reports what
// it sees. Additive only — it never disconnects a session or removes a portal.
func (p *PowerShell) EnsureISCSI(ctx context.Context, spec types.ISCSIStorageSpec, chapUser, chapSecret string) (ISCSIState, Outcome, error) {
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

	out, err := p.runWithEnvOut(ctx, iscsiScript(spec, wantMPIO), env)
	if err != nil {
		return st, OutcomeUnchanged, fmt.Errorf("ensure iscsi: %w", err)
	}
	var res struct {
		InitiatorIQN   string   `json:"initiatorIQN"`
		ServiceRunning bool     `json:"serviceRunning"`
		Portals        []string `json:"portals"`
		MPIOInstalled  bool     `json:"mpioInstalled"`
		MPIOClaimed    bool     `json:"mpioClaimed"`
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

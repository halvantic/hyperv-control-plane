package hyperv

import (
	"context"
	"fmt"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

type clusterOwnedObs struct {
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	State     string `json:"state"`
	GroupType string `json:"groupType,omitempty"`
}

// clusterCSVObs is a CSV plus the health of the virtual disk behind it, which is
// observed separately from the pool's — the two fail independently.
type clusterCSVObs struct {
	Name           string `json:"name"`
	Owner          string `json:"owner"`
	State          string `json:"state"`
	Health         string `json:"health"`
	Operational    string `json:"operational"`
	DetachedReason string `json:"detachedReason"`
	SizeBytes      uint64 `json:"sizeBytes"`
	FreeBytes      uint64 `json:"freeBytes"`
}

type clusterObservation struct {
	Exists     bool                `json:"exists"`
	Known      bool                `json:"known"`
	Reason     string              `json:"reason"`
	Name       string              `json:"name"`
	Members    []string            `json:"members"`
	Nodes      []clusterOwnedObs   `json:"nodes"`
	Groups     []clusterOwnedObs   `json:"groups"`
	CSVs       []clusterCSVObs     `json:"csvs"`
	ClusterVMs []clusterOwnedObs   `json:"clustervms"`
	Pool       *clusterPoolObs     `json:"pool"`
	Networks   []clusterNetworkObs `json:"networks"`
	Witness    *clusterWitnessObs  `json:"witness"`
	Broker     *clusterBrokerObs   `json:"replicaBroker"`
	FuncLevel  int                 `json:"functionalLevel"`
	NodeBuild  int                 `json:"nodeBuild"`
}

type clusterBrokerObs struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	StorageLocation string `json:"storageLocation"`
}

type clusterWitnessObs struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	State      string `json:"state"`
	QuorumType string `json:"quorumType"`
}

type clusterPoolObs struct {
	Name            string `json:"name"`
	RawBytes        uint64 `json:"rawBytes"`
	AllocatedBytes  uint64 `json:"allocatedBytes"`
	Health          string `json:"health"`
	Operational     string `json:"operational"`
	UnhealthyDisks  int    `json:"unhealthyDisks"`
	DisksInMaint    int    `json:"disksInMaintenance"`
	TotalDisks      int    `json:"totalDisks"`
	Resyncing       bool   `json:"resyncing"`
	ResyncPercent   int    `json:"resyncPercent"`
	ResyncJob       string `json:"resyncJob"`
	ResyncRemaining uint64 `json:"resyncRemaining"`
}

type clusterNetworkObs struct {
	Name   string `json:"name"`
	CIDR   string `json:"cidr"`
	Role   string `json:"role"`
	State  string `json:"state"`
	Metric int    `json:"metric"`
}

// clusterStateScript observes membership plus clustered groups/roles and CSV
// ownership. @(...) guards a single element collapsing to an object, and -Depth
// keeps the nested arrays in the JSON.
const clusterStateScript = `
$ErrorActionPreference = 'Stop'
$getClusterErr = ''
$c = $null
try { $c = Get-Cluster -ErrorAction Stop } catch { $getClusterErr = ([string]$_.Exception.Message).Trim() }
if (-not $c) {
  # Get-Cluster returned nothing — but is the node truly un-clustered, or was the
  # cluster service just momentarily unavailable (starting, mid-operation)? Report
  # exists+unknown for the latter so the reconciler neither forms nor clobbers
  # status this pass.
  #
  # Membership evidence must SURVIVE THE CLUSTER SERVICE BEING DOWN. HKLM:\Cluster
  # is the cluster database hive MOUNTED BY ClusSvc at startup, so it is absent on
  # a node that is joined but whose service is stopped, crashed or still starting —
  # and this branch then declares, with certainty, that the machine has never been
  # in a cluster. Observed on the rig 2026-08-06: ClusSvc sat in StartPending on
  # two of three nodes, and the designated former ran New-Cluster against the live
  # cluster it was already a member of.
  #
  # That attempt failed only because New-Cluster does its own check. Had the
  # service been down on every member, nothing would have refused, and a second
  # cluster forming over a live S2D pool loses data. So the asymmetry decides the
  # design: being wrong about "clustered" costs one deferred pass, being wrong
  # about "not clustered" costs the pool.
  #
  # These two persist on disk and in the registry regardless of service state:
  #   C:\Windows\Cluster\CLUSDB                      the cluster database itself
  #   ...\Services\ClusSvc\Parameters\ClusterName    written when the node joins
  $joined = $false
  if (Test-Path 'HKLM:\Cluster') { $joined = $true }
  if ((-not $joined) -and (Test-Path (Join-Path $env:SystemRoot 'Cluster\CLUSDB'))) { $joined = $true }
  if (-not $joined) {
    $pn = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\ClusSvc\Parameters' -ErrorAction SilentlyContinue).ClusterName
    if ($pn) { $joined = $true }
  }
  if ($joined) {
    # Say WHY, not only that it could not be read. "Unreadable this pass" is
    # equally true of a service still starting and of a node the cluster has
    # QUARANTINED — and only one of those clears itself. The cluster service's
    # own state and this node's membership state distinguish them, and both are
    # readable without the cluster being reachable.
    $svc = ''
    try { $svc = [string](Get-Service ClusSvc -ErrorAction Stop).Status } catch { $svc = 'unreadable' }
    $me = ''
    try { $me = [string](Get-ClusterNode -Name $env:COMPUTERNAME -ErrorAction SilentlyContinue).State } catch {}
    $why = @()
    if ($getClusterErr) { $why += $getClusterErr }
    $why += ('cluster service is ' + $svc)
    if ($me) { $why += ('this node reports itself as ' + $me) }
    [pscustomobject]@{ exists = $true; known = $false; reason = ($why -join '; ') } | ConvertTo-Json -Compress
    return
  }
  [pscustomobject]@{ exists = $false; known = $true } | ConvertTo-Json -Compress; return
}
$nodeObjs = @(Get-ClusterNode -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; state = [string]$_.State } })
$nodes = @($nodeObjs | ForEach-Object { $_.name })
$groups = @(Get-ClusterGroup -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; owner = [string]$_.OwnerNode; state = [string]$_.State; groupType = [string]$_.GroupType } })
# Volume health is observed separately from pool health: the two fail
# independently, and attributing a volume's problem to the pool sends the
# operator to repair storage that is fine. A CSV is named "Cluster Virtual Disk
# (<volume>)", so match it back to its virtual disk by that inner name.
$vds = @{}
foreach ($vd in @(Get-VirtualDisk -ErrorAction SilentlyContinue)) {
  $vds[[string]$vd.FriendlyName] = $vd
}
$csvs = @(Get-ClusterSharedVolume -ErrorAction SilentlyContinue | ForEach-Object {
  $csvName = [string]$_.Name
  $vdName = $csvName
  if ($csvName -match '\(([^)]+)\)\s*$') { $vdName = $Matches[1] }
  $vd = $vds[$vdName]
  $h = ''; $op = ''; $dr = ''
  if ($vd) {
    $h = [string]$vd.HealthStatus
    $op = [string]($vd.OperationalStatus -join ',')
    $dr = [string]$vd.DetachedReason
  }
  # Size comes from the VOLUME, not the backing virtual disk, and the difference
  # is the whole point of reporting it. Growing a volume is two steps — grow the
  # virtual disk, then extend the partition into it — and a half-done resize
  # leaves the virtual disk larger while the usable space is unchanged. Reporting
  # the virtual disk's size would confirm a resize that never reached the
  # filesystem.
  $sz = [uint64]0; $free = [uint64]0
  $info = $_.SharedVolumeInfo
  if ($info -and $info.Partition) {
    $sz = [uint64]$info.Partition.Size
    $free = [uint64]$info.Partition.FreeSpace
  }
  [pscustomobject]@{ name = $csvName; owner = [string]$_.OwnerNode; state = [string]$_.State; health = $h; operational = $op; detachedReason = $dr; sizeBytes = $sz; freeBytes = $free } })
$cvms = @(Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { $_.GroupType -eq 'VirtualMachine' } | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; owner = [string]$_.OwnerNode; state = [string]$_.State } })
$sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
$pool = if ($sp) {
  $pd = @(Get-PhysicalDisk -StoragePool $sp -ErrorAction SilentlyContinue)
  # Disks the cluster has taken into storage maintenance mode are counted
  # SEPARATELY, not as failures. Suspend-ClusterNode -Drain puts a node's disks
  # there and Resume takes them back out, and while they are out they report
  # HealthStatus Warning — so counting on health alone turned every planned
  # drain into "4 of 12 disks unhealthy · Repair pool" beside a pool Windows
  # called Healthy. The two states have opposite remedies (replace a disk versus
  # resume a node), so they must not share a number.
  $maint = @($pd | Where-Object { @($_.OperationalStatus) -contains 'In Maintenance Mode' })
  $maintIds = @($maint | ForEach-Object { [string]$_.UniqueId })
  $bad = @($pd | Where-Object { $_.HealthStatus -ne 'Healthy' -and $maintIds -notcontains [string]$_.UniqueId }).Count
  # Observe whether S2D is actively rebuilding rather than inferring "broken"
  # from health alone. A repair/regeneration job makes the pool, its virtual
  # disks and some physical disks report non-Healthy while it runs — that is
  # normal, self-resolving work, not a fault needing operator action. Reporting
  # the job is what lets the console say "rebuilding, 12%" instead of telling
  # someone to repair a pool that is already repairing itself.
  # No "| Select-Object -First 1": the -First pipeline stop can abort the whole
  # script after a storage cmdlet, exiting 0 with no output marker.
  # A SUSPENDED job is not a rebuild. S2D queues repair jobs and suspends them
  # while a node is merely paused — they sit at 0 bytes processed and do nothing
  # until the node is gone for good. Counting them as "rebuilding" reported a
  # rebuild that was permanently at 0%, which is the same false-progress the
  # PercentComplete fix removed, and it fired storage alarms every time an
  # operator drained a node deliberately.
  $rjobs = @(Get-StorageJob -ErrorAction SilentlyContinue | Where-Object {
    [string]$_.JobState -in @('Running','Starting') -and
    ([string]$_.Name -match 'Repair|Regener|Resync|Rebalance|Optimi')
  })
  $resync = ($rjobs.Count -gt 0)
  $rpct = 0; $rname = ''; $rrem = [uint64]0
  if ($resync) {
    # Progress comes from the BYTE counters, not PercentComplete. Storage Spaces
    # leaves PercentComplete at 0 for the whole of a running repair on many
    # builds — it is populated for some job types and not others — so reading it
    # reported "rebuilding 0%" for the entire rebuild and made the figure
    # worthless. BytesProcessed/BytesTotal are populated; summing them across the
    # jobs gives the rebuild's real overall progress.
    $tot = 0.0; $don = 0.0
    foreach ($j in $rjobs) {
      if ($j.BytesTotal) { $tot += [double]$j.BytesTotal }
      if ($j.BytesProcessed) { $don += [double]$j.BytesProcessed }
    }
    # Outstanding work. The percentage cannot express overall progress: a
    # finished job disappears from Get-StorageJob, so the next one starts the
    # count again — seen live going 21% to 84% to 0. This figure IS comparable
    # across jobs. It trends down across a rebuild's phases, and stays put when a
    # repair keeps restarting and retaining nothing, which is the one thing that
    # tells those two apart.
    if ($tot -gt $don) { $rrem = [uint64]($tot - $don) }
    if ($tot -gt 0) {
      $rpct = [int][math]::Round(($don / $tot) * 100)
    } else {
      # No byte counters: fall back to the LEAST-progressed job, so several jobs
      # report the work still outstanding rather than one that has finished.
      $pcts = @($rjobs | ForEach-Object { [int]$_.PercentComplete })
      if ($pcts.Count -gt 0) { $rpct = [int](($pcts | Measure-Object -Minimum).Minimum) }
    }
    if ($rpct -lt 0) { $rpct = 0 }
    if ($rpct -gt 100) { $rpct = 100 }
    # Job names look like "<volume>-Repair"; report the kind, not the volume.
    $rname = [string]$rjobs[0].Name
    if ($rname -match '-([A-Za-z]+)$') { $rname = $Matches[1] }
  }
  [pscustomobject]@{ name = [string]$sp.FriendlyName; rawBytes = [uint64]$sp.Size; allocatedBytes = [uint64]$sp.AllocatedSize; health = [string]$sp.HealthStatus; operational = ([string]($sp.OperationalStatus -join ',')); unhealthyDisks = [int]$bad; disksInMaintenance = [int]$maint.Count; totalDisks = [int]$pd.Count; resyncing = $resync; resyncPercent = $rpct; resyncJob = $rname; resyncRemaining = $rrem }
} else { $null }
$nets = @(Get-ClusterNetwork -ErrorAction SilentlyContinue | ForEach-Object {
  $bits = (($_.AddressMask -split '\.') | ForEach-Object { ([Convert]::ToString([int]$_,2)).ToCharArray() } | Where-Object { $_ -eq '1' }).Count
  $role = switch ([int]$_.Role) { 0 { 'None' } 1 { 'Cluster' } 3 { 'ClusterAndClient' } default { [string]$_.Role } }
  # Metric is what decides where cluster and CSV/SMB traffic actually goes:
  # lowest metric wins among the networks enabled for cluster use. Windows
  # assigns it automatically (preferring networks with no gateway), so the role
  # alone never answers "which network is storage on".
  [pscustomobject]@{ name = [string]$_.Name; cidr = ([string]$_.Address + '/' + $bits); role = $role; state = [string]$_.State; metric = [int]$_.Metric } })
# Quorum. A cluster with no witness reports type None — an answer, not an
# absence — because "three nodes and no witness" is precisely the configuration
# worth telling someone about, and it is indistinguishable from a healthy
# cluster if quorum is never actually read.
#
# The witness resource's State is captured separately from its existence: a
# witness configured against a share that has gone away sits Offline and does
# not vote, and that only becomes apparent when a node is already lost.
$witness = $null
$q = Get-ClusterQuorum -ErrorAction SilentlyContinue
if ($q) {
  $wtype = 'None'; $wpath = ''; $wstate = ''
  $wr = $q.QuorumResource
  if ($wr) {
    $wstate = [string]$wr.State
    $rt = [string]$wr.ResourceType
    if ($rt -like '*File Share Witness*')  { $wtype = 'FileShare' }
    elseif ($rt -like '*Cloud Witness*')   { $wtype = 'Cloud' }
    elseif ($rt -like '*Physical Disk*')   { $wtype = 'Disk' }
    else { $wtype = $rt }
    # SharePath for a file share witness, AccountName for a cloud one. Both are
    # cluster parameters on the resource rather than properties of it.
    $sp2 = ($wr | Get-ClusterParameter -Name SharePath -ErrorAction SilentlyContinue).Value
    if ($sp2) { $wpath = [string]$sp2 }
    if (-not $wpath) {
      $an = ($wr | Get-ClusterParameter -Name AccountName -ErrorAction SilentlyContinue).Value
      if ($an) { $wpath = [string]$an }
    }
  }
  $witness = [pscustomobject]@{ type = $wtype; path = $wpath; state = $wstate; quorumType = [string]$q.QuorumType }
}
# The Hyper-V Replica Broker is how a cluster sends or receives replication, and
# it is observed for the same reason the witness is: it can exist on the cluster
# while nothing in Ballast declares it, and then Ballast neither enforces nor
# reports a live replication endpoint. Seen on the rig 2026-08-06 — the broker
# was Online with its client access point and IP, while ClusterSpec.ReplicaBroker
# was null.
#
# The storage location is not a property of the broker resource: it lives in the
# per-node replication authorization entries, and it is the setting that actually
# decides where an incoming replica lands. Reporting it is what makes "the
# replica has nowhere to go" visible before a relationship fails — the rig's
# entry pointed at C:\ClusterStorage\DS1\Replica long after that volume existed.
$broker = $null
$br = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Virtual Machine Replication Broker' })[0]
if ($br) {
  # The operator-facing name is the client access point (the Network Name in the
  # broker's group), not the resource's own name, because that is what a primary
  # addresses replication to.
  $bname = ''
  try {
    $bg = [string]$br.OwnerGroup
    $nn = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Network Name' -and [string]$_.OwnerGroup -eq $bg })[0]
    if ($nn) { $bname = [string]($nn | Get-ClusterParameter -Name DnsName -ErrorAction SilentlyContinue).Value }
    if (-not $bname) { $bname = $bg }
  } catch {}
  $bloc = ''
  try {
    $ae = @(Get-VMReplicationAuthorizationEntry -ErrorAction SilentlyContinue)[0]
    if ($ae) { $bloc = [string]$ae.ReplicaStorageLocation }
    if (-not $bloc) { $bloc = [string](Get-VMReplicationServer -ErrorAction SilentlyContinue).DefaultStorageLocation }
  } catch {}
  $broker = [pscustomobject]@{ name = $bname; state = [string]$br.State; storageLocation = $bloc }
}
# ClusterFunctionalLevel is the cluster's operating mode, and it is the one thing
# a rolling OS upgrade does NOT raise by itself. Take a cluster to a newer
# Windows node by node — which is exactly what cluster-aware updating does — and
# when the last node returns the cluster still runs at the old level: the new
# OS's features stay unavailable and the upgrade is not actually finished until
# Update-ClusterFunctionalLevel is run. That is deliberately manual because it
# cannot be undone.
#
# Reported so the console can state it instead of showing a placeholder. The
# highest level any node could support comes from the node build, so the two
# together say whether an upgrade was completed or merely performed.
$flevel = 0
try { $flevel = [int]$c.ClusterFunctionalLevel } catch {}
$nodeBuild = 0
try { $nodeBuild = [int](Get-CimInstance Win32_OperatingSystem -ErrorAction SilentlyContinue).BuildNumber } catch {}
[pscustomobject]@{ exists = $true; known = $true; name = [string]$c.Name; members = @($nodes); nodes = @($nodeObjs); groups = @($groups); csvs = @($csvs); clustervms = @($cvms); pool = $pool; networks = @($nets); witness = $witness; replicaBroker = $broker; functionalLevel = $flevel; nodeBuild = $nodeBuild } | ConvertTo-Json -Compress -Depth 4
`

// witnessScript applies a file-share witness, and only when it differs from
// what is already configured.
//
// Idempotency matters more than usual here: Set-ClusterQuorum is not a no-op
// when it re-applies the same value — it tears the witness resource down and
// recreates it, which momentarily drops a vote. Doing that on every reconcile
// pass would put a recurring quorum wobble into a cluster that was fine.
//
// Paths are compared case-insensitively with trailing slashes ignored, because
// Windows stores the path as given and an operator writing the same share two
// ways must not cause a rebuild every pass.
const witnessScript = `
$ErrorActionPreference = 'Stop'
$desired = %s
$changed = $false
$q = Get-ClusterQuorum -ErrorAction Stop
$curPath = ''
$curType = 'None'
if ($q.QuorumResource) {
  $rt = [string]$q.QuorumResource.ResourceType
  if ($rt -like '*File Share Witness*') { $curType = 'FileShare' }
  elseif ($rt -like '*Cloud Witness*')  { $curType = 'Cloud' }
  elseif ($rt -like '*Physical Disk*')  { $curType = 'Disk' }
  else { $curType = $rt }
  $v = ($q.QuorumResource | Get-ClusterParameter -Name SharePath -ErrorAction SilentlyContinue).Value
  if ($v) { $curPath = [string]$v }
}
function Norm([string]$p) { return ($p.TrimEnd('\','/')).ToLowerInvariant() }

if ($desired -eq '') {
  # Declared None: fall back to node majority, but only if a witness is set.
  if ($curType -ne 'None') {
    Set-ClusterQuorum -NodeMajority -ErrorAction Stop | Out-Null
    $changed = $true
  }
} elseif ($curType -ne 'FileShare' -or (Norm $curPath) -ne (Norm $desired)) {
  $server = ''; $share = ''
  if ($desired -match '^\\\\([^\\]+)\\([^\\]+)') { $server = $matches[1]; $share = $matches[2] }
  if (-not $server) { throw ("the witness path " + $desired + " is not a \\server\share path") }
  $cno = ''
  try { $cno = [string](Get-Cluster -ErrorAction Stop).Name } catch {}

  try {
    Set-ClusterQuorum -FileShareWitness $desired -ErrorAction Stop | Out-Null
  } catch {
    # Set-ClusterQuorum reports the same code for a server that is not there, a
    # share this node cannot read, and a server that will not accept the
    # cluster's permission grant. Those have three different remedies, so
    # establish which one it is instead of passing the code on.
    $raw = [string]$_.Exception.Message
    $up = $false
    try { $up = Test-NetConnection -ComputerName $server -Port 445 -InformationLevel Quiet -WarningAction SilentlyContinue } catch {}
    if (-not $up) { throw ('the file server ' + $server + ' is not reachable on SMB (tcp/445) from this node, so the witness cannot be configured. ' + $raw) }
    $readable = $false
    try { $readable = [bool](Test-Path -LiteralPath $desired -ErrorAction SilentlyContinue) } catch {}
    # The file server is somebody else's to administer — Ballast has no agent on
    # it — so name the one step rather than failing obscurely. A NAS appliance is
    # the usual case: it serves SMB but does not accept remote share-permission
    # changes, so the cluster's own grant cannot succeed and the access has to be
    # given on the appliance.
    if (-not $readable) {
      throw ('the share ' + $desired + ' answers on SMB but this node cannot read it. Grant the cluster computer account ' + $cno + '$ read/write access to the share on the file server itself, then retry. ' + $raw)
    }
    if ($raw -match '\b67\b') {
      # Configuring a witness makes the cluster add its own computer account to
      # the share's permissions. A non-Windows file server (a NAS appliance) does
      # not accept remote share-permission changes, so that grant cannot succeed
      # and the share has to be permissioned on the appliance instead. The code
      # is reported as "unexpected error code 67", which reads like the share is
      # missing when it is sitting right there — and the share being readable is
      # exactly what proves it is not missing.
      $hint = ''
      if ($server -as [ipaddress]) { $hint = ' Addressing the file server by name rather than by IP also matters, because the grant authenticates with Kerberos.' }
      throw ('the share ' + $desired + ' exists and is readable from this node, so the path is right — but ' + $server + ' refused the cluster''s attempt to grant itself access to it. Grant the cluster computer account ' + $cno + '$ read/write permission on that share on ' + $server + ' itself (Ballast has no agent there and cannot do it), then retry. If another cluster already has a working witness on this server, copy that share''s permissions.' + $hint + ' ' + $raw)
    }
    throw $raw
  }
  $changed = $true
}
[pscustomobject]@{ changed = $changed } | ConvertTo-Json -Compress
`

// EnsureClusterWitness makes the cluster's quorum witness match w. FileShare and
// None are applied; a disk witness is applied only where the cluster actually has
// shared block storage. Anything else is refused with a reason rather than
// half-attempted.
//
// kind is the cluster's storage model, because whether a disk witness is even
// possible depends on it. This used to be a flat refusal, which was correct while
// S2D was the only kind and became wrong the moment iSCSI existed — a disk
// witness is the classic choice on array-backed storage.
//
// Run by the former only. Every member can see the cluster, so without that
// gate all of them would race to set the same witness and each would see the
// others' write as drift.
func (p *PowerShell) EnsureClusterWitness(ctx context.Context, w types.WitnessSpec, kind types.ClusterStorageKind) (Outcome, error) {
	var path string
	switch w.Type {
	case types.WitnessFileShare:
		if strings.TrimSpace(w.FileSharePath) == "" {
			return OutcomeUnchanged, fmt.Errorf("file share witness needs a share path")
		}
		path = strings.TrimSpace(w.FileSharePath)
	case types.WitnessNone:
		path = ""
	case types.WitnessDisk:
		// A disk witness needs shared block storage every node can attach. S2D has
		// none — the refusal is not a limitation of Ballast's, and attempting it
		// fails obscurely inside Set-ClusterQuorum, so say why here instead.
		if kind == types.StorageKindS2D {
			return OutcomeUnchanged, fmt.Errorf("a disk witness needs shared block storage and cannot be used with Storage Spaces Direct; use a file share or cloud witness")
		}
		if kind != types.StorageKindISCSI {
			return OutcomeUnchanged, fmt.Errorf("a disk witness needs shared block storage, and this cluster does not declare any; set the cluster's storage kind first, or use a file share witness")
		}
		if w.Disk == nil || (strings.TrimSpace(w.Disk.SerialNumber) == "" && strings.TrimSpace(w.Disk.TargetIQN) == "") {
			return OutcomeUnchanged, fmt.Errorf("a disk witness needs the witness LUN identified: give the disk's serial number (or the target it is presented on) on the witness")
		}
		return p.ensureDiskWitness(ctx, *w.Disk)
	case types.WitnessCloud:
		return OutcomeUnchanged, fmt.Errorf("cloud witness is observed but not yet applied by Ballast; set it with Set-ClusterQuorum -CloudWitness")
	default:
		return OutcomeUnchanged, fmt.Errorf("unknown witness type %q", w.Type)
	}

	out, err := p.run(ctx, fmt.Sprintf(witnessScript, psQuote(path)))
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure cluster witness: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure cluster witness: %w", err)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

func (p *PowerShell) GetClusterState(ctx context.Context) (ClusterState, error) {
	out, err := p.run(ctx, clusterStateScript)
	if err != nil {
		return ClusterState{}, fmt.Errorf("get cluster state: %w", err)
	}
	var obs clusterObservation
	if err := decodeJSON(out, &obs); err != nil {
		return ClusterState{}, fmt.Errorf("get cluster state: %w", err)
	}
	groups := make([]ClusterGroup, 0, len(obs.Groups))
	for _, g := range obs.Groups {
		groups = append(groups, ClusterGroup{Name: g.Name, OwnerNode: g.Owner, State: g.State, GroupType: g.GroupType})
	}
	csvs := make([]ClusterCSV, 0, len(obs.CSVs))
	for _, v := range obs.CSVs {
		csvs = append(csvs, ClusterCSV{Name: v.Name, OwnerNode: v.Owner, State: v.State,
			Health: v.Health, Operational: v.Operational, DetachedReason: v.DetachedReason,
			SizeBytes: v.SizeBytes, FreeBytes: v.FreeBytes})
	}
	cvms := make([]ClusterVM, 0, len(obs.ClusterVMs))
	for _, v := range obs.ClusterVMs {
		cvms = append(cvms, ClusterVM{Name: v.Name, OwnerNode: v.Owner, State: v.State})
	}
	nodes := make([]ClusterNodeState, 0, len(obs.Nodes))
	for _, n := range obs.Nodes {
		nodes = append(nodes, ClusterNodeState{Name: n.Name, State: n.State})
	}
	var pool *ClusterPool
	if obs.Pool != nil {
		pool = &ClusterPool{Name: obs.Pool.Name, RawBytes: obs.Pool.RawBytes, AllocatedBytes: obs.Pool.AllocatedBytes,
			Health: obs.Pool.Health, Operational: obs.Pool.Operational, UnhealthyDisks: obs.Pool.UnhealthyDisks,
			DisksInMaintenance: obs.Pool.DisksInMaint, TotalDisks: obs.Pool.TotalDisks,
			Resyncing: obs.Pool.Resyncing, ResyncPercent: obs.Pool.ResyncPercent, ResyncJob: obs.Pool.ResyncJob,
			ResyncRemainingBytes: obs.Pool.ResyncRemaining}
	}
	netw := make([]ClusterNetworkInfo, 0, len(obs.Networks))
	for _, n := range obs.Networks {
		netw = append(netw, ClusterNetworkInfo{Name: n.Name, CIDR: n.CIDR, Role: n.Role, State: n.State, Metric: n.Metric})
	}
	var witness *ClusterWitness
	if obs.Witness != nil {
		witness = &ClusterWitness{Type: obs.Witness.Type, Path: obs.Witness.Path,
			State: obs.Witness.State, QuorumType: obs.Witness.QuorumType}
	}
	var broker *ClusterReplicaBroker
	if obs.Broker != nil {
		broker = &ClusterReplicaBroker{Name: obs.Broker.Name, State: obs.Broker.State,
			StorageLocation: obs.Broker.StorageLocation}
	}
	return ClusterState{Exists: obs.Exists, Known: obs.Known, UnknownReason: obs.Reason, Name: obs.Name, Members: obs.Members, Nodes: nodes, Groups: groups, CSVs: csvs, VMs: cvms, Pool: pool, Networks: netw, Witness: witness, ReplicaBroker: broker,
		FunctionalLevel: obs.FuncLevel, NodeOSBuild: obs.NodeBuild}, nil
}

const installClusteringScript = `
$ErrorActionPreference = 'Stop'
$r = Install-WindowsFeature -Name Failover-Clustering -IncludeManagementTools
$changed = (@($r.FeatureResult) | Measure-Object).Count -gt 0
[pscustomobject]@{ changed = $changed } | ConvertTo-Json -Compress
`

// clusterFirewallScript enables the inbound firewall rule groups a cluster node
// needs to coordinate with peers. WMI carries the RPC calls cross-node cluster
// operations make (e.g. Add-ClusterVirtualMachineRole); without it they fail
// "RPC server unavailable". Enabling an already-enabled rule is a no-op.
const clusterFirewallScript = `
$ErrorActionPreference = 'Stop'
$groups = @('Failover Clusters','Windows Management Instrumentation (WMI)','Remote Event Log Management')
$changed = 0
foreach ($g in $groups) {
  # Enable the rule group AND make it apply on every profile: a cluster node's
  # management NIC sometimes sits on the Public profile (e.g. when the DC isn't
  # detected on it), and Domain/Private-scoped rules then don't apply, so cross-
  # node RPC/WMI (Add-ClusterVirtualMachineRole etc.) fails 'RPC server unavailable'.
  $rules = Get-NetFirewallRule -DisplayGroup $g -ErrorAction SilentlyContinue
  foreach ($r in $rules) {
    if ($r.Enabled -ne 'True' -or $r.Profile -ne 'Any') {
      Set-NetFirewallRule -Name $r.Name -Enabled True -Profile Any -ErrorAction SilentlyContinue
      $changed++
    }
  }
}
[pscustomobject]@{ changed = ($changed -gt 0) } | ConvertTo-Json -Compress
`

// UpdateClusterFunctionalLevel raises the cluster's operating mode to what its
// nodes now support. Run on the former, after a rolling OS upgrade.
//
// Irreversible, and it is a job rather than anything the reconcile loop does:
// once raised, the cluster cannot go back, and a node still running the older
// Windows can no longer join. So the decision is the operator's, and the only
// thing Ballast does automatically is notice that it is outstanding.
func (p *PowerShell) UpdateClusterFunctionalLevel(ctx context.Context) (string, error) {
	script := `$ErrorActionPreference = 'Stop'
$c = Get-Cluster -ErrorAction Stop
$before = [int]$c.ClusterFunctionalLevel
# Already at the highest its nodes support: report it rather than running a
# one-way operation for nothing. Update-ClusterFunctionalLevel is a no-op in that
# case, but saying "already at level N" is the answer the operator wants.
$down = @(Get-ClusterNode -ErrorAction SilentlyContinue | Where-Object { [string]$_.State -ne 'Up' })
if ($down.Count -gt 0) {
  throw ('refusing to raise the functional level while ' + (($down | ForEach-Object { [string]$_.Name }) -join ', ') + ' is not Up. Every node must be running the newer Windows and joined, or it can never rejoin afterwards - this cannot be undone.')
}
Update-ClusterFunctionalLevel -Force -ErrorAction Stop | Out-Null
$after = [int](Get-Cluster -ErrorAction Stop).ClusterFunctionalLevel
if ($after -eq $before) { 'RESULT=cluster functional level is already ' + $before + ' (no change)' }
else { 'RESULT=cluster functional level raised from ' + $before + ' to ' + $after }
`
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("update cluster functional level: %w", err)
	}
	msg := strings.TrimSpace(string(out))
	if i := strings.Index(msg, "RESULT="); i >= 0 {
		msg = strings.TrimSpace(msg[i+len("RESULT="):])
	}
	return msg, nil
}

func (p *PowerShell) EnsureClusterFirewall(ctx context.Context) (Outcome, error) {
	out, err := p.run(ctx, clusterFirewallScript)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure cluster firewall: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure cluster firewall: %w", err)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// EnsureMigrationDelegation sets Kerberos constrained delegation (the migration
// + cifs SPNs) between cluster nodes' computer accounts, which cluster-initiated
// live migration needs when the host auth type is Kerberos. Idempotent: it reads
// each computer's existing msDS-AllowedToDelegateTo and only adds what is
// missing. Run on the former as a domain admin.
//
// Uses System.DirectoryServices (LDAP port 389) rather than the ActiveDirectory
// PowerShell module so it does not depend on AD Web Services (port 9389) being
// reachable. ADWS is often absent or blocked on small lab DCs.
func (p *PowerShell) EnsureMigrationDelegation(ctx context.Context, nodes []string) (Outcome, error) {
	nodeExpr := "@(Get-ClusterNode -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.Name })"
	if len(nodes) > 0 {
		nodeExpr = "@(" + psStringList(nodes) + ")"
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$dom = (Get-CimInstance Win32_ComputerSystem).Domain
$domDn = 'DC=' + (($dom -split '\.') -join ',DC=')
# Locate a DC to connect to directly via LDAP/389.
# Strategy: try Netlogon DC locator first; if that fails (SRV records not
# resolvable on a freshly configured converged host) probe each DNS server
# configured on this host — in a domain environment the DNS servers are
# typically the DCs themselves, so attempting LDAP against each one works.
$dcAddr = $null
try {
  $ctx = [System.DirectoryServices.ActiveDirectory.DirectoryContext]::new(
    [System.DirectoryServices.ActiveDirectory.DirectoryContextType]::Domain, $dom)
  $dcAddr = ([System.DirectoryServices.ActiveDirectory.DomainController]::FindOne($ctx)).IPAddress
} catch {}
if (-not $dcAddr) {
  $candidates = @(Get-DnsClientServerAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object { $_.ServerAddresses } |
    ForEach-Object { $_.ServerAddresses } |
    Select-Object -Unique)
  foreach ($ip in $candidates) {
    try {
      $t = [System.DirectoryServices.DirectorySearcher]::new([ADSI]"LDAP://$ip/$domDn")
      $t.Filter = "(objectClass=domain)"; $t.SizeLimit = 1
      $null = $t.FindOne()
      $dcAddr = $ip; break
    } catch {}
  }
}
if (-not $dcAddr) { throw "cannot locate a domain controller for $dom — check DNS on this host points at a DC" }
# Always include the local host so delegation is configured both ways.
$nodes = (@($env:COMPUTERNAME) + (%[1]s)) | Where-Object { $_ } | Select-Object -Unique
$changed = @()
foreach ($src in $nodes) {
  $want = @()
  foreach ($dst in ($nodes | Where-Object { $_ -ne $src })) {
    $want += 'Microsoft Virtual System Migration Service/' + $dst + '.' + $dom
    $want += 'Microsoft Virtual System Migration Service/' + $dst
    $want += 'cifs/' + $dst + '.' + $dom
    $want += 'cifs/' + $dst
  }
  $srch = [System.DirectoryServices.DirectorySearcher]::new([ADSI]"LDAP://$dcAddr/$domDn")
  $srch.Filter = "(&(objectClass=computer)(sAMAccountName=${src}$))"
  $srch.PropertiesToLoad.Add('msDS-AllowedToDelegateTo') | Out-Null
  $result = $srch.FindOne()
  if (-not $result) { Write-Warning "AD: computer $src not found; skipping"; continue }
  $cur = @($result.Properties['msDS-AllowedToDelegateTo'] | ForEach-Object { [string]$_ })
  [string[]]$missing = @($want | Where-Object { $cur -notcontains $_ } | ForEach-Object { [string]$_ })
  if ($missing.Count -gt 0) {
    $de = $result.GetDirectoryEntry()
    foreach ($spn in $missing) { $de.Properties['msDS-AllowedToDelegateTo'].Add($spn) | Out-Null }
    $de.CommitChanges()
    $de.Dispose()
    $changed += $src
  }
}
[pscustomobject]@{ changed = ($changed.Count -gt 0) } | ConvertTo-Json -Compress`, nodeExpr)
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure migration delegation: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure migration delegation: %w", err)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

func (p *PowerShell) EnsureFailoverClusteringFeature(ctx context.Context) (Outcome, error) {
	out, err := p.run(ctx, installClusteringScript)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("install failover clustering: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("install failover clustering: %w", err)
	}
	if res.Changed {
		return OutcomeCreated, nil
	}
	return OutcomeUnchanged, nil
}

// FormCluster runs New-Cluster on this node, the designated former. -NoStorage
// keeps formation independent of S2D (a later increment). The members are added
// in the same call, so a single former brings up the whole cluster.
// DestroyCluster tears the cluster down from this node (the former): it removes
// every clustered VM role, disables Storage Spaces Direct (destroying the pool
// and CSVs), and removes the failover cluster along with its AD computer object.
// Destructive and idempotent — a no-op when no cluster exists. Runs agent-local
// (cluster cmdlets cannot run over a remote WinRM double-hop).
func (p *PowerShell) DestroyCluster(ctx context.Context) error {
	script := "$ErrorActionPreference='Stop'; Import-Module FailoverClusters; " +
		"if (-not (Get-Cluster -ErrorAction SilentlyContinue)) { 'no cluster'; return }; " +
		"Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { $_.GroupType -eq 'VirtualMachine' } | ForEach-Object { " +
		"Stop-ClusterGroup -Name $_.Name -ErrorAction SilentlyContinue | Out-Null; " +
		"Remove-ClusterGroup -Name $_.Name -RemoveResources -Force -ErrorAction SilentlyContinue }; " +
		"Disable-ClusterStorageSpacesDirect -Confirm:$false -ErrorAction SilentlyContinue; " +
		"Remove-Cluster -Force -CleanupAD; 'destroyed'"
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("destroy cluster: %w", err)
	}
	return nil
}

func (p *PowerShell) FormCluster(ctx context.Context, f ClusterFormation) error {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	// Domain-joined hosts form the cluster with an Active Directory access point
	// (a real cluster name object, the CNO). Clustered Hyper-V live migration is
	// cluster-initiated and so must authenticate with Kerberos against the cluster
	// name; an AD-detached (DNS-only) cluster has no CNO and falls back to NTLM for
	// the cluster name, which makes Kerberos live migration unsupported (the
	// migration fails at "register cluster name in the local user groups", event
	// 20501). The access point is immutable after creation, so it must be chosen
	// correctly here. Workgroup hosts have no AD and fall back to DNS-only.
	b.WriteString("$aap = if ((Get-CimInstance Win32_ComputerSystem).PartOfDomain) { 'ActiveDirectoryAndDns' } else { 'Dns' }\n")
	fmt.Fprintf(&b, "New-Cluster -Name %s -Node %s -NoStorage -AdministrativeAccessPoint $aap -Force",
		psQuote(f.Name), psStringList(f.Members))
	if f.ManagementIP != "" {
		fmt.Fprintf(&b, " -StaticAddress %s", psQuote(f.ManagementIP))
	}
	b.WriteString(" | Out-Null\n")
	// Make the cluster tolerant of transient heartbeat loss under heavy I/O. On a
	// converged/nested cluster an S2D repair can saturate the single network, drop
	// heartbeats, partition nodes, and spiral into a repair storm (repair restarts
	// each time a node flaps). Raising the thresholds lets one repair pass finish
	// instead of flapping. Best-effort so it never fails formation.
	b.WriteString("try { $cl = Get-Cluster; $cl.SameSubnetThreshold = 20; $cl.SameSubnetDelay = 2000; $cl.CrossSubnetThreshold = 20; $cl.CrossSubnetDelay = 4000 } catch {}\n")
	if err := p.run2(ctx, b.String()); err != nil {
		return fmt.Errorf("form cluster %q: %w", f.Name, err)
	}
	return nil
}

// A clustered VM's registration with Hyper-V IS a cluster resource: the "Virtual
// Machine Configuration" resource in the VM's group. When the role is taken
// Offline that resource goes offline too, and the VM is DEREGISTERED from Hyper-V
// on every node — Get-VM cannot see it anywhere, and says so with "Hyper-V was
// unable to find a virtual machine with name X".
//
// That reads exactly like a deleted VM, and it is not: the group, the resources,
// the configuration and the disks are all intact and the VM comes straight back
// when the role is started. It was diagnosed on the live rig after a stop, a
// failed clone and a failed capture all pointed at a VM nobody had touched.
//
// It also creates a catch-22 for any operation that copies a VM's disk. Those
// need the VM Off, because a running VM holds its VHDX open — but for a clustered
// VM, Off means the role is Offline, which means there is no VM to find.
//
// clusteredVMRegisterPrelude resolves it: when Get-VM comes up empty and the VM
// is an offline cluster role, it brings ONLY the configuration resource online.
// That registers the VM with Hyper-V and leaves it Off — the same state a
// clustered VM is in between being created and first started — without starting
// it. clusteredVMRestoreSuffix puts it back afterwards, so the operator's role
// state is unchanged either way.
//
// The scripts using this must place the body between the two, and must define
// nothing named $__vm, $__broughtOnline or $__cfg of their own.
func clusteredVMRegisterPrelude(vmVar string) string {
	return `
$global:__broughtOnline = $null
function Ensure-BallastVMRegistered {
  param([string]$n)
  $v = Get-VM -Name $n -ErrorAction SilentlyContinue
  if ($v) { return $v }
  # Get-ClusterResource is absent on a standalone host, which is a legitimate
  # "no, it is not a cluster role" rather than an error.
  $cfg = @(Get-ClusterResource -ErrorAction SilentlyContinue |
    Where-Object { [string]$_.OwnerGroup -eq $n -and [string]$_.ResourceType -eq 'Virtual Machine Configuration' })
  if ($cfg.Count -eq 0) { return $null }
  Start-ClusterResource -InputObject $cfg[0] -ErrorAction Stop | Out-Null
  if (-not $global:__broughtOnline) { $global:__broughtOnline = $cfg[0] }
  $deadline = (Get-Date).AddSeconds(60)
  while ($true) {
    $v = Get-VM -Name $n -ErrorAction SilentlyContinue
    if ($v) { return $v }
    if ((Get-Date) -gt $deadline) {
      throw ('the cluster role for ' + $n + ' came online but Hyper-V still does not see the VM after 60 seconds')
    }
    Start-Sleep -Seconds 2
  }
}
$__vm = Ensure-BallastVMRegistered ` + vmVar + `
if (-not $__vm) {
  throw ('no virtual machine named ' + ` + vmVar + ` + ' exists on this host, and it is not an offline cluster role here either. If it is clustered it may have moved to another node.')
}
try {
`
}

const clusteredVMRestoreSuffix = `
} finally {
  if ($global:__broughtOnline) {
    try { Stop-ClusterResource -InputObject $global:__broughtOnline -ErrorAction Stop | Out-Null } catch {}
  }
}
`

// NodeMaintenanceState is what the cluster says about this node's availability.
type NodeMaintenanceState struct {
	// IsMember is false on a host that is not in a cluster at all, where
	// maintenance is purely a centre-side fact and there is nothing to pause.
	IsMember bool
	// Paused means the node accepts no roles.
	Paused bool
	// Draining means roles are still moving off. A node is paused the instant the
	// drain is asked for, but it is not OUT OF SERVICE until its VMs have
	// actually left — reporting maintenance before then would tell an operator it
	// is safe to reboot a node still running their workloads.
	Draining bool
	// StorageOut means some of the node's disks are marked "In Maintenance Mode"
	// in the S2D pool. On a PAUSED node this is normal and expected — the cluster
	// takes the disks out as part of the drain and puts them back on resume, and
	// Ballast neither sets nor clears it. On a node that is Up it is wreckage from
	// the old manual enable, and it is not benign: it holds every virtual disk
	// degraded, and a degraded space makes Suspend-ClusterNode refuse, so the node
	// cannot be drained at all until it is cleared.
	StorageOut bool
	// StorageError is set only for that second case — back in service, disks still
	// out, and the clear did not take. A paused node's disks being out is never an
	// error, or every drain would raise one.
	StorageError string

	// Blocked is set when the CLUSTER refused the drain for a reason that is not a
	// fault and not the operator's to fix — today, a degraded space, which clears
	// itself when the repair finishes.
	//
	// Distinct from an error because it is neither: the request stands, the node is
	// healthy, and the correct action is to wait. Reported as an error it read as
	// "maintenance failed", which invites forcing it — and forcing it removes a
	// copy the pool still needs.
	Blocked string
}

// MaintenanceIntent says what a pass should do about a node's availability.
//
// Observe exists because a node can be paused by someone who never went through
// Ballast — Failover Cluster Manager, a script, a half-finished job. Reading the
// state on every pass and only ACTING when maintenance is declared keeps that
// visible instead of the console quietly disagreeing with the cluster, and stops
// the reconciler resuming a node an operator paused by hand for a reason.
type MaintenanceIntent int

const (
	MaintenanceObserve MaintenanceIntent = iota
	MaintenanceEnter
	MaintenanceExit
)

func intentWord(i MaintenanceIntent) string {
	switch i {
	case MaintenanceEnter:
		return "enter"
	case MaintenanceExit:
		return "exit"
	default:
		return "observe"
	}
}

// maintenanceScript is built by a pure function so its content is pinned by
// tests without a host, like the template and clone scripts.
//
// The drain deliberately does NOT pass -Wait. On Suspend-ClusterNode that is a
// SWITCH, not a timeout: without it the drain is initiated and the cmdlet
// returns, which is what a reconciler wants — waiting would hold the whole cycle
// for minutes of live migration and look like a hang. "-Wait 0" would be worse
// than wrong, because 0 then binds positionally to -Name, a StringCollection —
// the same trap as the cluster cmdlets fixed in d113ecc. Checked against the
// cmdlet's real syntax on a live cluster before it ever ran.
func maintenanceScript(node string, intent MaintenanceIntent, deepStorage bool) string {
	deep := "$false"
	if deepStorage {
		deep = "$true"
	}
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
$intent = %[2]s
$deep = %[3]s
# Get-ClusterNode is absent on a host with no FailoverClusters module, and
# returns nothing for a host that is not a member. Both mean "not a cluster
# node", which is not an error — maintenance on a standalone host is a
# centre-side fact with nothing to enforce here.
$n = $null
try { $n = @(Get-ClusterNode -Name %[1]s -ErrorAction SilentlyContinue)[0] } catch {}
if (-not $n) { [pscustomobject]@{ member=$false; paused=$false; draining=$false; changed=$false } | ConvertTo-Json -Compress; return }
$state = [string]$n.State
$drain = [string]$n.DrainStatus
$paused = ($state -eq 'Paused')
$changed = $false
# Storage maintenance mode is the CLUSTER'S to manage, not Ballast's.
#
# Suspend-ClusterNode -Drain puts the node's physical disks into maintenance
# mode by itself, and Resume-ClusterNode takes them back out. Proven on the rig:
# HVNEW03 paused with the pool completely clear, and one pass later its four
# disks — and only its four — were In Maintenance Mode, by an agent build
# containing no code that enables it. So there was never anything for Ballast to
# do here.
#
# Doing it by hand was actively harmful. Enable-StorageMaintenanceMode on a
# scale unit is not atomic: it releases the disks one at a time, the spaces go
# Degraded part-way through, its own health validation then fails and it aborts
# WITHOUT rolling back, stranding the disks it already took. Stranded disks hold
# every space Degraded, and a degraded space makes Suspend-ClusterNode refuse —
# so calling it BEFORE the pause makes the pause impossible by its own side
# effect. That is the deadlock that cost HVNEW03 an afternoon: two of four disks
# stranded, all three virtual disks degraded, every drain refused with "a
# clustered space is in a degraded condition".
#
# Forcing past the check (-ValidateVirtualDisksHealthy $false) would make the
# abort less likely, not safe. The validation exists because releasing storage
# that still holds the only copy of data is how the data is lost.
#
# So: never enable, and never clear a flag the cluster is holding. While the
# node is paused, disks in maintenance is the CORRECT state and clearing it
# strips protection the cluster put there — Ballast would be fighting the
# cluster on every pass. Cluster-owned state is untouchable.
#
# The one thing left is healing wreckage: after a resume, with the node back Up,
# a lingering maintenance flag is nobody's intent — it is what the old manual
# enable left behind. That is cleared, and only then.
function Get-BallastMaintDisks($nodeName) {
  $out = @()
  foreach ($d in @(Get-PhysicalDisk -ErrorAction SilentlyContinue |
      Where-Object { @($_.OperationalStatus) -contains 'In Maintenance Mode' })) {
    $owner = [string]($d | Get-StorageNode -PhysicallyConnected -ErrorAction SilentlyContinue).Name
    if ($owner -and (($owner -split '\.')[0] -eq $nodeName)) { $out += $d }
  }
  return ,$out
}
function Get-BallastScaleUnit($nodeName) {
  return @(Get-StorageFaultDomain -Type StorageScaleUnit -ErrorAction SilentlyContinue |
    Where-Object { [string]$_.FriendlyName -eq $nodeName })
}
# try/catch throughout: a cluster without S2D has no scale units and no storage
# nodes, which is not an error.
function Clear-BallastStorageMaintenance($nodeName) {
  try {
    $su = Get-BallastScaleUnit $nodeName
    if ($su.Count -gt 0 -and ((@($su[0].OperationalStatus) -join ',') -match 'Maintenance')) {
      $su[0] | Disable-StorageMaintenanceMode -ErrorAction SilentlyContinue
    }
    # Disabling the scale unit is a no-op when the unit was never in maintenance
    # and only individual disks were stranded — which is exactly the wreckage a
    # part-way Enable leaves behind. Those have to be cleared by disk.
    foreach ($d in (Get-BallastMaintDisks $nodeName)) {
      Disable-StorageMaintenanceMode -InputObject $d -ErrorAction SilentlyContinue
    }
  } catch {}
  # Report what actually happened, not that an attempt was made. Reporting the
  # attempt made a failing clear look like progress every pass, which is how a
  # node that never changed kept reading as Updated.
  return (-not (Get-BallastStorageOut $nodeName))
}
function Get-BallastStorageOut($nodeName) {
  try {
    $su = Get-BallastScaleUnit $nodeName
    if ($su.Count -gt 0 -and ((@($su[0].OperationalStatus) -join ',') -match 'Maintenance')) { return $true }
    return ((Get-BallastMaintDisks $nodeName).Count -gt 0)
  } catch { return $false }
}
# The storage read is CLUSTER-WIDE: Get-PhysicalDisk in an S2D cluster returns
# every disk in the cluster, not this host's, and Get-StorageFaultDomain is no
# cheaper. Measured at 3m53s of a 4m27s pass on a node that had just rebooted —
# spent answering "are my disks still out of the pool?" for a node that was Up,
# not draining, and had no maintenance declared, where the answer cannot change.
#
# So it is asked when it can matter and skipped when it cannot:
#   - entering maintenance: the script is about to act on storage
#   - a paused node: its disks being out IS the state being reported
#   - $deep: the caller's slow cadence, which is what still finds wreckage left
#     on a node that resumed long ago
# Anything that CHANGES re-reads it below regardless, so no decision is ever made
# on a skipped answer — a skipped read reports $false, and the one consumer of
# that (the stuck-disks warning) requires an unpaused node, which is exactly the
# case the deep cadence covers.
$storageOut = $false
if ($deep -or $paused -or $intent -eq 'enter') { $storageOut = Get-BallastStorageOut %[1]s }
$storageErr = ''
# Entering is the drain and nothing else. The cluster takes the node's disks
# out as part of it, so there is no storage half to run, in either order.
$blocked = ''
if ($intent -eq 'enter') {
  if (-not $paused) {
    try {
      Suspend-ClusterNode -Name %[1]s -Drain -ErrorAction Stop | Out-Null
      $changed = $true
    } catch {
      # A degraded space refuses the pause, and that refusal is CORRECT: draining
      # this node takes its disks out of the pool, and a space that is already a
      # copy short cannot afford to lose another. It is not a fault to fix and not
      # a request to retry differently — it succeeds by itself when the repair
      # finishes.
      #
      # Passed through raw it read "Suspend-ClusterNode : An error occurred pausing
      # node", which says a cmdlet failed and nothing about why or what to do. The
      # repair progress is right here for the asking, so it is asked for.
      $m = [string]$_.Exception.Message
      if ($m -match 'degraded' -or $m -match 'clustered space') {
        $bits = @()
        foreach ($j in @(Get-StorageJob -ErrorAction SilentlyContinue | Where-Object { [string]$_.JobState -ne 'Completed' })) {
          $pc = ''
          try { $pc = ' at ' + [string][int]$j.PercentComplete + ' per cent' } catch {}
          $bits += ([string]$j.Name + $pc)
        }
        $bad = @(Get-VirtualDisk -ErrorAction SilentlyContinue |
          Where-Object { [string]$_.HealthStatus -ne 'Healthy' } |
          ForEach-Object { [string]$_.FriendlyName + ' (' + [string]$_.HealthStatus + ')' })
        $blocked = 'The cluster will not pause this node yet: '
        if ($bad.Count -gt 0) {
          $blocked += ($bad -join ', ') + ' ' + $(if ($bad.Count -eq 1) { 'is' } else { 'are' }) + ' not healthy, and draining this node takes its disks out of the pool as well.'
        } else {
          $blocked += 'a clustered space is degraded, and draining this node takes its disks out of the pool as well.'
        }
        if ($bits.Count -gt 0) {
          $blocked += ' A repair is running (' + ($bits -join ', ') + ').'
        }
        $blocked += ' Nothing to do: maintenance stays requested and starts on its own once the pool is healthy again. Forcing it would remove a copy the pool still needs.'
      } else {
        throw
      }
    }
  }
} elseif ($intent -eq 'exit') {
  if ($paused) {
    Resume-ClusterNode -Name %[1]s -ErrorAction Stop | Out-Null
    $changed = $true
    # Re-read before deciding anything about storage: while the node is still
    # paused its disks are the cluster's business.
    $n2 = @(Get-ClusterNode -Name %[1]s -ErrorAction SilentlyContinue)[0]
    if ($n2) { $paused = ([string]$n2.State -eq 'Paused') }
  }
  # Back Up and still marked out of the pool. The cluster releases the disks on
  # resume, so anything left here is wreckage from the old manual enable, and
  # clearing it is safe precisely because the node is no longer paused.
  if ((-not $paused) -and $storageOut) {
    if (Clear-BallastStorageMaintenance %[1]s) { $changed = $true }
  }
}
if ($changed) {
  $n = @(Get-ClusterNode -Name %[1]s -ErrorAction SilentlyContinue)[0]
  if ($n) { $state = [string]$n.State; $drain = [string]$n.DrainStatus; $paused = ($state -eq 'Paused') }
  $storageOut = Get-BallastStorageOut %[1]s
}
# Only worth reporting for a node that is back IN service with its disks still
# marked out — that combination is stuck and needs a human. A paused node with
# disks in maintenance is the normal drained state and must never be reported as
# a fault, or every drain raises a false alarm.
if ($intent -eq 'exit' -and (-not $paused) -and $storageOut) {
  $storageErr = "This node is back in service but its disks are still marked In Maintenance Mode in the storage pool, which holds every virtual disk degraded. Ballast tried to clear it and could not."
}
[pscustomobject]@{ member=$true; paused=$paused; draining=($drain -eq 'InProgress'); storageOut=$storageOut; storageError=$storageErr; blocked=$blocked; changed=$changed } | ConvertTo-Json -Compress`,
		psQuote(node), psQuote(intentWord(intent)), deep)
}

// EnsureNodeMaintenance reads a cluster node's availability and, when the intent
// says so, drives it. Idempotent: a paused node asked to pause is unchanged.
func (p *PowerShell) EnsureNodeMaintenance(ctx context.Context, node string, intent MaintenanceIntent, deepStorage bool) (Outcome, NodeMaintenanceState, error) {
	out, err := p.run(ctx, maintenanceScript(node, intent, deepStorage))
	if err != nil {
		return OutcomeUnchanged, NodeMaintenanceState{}, fmt.Errorf("ensure node maintenance on %q: %w", node, err)
	}
	var obs struct {
		Member       bool   `json:"member"`
		Paused       bool   `json:"paused"`
		Draining     bool   `json:"draining"`
		StorageOut   bool   `json:"storageOut"`
		StorageError string `json:"storageError"`
		Blocked      string `json:"blocked"`
		Changed      bool   `json:"changed"`
	}
	if derr := decodeJSON(out, &obs); derr != nil {
		return OutcomeUnchanged, NodeMaintenanceState{}, fmt.Errorf("ensure node maintenance on %q: %w", node, derr)
	}
	st := NodeMaintenanceState{IsMember: obs.Member, Paused: obs.Paused, Draining: obs.Draining, StorageOut: obs.StorageOut, StorageError: obs.StorageError, Blocked: obs.Blocked}
	if obs.Changed {
		return OutcomeUpdated, st, nil
	}
	return OutcomeUnchanged, st, nil
}

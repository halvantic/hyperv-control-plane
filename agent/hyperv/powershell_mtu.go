package hyperv

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Applying and observing MTU.

   Three facts shape all of this, and each one is a way the naive version fails
   silently.

   THE SETTING IS ON THE PHYSICAL ADAPTER. Jumbo frames are a driver advanced
   property (*JumboPacket), not an IP setting. A vSwitch has no MTU of its own
   and every vNIC on it inherits what the uplinks will carry, so setting 9000 on
   a storage vNIC whose uplink is at 1500 configures nothing and reports
   success.

   THE NUMBER MEANS DIFFERENT THINGS ON DIFFERENT CARDS. Intel offers 9014,
   Mellanox 9614, Broadcom 9216 — all carrying a 9000-byte payload, each
   counting headers differently, and most drivers accept ONLY values from their
   own menu. So the value written is never the operator's number; it is the
   smallest one the driver itself offered that carries that payload. The driver
   is asked what it will take rather than told, which is the only way this works
   across a mixed fleet.

   WHAT GOVERNS IS NlMtu, NOT THE DRIVER SETTING. The advanced property is what
   was requested of the miniport; the IP interface's NlMtu is what the stack
   will actually send. They can disagree — a driver that silently declined, a
   pending reset, a vNIC whose switch has not caught up — so every apply reads
   NlMtu back and reports that, not the value it just wrote. Trusting the write
   is how this ends up reporting jumbo on a host that has none.

   Changing *JumboPacket resets the miniport, so the link bounces for a second
   or two. That is why nothing here writes unless the observed value actually
   differs: on a steady pass the adapter is read and left alone. The idempotency
   rule earns its keep here more literally than most places — a reconciler that
   rewrote the same value every pass would bounce every uplink in the fleet on a
   40-second cycle.
*/

// AdapterMTU is what one physical adapter reports about its MTU.
type AdapterMTU struct {
	Name string `json:"name"`
	/* MtuSize is the miniport's own MTU, from Get-NetAdapter.

	   The best answer available and the last one this agent learned to ask for.
	   It is present on a teamed adapter, which NlMtu is not, and it is what the
	   card is actually carrying, which the driver's *JumboPacket display value
	   is not always.

	   Both of the other two failed on one real card at once. HPE's 631FLR-SFP28
	   (Marvell FastLinQ), 2026-09-07: Get-NetIPInterface does not list the
	   physical adapters at all, so NlMtu was 0; *JumboPacket reported
	   DisplayValue 1514 with no valid values and no numeric range; and
	   Get-NetAdapter reported MtuSize 9000, which was the truth. Ballast drew
	   1514 for an adapter carrying 9000. */
	MtuSize int `json:"mtuSize"`
	// NlMtu is the IP interface's MTU. Absent on a teamed adapter, which has no
	// IP interface by design.
	NlMtu int `json:"nlMtu"`
	// Keyword is the driver's jumbo advanced-property keyword, empty when the
	// adapter exposes none. Empty is a fact about the card, not a read failure.
	Keyword string `json:"keyword"`
	// Setting is the value currently selected, verbatim from the driver.
	Setting string `json:"setting"`
	// Values are every value this driver will accept, verbatim. EMPTY is not a
	// broken card: an enumerated property lists its options here, and a NUMERIC
	// one has none — it has a range instead. See Min/Max below.
	Values []string `json:"values"`
	// Min, Max and Step describe a NUMERIC *JumboPacket, which is how a good
	// many server NICs expose it — HPE's Embedded FlexibleLOM among them.
	//
	// They were not read at all, so an adapter that is perfectly capable of
	// jumbo frames reported "a jumbo-frame setting with no size this agent could
	// read. It offered: ." on every pass. Observed on real hardware 2026-09-07:
	// the message was accurate about what it saw and wrong about what it meant.
	Min  int `json:"min"`
	Max  int `json:"max"`
	Step int `json:"step"`
	/* RSC and LSO: the third thing that has to agree, and the one nothing
	   reports.

	   Receive Segment Coalescing and Large Send Offload both re-segment traffic
	   in the NIC, and on some drivers they interact badly with a 9000-byte MTU
	   behind a Hyper-V vSwitch. Every layer still reports 9000 — the adapter,
	   the IP interface, the switch — so no amount of reading configuration
	   shows anything wrong, and only an end-to-end frame proves otherwise.

	   Found on the rig, 2026-09-07: HPE 631FLR-SFP28 on Server 2025, everything
	   set to 9000 and jumbo not working until both were disabled by hand on
	   every host. That hand is what CLAUDE.md rules out, so Ballast now reads
	   them, says so, and can turn them off. */
	RSC bool `json:"rsc"`
	LSO bool `json:"lso"`
	// Found is false when there is no adapter by this name.
	Found bool `json:"found"`
}

// mtuScript reads the MTU picture for a set of adapters in one invocation.
//
// One call for the whole team: this runs on every reconcile pass to decide
// whether anything needs doing, and a per-adapter call would pay a fresh module
// load each time — the same cost that made the per-vNIC path unusable.
func mtuScript(adapters []string) string {
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$out = @()
foreach ($n in %s) {
  $a = Get-NetAdapter -Name $n -ErrorAction SilentlyContinue
  if (-not $a) { $out += [pscustomobject]@{ name = $n; found = $false }; continue }
  # The miniport's own MTU. Present whether or not the adapter is teamed, and
  # true whatever the driver's advanced property happens to say.
  $mtuSize = 0
  if ($a.MtuSize) { $mtuSize = [int]$a.MtuSize }
  # NlMtu is what the IP stack will actually send. Read first and reported
  # regardless of what the driver property below claims.
  $mtu = 0
  $ip = Get-NetIPInterface -InterfaceIndex $a.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue
  if ($ip) { $mtu = [int](@($ip)[0].NlMtu) }
  # The jumbo keyword is not spelled the same by every driver. *JumboPacket is
  # the standardised one; some cards only expose a display name, so fall back to
  # matching on that rather than reporting a jumbo-capable card as incapable.
  # Offloads that re-segment traffic in the NIC. Reported whether or not jumbo
  # is asked for, because the caller decides whether they matter.
  $rsc = $false
  foreach ($r in @(Get-NetAdapterRsc -Name $n -ErrorAction SilentlyContinue)) {
    if ($r.IPv4Enabled -or $r.IPv6Enabled) { $rsc = $true }
  }
  $lso = $false
  foreach ($l in @(Get-NetAdapterLso -Name $n -ErrorAction SilentlyContinue)) {
    if ($l.V1IPv4Enabled -or $l.IPv4Enabled -or $l.IPv6Enabled) { $lso = $true }
  }
  $adv = Get-NetAdapterAdvancedProperty -Name $n -RegistryKeyword '*JumboPacket' -ErrorAction SilentlyContinue
  if (-not $adv) {
    $adv = @(Get-NetAdapterAdvancedProperty -Name $n -ErrorAction SilentlyContinue |
      Where-Object { $_.DisplayName -like '*Jumbo*' })[0]
  }
  # An advanced property is EITHER enumerated or numeric. ValidDisplayValues is
  # populated only for the first kind; the second carries a range instead, and
  # reading only the list makes a capable card look incapable.
  $out += [pscustomobject]@{
    name    = $n
    found   = $true
    rsc     = $rsc
    lso     = $lso
    mtuSize = $mtuSize
    nlMtu   = $mtu
    keyword = [string]$adv.RegistryKeyword
    setting = [string]$adv.DisplayValue
    # Blanks dropped: a driver with no enumerated values yields one empty
    # string here, and one empty string reads as one offered size to
    # everything downstream — the same fault the DNS list had.
    values  = @($adv.ValidDisplayValues | ForEach-Object { [string]$_ } | Where-Object { $_ })
    min     = [int]$adv.NumericParameterMinValue
    max     = [int]$adv.NumericParameterMaxValue
    step    = [int]$adv.NumericParameterStepValue
  }
}
# Always an array, so one adapter does not decode as an object.
ConvertTo-Json -Compress -Depth 4 -InputObject @($out)
`, psStringList(adapters))
}

// AdapterMTUs observes the MTU picture for the named physical adapters.
func (p *PowerShell) AdapterMTUs(ctx context.Context, adapters []string) ([]AdapterMTU, error) {
	if len(adapters) == 0 {
		return nil, nil
	}
	out, err := p.run(ctx, mtuScript(adapters))
	if err != nil {
		return nil, fmt.Errorf("read adapter MTU: %w", err)
	}
	var got []AdapterMTU
	if err := decodeJSON(out, &got); err != nil {
		return nil, fmt.Errorf("read adapter MTU: %w", err)
	}
	return got, nil
}

/*
EnsureAdapterMTU sets every named adapter to carry want, and reports what happened.

	Returns OutcomeUnchanged when every adapter already carries it — which is the
	normal case on all but the first pass, and matters more than usual: writing
	the property resets the miniport, so an apply that ran every pass would bounce
	the uplinks on every reconcile.

	An adapter that CANNOT carry want is not an error that stops the others. It is
	a fact about that card, reported per adapter and gathered into one message, so
	a mixed fleet configures what it can and says precisely what it could not.
*/
func (p *PowerShell) EnsureAdapterMTU(ctx context.Context, adapters []string, want int) (Outcome, error) {
	if want <= 0 || len(adapters) == 0 {
		return OutcomeUnchanged, nil
	}
	obs, err := p.AdapterMTUs(ctx, adapters)
	if err != nil {
		return OutcomeUnchanged, err
	}

	apply, refusals := planAdapterMTU(obs, want)
	/* No offload is named here any more.

	   This reported LSO as a thing that stops jumbo working, because disabling
	   it made jumbo start working. The operator then re-ENABLED LSO and jumbo
	   kept working: writing any advanced property resets the miniport, and that
	   reset was the entire effect. The offload was never involved.

	   What actually puts an MTU in force is the adapter restarting, which
	   EnsureInterfaceMTU now does for fabric vNICs. Naming an innocent setting
	   on a condition an operator reads is worse than naming nothing — it sends
	   them to change something that will not help, and it did. */

	if len(apply) > 0 {
		if err := p.run2(ctx, applyMTUScript(apply)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("set MTU %d on %s: %w", want, namesOf(apply), err)
		}
	}
	if len(refusals) > 0 {
		// Reported as an error so it reaches a condition, AFTER whatever could be
		// applied was applied. Half a team at jumbo is worth having and worth
		// saying; silence about the other half is not.
		return outcomeFor(len(apply) > 0), fmt.Errorf("%s", strings.Join(refusals, "; "))
	}
	return outcomeFor(len(apply) > 0), nil
}

/*
planAdapterMTU decides what to write, and what to say about what cannot be.

	Pure, and shared by the real backend and the stub. It was inline in
	EnsureAdapterMTU with the stub carrying its own copy, and the two drifted
	immediately: the stub had no "already selected but not in force" branch, so a
	test written against the real rule passed against a backend that did not
	implement it. A decision worth testing is a decision worth having once.
*/
func planAdapterMTU(obs []AdapterMTU, want int) (apply []mtuApply, refusals []string) {
	for _, a := range obs {
		if !a.Found {
			refusals = append(refusals, a.Name+" is not present on this host")
			continue
		}
		/* Already carrying it.

		   NlMtu is the better authority WHERE IT EXISTS, because the driver
		   property records what was asked for and NlMtu what will be sent. On a
		   teamed adapter it does not exist at all: a physical NIC bound into a
		   vSwitch has no IP interface by design, so Get-NetIPInterface returns
		   nothing and NlMtu reads 0.

		   Judging that as "0 < 9000, not carrying it" was a permanent false
		   negative. Observed on HVNEW01, 2026-09-07: all four uplinks had 9014
		   correctly selected in their drivers, and SwitchMTU reported "not
		   applied — its IP interface still reports an MTU of 0" on every pass,
		   for ever. A reconcile that can never settle is the failure this whole
		   feature is supposed to prevent, produced by the check itself.

		   So: NlMtu decides when there is one, the driver's own selected value
		   decides when there is not. */
		if adapterCarries(a, want) {
			continue
		}
		/* An advanced property is EITHER enumerated or numeric.

		   ValidDisplayValues is populated only for the first kind. Reading only
		   that list made a card with a perfectly good range report "a
		   jumbo-frame setting with no size this agent could read. It offered: ."
		   on every pass — accurate about what it saw, wrong about what it meant.
		   Seen on HPE Embedded FlexibleLOM 1 Port 1/2, 2026-09-07. */
		value, numeric, ok := chooseJumbo(a, want)
		if !ok {
			refusals = append(refusals, types.DescribeJumboRefusal(a.Name, a.Keyword, a.Values, a.Max, want))
			continue
		}
		// The driver already has the right value selected and its IP interface
		// has NOT caught up — a reset in flight, or a card that needs poking.
		// Only reachable when NlMtu exists; a teamed adapter with the right value
		// selected was settled by adapterCarries above. Writing it again would
		// bounce the link for nothing.
		if strings.EqualFold(strings.TrimSpace(a.Setting), strings.TrimSpace(value)) {
			refusals = append(refusals, fmt.Sprintf(
				"%s already has %s selected in its driver but its IP interface still reports an MTU of %d. "+
					"The adapter has not finished applying it; this usually settles within a pass or two, and a "+
					"disable/enable of the adapter forces it.", a.Name, value, a.NlMtu))
			continue
		}
		apply = append(apply, mtuApply{Name: a.Name, Keyword: a.Keyword, Value: value, Numeric: numeric})
	}
	return apply, refusals
}

/*
adapterCarries reports whether this adapter already sends frames of want bytes.

	Three sources, tried in order of how much they can be trusted. Every one of
	them is absent or wrong on some real card, which is why there are three.

	  MtuSize — the miniport's own MTU, from Get-NetAdapter. Present whether or
	            not the adapter is teamed, and the thing the card is actually
	            carrying. First because it is right most often.
	  NlMtu   — the IP interface's MTU. Absent on a teamed adapter by design,
	            and that absence must never read as a small number, or every
	            uplink in every team is permanently unsettled.
	  Setting — the driver's selected *JumboPacket value. Last, because it can
	            disagree with the card: the 631FLR reported 1514 while carrying
	            9000. Compared by SIZE, not by string, since drivers spell it
	            variously ("9014", "9014 Bytes") and a menu reaching 9014 is
	            carrying the 9000-byte payload that was asked for.
*/
func adapterCarries(a AdapterMTU, want int) bool {
	// The miniport's own MTU, when it has one. Present on a teamed adapter and
	// true whatever the driver property says — see AdapterMTU.MtuSize.
	if a.MtuSize > 0 {
		return a.MtuSize >= want
	}
	if a.NlMtu > 0 {
		return a.NlMtu >= want
	}
	n, ok := jumboSize(a.Setting)
	return ok && n >= want
}

// jumboSize reads the size out of a driver's selected value, ignoring the units
// some drivers append.
func jumboSize(v string) (int, bool) { return types.JumboValueSize(v) }

func outcomeFor(changed bool) Outcome {
	if changed {
		return OutcomeUpdated
	}
	return OutcomeUnchanged
}

type mtuApply struct {
	Name    string
	Keyword string
	Value   string
	// Numeric selects how the value is written. An enumerated property takes
	// -DisplayValue; a numeric one takes -RegistryValue, and passing a bare
	// number as a display value is rejected by some drivers and silently
	// ignored by others.
	Numeric bool
}

/*
chooseJumbo picks the value to write, from whichever half the driver offers.

	Returns the value, whether it is numeric (which decides how it is written),
	and whether one exists at all.
*/
func chooseJumbo(a AdapterMTU, want int) (value string, numeric bool, ok bool) {
	if v, found := types.JumboValueFor(a.Values, want); found {
		return v, false, true
	}
	if n, found := types.JumboNumericFor(a.Min, a.Max, a.Step, want); found {
		return strconv.Itoa(n), true, true
	}
	return "", false, false
}

func namesOf(a []mtuApply) string {
	out := make([]string, 0, len(a))
	for _, x := range a {
		out = append(out, x.Name)
	}
	return strings.Join(out, ", ")
}

// applyMTUScript writes the chosen value to each adapter.
//
// -NoRestart is deliberately NOT used. The property only takes effect once the
// miniport restarts, and deferring that leaves the host reporting the new value
// with the old MTU in force — the exact split between "configured" and "true"
// this whole file exists to close.
func applyMTUScript(apply []mtuApply) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	for _, a := range apply {
		kw := a.Keyword
		if kw == "" {
			kw = "*JumboPacket"
		}
		// A numeric property is set by its registry value; an enumerated one by
		// its display value. Using the wrong one is rejected by some drivers and
		// quietly ignored by others, which is the worse outcome.
		param := "-DisplayValue"
		if a.Numeric {
			param = "-RegistryValue"
		}
		fmt.Fprintf(&b, "Set-NetAdapterAdvancedProperty -Name %s -RegistryKeyword %s %s %s -ErrorAction Stop\n",
			psQuote(a.Name), psQuote(kw), param, psQuote(a.Value))
	}
	return b.String()
}

/*
EnsureInterfaceMTU sets the IP interface MTU on one management vNIC.

	Separate from the adapter property and applied after it, because it is a
	different layer with a different failure: the uplink decides what can cross
	the wire, this decides what the stack will put on it. Setting this above the
	uplink is refused in the schema (ValidateMTU) rather than here, so a spec that
	cannot work never reaches a host.

	Idempotent by reading first. Set-NetIPInterface is cheap and does not reset
	anything, but a write on every pass would report Updated for ever and the
	reconcile would never settle.
*/
func (p *PowerShell) EnsureInterfaceMTU(ctx context.Context, vnicName string, want int, mayCycle bool) (Outcome, error) {
	if want <= 0 {
		return OutcomeUnchanged, nil
	}
	alias := "vEthernet (" + vnicName + ")"

	/* A management vNIC is an adapter as well as an IP interface, and both have
	   to agree before a jumbo frame leaves the host.

	   Set-NetIPInterface alone was not enough on the rig: the call is accepted
	   and NlMtu stays at 1500, on a host whose uplinks are genuinely at 9000.
	   The Hyper-V Virtual Ethernet Adapter has its own *JumboPacket, and the IP
	   layer cannot exceed what the miniport under it will carry — so the
	   adapter property is set FIRST and the interface MTU after it, which is
	   the same order as the physical uplinks and the vSwitch above them.

	   Both steps are idempotent and read back. The adapter property is only
	   written when it is actually short, because writing it resets the vNIC and
	   a vNIC reset drops the host's management address for a moment. */
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$alias    = %[1]s
$want     = %[2]d
$mayCycle = %[3]s

$i = Get-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue
if (-not $i) { Write-Output 'RESULT=absent'; return }
$cur = [int](@($i)[0].NlMtu)

# The adapter beneath the interface. Its MTU caps whatever the IP layer is told.
$na  = Get-NetAdapter -Name $alias -ErrorAction SilentlyContinue
$aMtu = 0; if ($na -and $na.MtuSize) { $aMtu = [int]$na.MtuSize }
$adv = Get-NetAdapterAdvancedProperty -Name $alias -RegistryKeyword '*JumboPacket' -ErrorAction SilentlyContinue
$advVal = ''; if ($adv) { $advVal = [string]$adv.DisplayValue }

if ($cur -eq $want -and ($aMtu -eq 0 -or $aMtu -ge $want)) { Write-Output 'RESULT=unchanged'; return }

# Raise the adapter first, and only when it is short: writing this resets the
# vNIC, and on the management vNIC that drops the host's address for a moment.
$bumped = $false
if ($adv -and $aMtu -gt 0 -and $aMtu -lt $want) {
  $vals = @($adv.ValidDisplayValues | ForEach-Object { [string]$_ } | Where-Object { $_ })
  $pick = ''
  foreach ($v in $vals) {
    $n = 0; if ([int]::TryParse((($v -split '\s+')[0]), [ref]$n)) { if ($n -ge $want -and ($pick -eq '' -or $n -lt $pickN)) { $pick = $v; $pickN = $n } }
  }
  if ($pick -ne '') {
    Set-NetAdapterAdvancedProperty -Name $alias -RegistryKeyword '*JumboPacket' -DisplayValue $pick -ErrorAction Stop
    $bumped = $true
    Start-Sleep -Seconds 3
  }
}

Set-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -NlMtuBytes $want -ErrorAction Stop

# The MTU is now SET and is not yet in force.
#
# Set-NetIPInterface resets nothing, and no reading anywhere shows the
# difference: HVNEW04 reported NlMtu 9000 on every vNIC while a 9000-byte
# frame would not cross. What puts it in force is the adapter restarting,
# which is why disabling an offload appeared to fix jumbo — writing any
# advanced property resets the miniport as a side effect. That was chased
# for a while as though the offload were the cause; it never was.
#
# Cycled here only where the caller allows it. A fabric vNIC carries no part
# of the agent's own path and can be bounced freely; the management vNIC
# holds the host's address and its link to the centre, so that one is left
# for an operator to do deliberately.
if ($mayCycle) {
  Restart-NetAdapter -Name $alias -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 4
}

# Read back rather than trusting the set. An interface whose adapter cannot
# carry the size accepts the call and keeps the old value, which would otherwise
# be reported as a successful change.
$now = [int](@(Get-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue)[0].NlMtu)
if ($now -ne $want) {
  $na2 = Get-NetAdapter -Name $alias -ErrorAction SilentlyContinue
  $a2 = 0; if ($na2 -and $na2.MtuSize) { $a2 = [int]$na2.MtuSize }
  $adv2 = Get-NetAdapterAdvancedProperty -Name $alias -RegistryKeyword '*JumboPacket' -ErrorAction SilentlyContinue
  $offer = ''
  if ($adv2) { $offer = (@($adv2.ValidDisplayValues | ForEach-Object { [string]$_ } | Where-Object { $_ }) -join ', ') }
  $v2 = ''; if ($adv2) { $v2 = [string]$adv2.DisplayValue }
  # Say what was actually seen, not just that it failed. Which of the two layers
  # is short is the whole diagnosis, and only this script can see both.
  throw ("the interface accepted an MTU of $want and still reports $now. " +
    "Its adapter carries $a2 and its jumbo setting is '" + $v2 + "'" +
    $(if ($offer) { " (offers: $offer)" } else { " (offers nothing this agent can read)" }) + ". " +
    $(if ($a2 -gt 0 -and $a2 -lt $want) { "The adapter under this vNIC is the limit." } else { "The adapter is not the limit, so the vSwitch or its uplinks are." }))
}
if (-not $mayCycle) { Write-Output 'RESULT=set-needs-restart'; return }
if ($bumped) { Write-Output 'RESULT=set-adapter' } else { Write-Output 'RESULT=set' }
`, psQuote(alias), want, psBool(mayCycle))

	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("set MTU %d on %s: %w", want, alias, err)
	}
	switch resultOf(out) {
	case "absent":
		return OutcomeUnchanged, fmt.Errorf("no IPv4 interface named %q, so its MTU could not be set", alias)
	case "set", "set-adapter":
		return OutcomeUpdated, nil
	case "set-needs-restart":
		// Applied and not yet in force, and nothing readable will say so. The
		// management vNIC is not cycled on Ballast's own initiative because it
		// carries the host's address and the agent's link to the centre.
		return OutcomeUpdated, fmt.Errorf(
			"the MTU of %d is set on %s and will not take effect until the adapter is restarted. Nothing reports "+
				"the difference — the interface already reads %d — so a jumbo frame will not cross until it is "+
				"cycled. This is the management vNIC, so restarting it briefly drops this host's address and its "+
				"link to the centre; do it deliberately from the host's networking page", want, alias, want)
	default:
		return OutcomeUnchanged, nil
	}
}

func resultOf(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "RESULT="); ok {
			return v
		}
	}
	return ""
}

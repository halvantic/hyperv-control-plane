package types

import (
	"fmt"
	"strconv"
	"strings"
)

/* MTU: what has to agree, and what nothing on the host will tell you.

   Jumbo frames are the setting most likely to be configured on one side only,
   because every layer reports success independently. The driver accepts the
   value. The IP interface shows 9000. The session establishes. Traffic flows —
   at 1500, or not at all above it, depending on where the mismatch is — and the
   only symptom is throughput that never arrives or an intermittent stall on
   large writes. Nothing anywhere says "MTU".

   Three things have to agree and only one of them is Ballast's to set:

     1. The physical uplink. A driver advanced property (*JumboPacket), set per
        adapter. Ballast owns this.
     2. The IP interface on the vNIC. NlMtu, which cannot exceed what the uplink
        will carry. Ballast owns this.
     3. The physical switch between the hosts. Ballast does not administer it,
        has no presence on it, and cannot read it.

   The third is the reason this file exists rather than a single reconciler.
   Ballast can get its half exactly right and jumbo can still be broken, so the
   honest report is not "configured" but "configured, and here is whether a
   frame that size actually crossed the wire". CLAUDE.md is explicit that where
   an action lies outside the boundary, the console names the one step required
   rather than failing obscurely — for MTU that step is "your switch ports need
   jumbo enabled", and only an end-to-end test can say it.
*/

// DefaultJumboMTU is the payload MTU meant by "jumbo frames" everywhere it
// matters here: SMB Direct to an S2D pool, and iSCSI to an array.
//
// 9000 rather than 9014 or 9216 deliberately. Those are frame sizes and they
// differ per vendor; the payload is the thing both ends have to agree on, and
// it is the number an array's documentation quotes.
const DefaultJumboMTU = 9000

// StandardMTU is the Ethernet default. Named because "1500" appears in messages
// where the reader needs to know it is the untouched default and not a choice.
const StandardMTU = 1500

// MinJumboMTU is the smallest MTU worth calling jumbo. Below this the setting
// costs an adapter reset and buys nothing.
const MinJumboMTU = 1501

/*
ValidateMTU checks a host's declared MTUs against each other.

	Everything here is about the two layers Ballast owns agreeing. Whether the
	switch between the hosts agrees is not knowable from a spec and is not
	guessed at — see TestJumboPath, which is the only thing that can answer it.
*/
func ValidateMTU(n HostNetworkingSpec) []StorageNetworkProblem {
	var out []StorageNetworkProblem

	switchOf := map[string]VirtualSwitchSpec{}
	for _, sw := range n.Switches {
		switchOf[strings.ToLower(sw.Name)] = sw
		if sw.MTUBytes != 0 && (sw.MTUBytes < StandardMTU || sw.MTUBytes > 9216) {
			out = append(out, StorageNetworkProblem{
				Why: "switch " + sw.Name + " declares an MTU of " + strconv.Itoa(sw.MTUBytes) +
					", which is outside the range any Ethernet adapter will accept (1500 to 9216). " +
					"Jumbo frames are normally 9000.",
				Fatal: true,
			})
		}
	}

	for _, v := range n.ManagementVNICs {
		if v.MTUBytes == 0 {
			continue
		}
		if v.MTUBytes < StandardMTU || v.MTUBytes > 9216 {
			out = append(out, StorageNetworkProblem{
				VNIC: v.Name,
				Why: "declares an MTU of " + strconv.Itoa(v.MTUBytes) + ", which is outside the range any Ethernet " +
					"interface will accept (1500 to 9216). Jumbo frames are normally 9000.",
				Fatal: true,
			})
			continue
		}
		sw, ok := switchOf[strings.ToLower(v.SwitchName)]
		if !ok {
			continue // a vNIC on an undeclared switch is a different problem, reported elsewhere
		}

		/* A vNIC asking for more than its uplink can carry.

		   This is the silent one. Windows accepts the interface MTU whatever the
		   adapter beneath it is set to, so the host reports 9000 on both, and
		   every frame over the uplink's real limit is dropped by the miniport
		   with no error surfaced to the IP stack. Refused rather than reported,
		   because unlike a single storage path there is no arrangement in which
		   this is what somebody meant. */
		uplink := sw.MTUBytes
		if uplink == 0 {
			uplink = StandardMTU
		}
		if v.MTUBytes > uplink {
			why := "asks for an MTU of " + strconv.Itoa(v.MTUBytes) + " on switch " + sw.Name +
				", whose uplinks carry " + strconv.Itoa(uplink) + ". "
			if sw.MTUBytes == 0 {
				why += "The switch declares no MTU, so its adapters are left at the Ethernet default of 1500. " +
					"Declare the MTU on the switch as well — that is where jumbo frames are actually enabled, " +
					"on the physical adapters."
			} else {
				why += "An interface cannot send a frame its uplink will not carry: the host would report both as " +
					"configured and the adapter would drop everything above " + strconv.Itoa(uplink) + " silently."
			}
			out = append(out, StorageNetworkProblem{VNIC: v.Name, Why: why, Fatal: true})
		}
	}

	/* A storage vNIC left at the default on a switch that carries jumbo.

	   Not fatal and not presumptuous: 1500 storage works. But a switch declared
	   at 9000 exists because somebody wanted jumbo storage, and a storage vNIC
	   that did not opt in is far more likely to be an omission than a decision —
	   and it is invisible, because the vNIC is up and carrying traffic. */
	for _, v := range n.StorageVNICs() {
		sw, ok := switchOf[strings.ToLower(v.SwitchName)]
		if !ok || sw.MTUBytes < MinJumboMTU {
			continue
		}
		if v.MTUBytes == 0 {
			out = append(out, StorageNetworkProblem{
				VNIC: v.Name,
				Why: "is a storage vNIC on " + sw.Name + ", whose uplinks are set to " + strconv.Itoa(sw.MTUBytes) +
					", but declares no MTU of its own — so its IP interface stays at the Ethernet default of 1500 " +
					"and the jumbo frames the uplinks can carry are never sent. Declare " + strconv.Itoa(sw.MTUBytes) +
					" on the vNIC too.",
				Fatal: false,
			})
		}
	}
	return out
}

/*
JumboValueFor picks the driver value that carries the wanted payload.

	The whole reason the agent cannot simply write the number an operator typed.
	Every vendor counts *JumboPacket differently — Intel's 9014 and Mellanox's
	9614 both carry a 9000-byte payload, some drivers offer a short menu of fixed
	sizes and nothing between — so the only correct value is one the driver itself
	offered. This picks the SMALLEST offered value that still carries want, which
	is what a switch's own MTU has to accommodate.

	values are the driver's ValidDisplayValues verbatim, e.g. "1514", "4088",
	"9014", or "Disabled". Non-numeric entries are ignored rather than rejected:
	they are how a driver spells "off", and refusing to parse them would make an
	ordinary adapter look broken.

	Returns the value to write and whether one exists. When nothing offered is
	large enough the caller must say so — quietly writing the biggest available
	would leave the host reporting jumbo while carrying less than asked.
*/
func JumboValueFor(values []string, want int) (string, bool) {
	best, bestN := "", 0
	for _, v := range values {
		n, ok := jumboNumber(v)
		if !ok || n < want {
			continue
		}
		if best == "" || n < bestN {
			best, bestN = v, n
		}
	}
	return best, best != ""
}

/*
JumboHeaderBytes is the Ethernet + VLAN overhead a frame size carries over

	its payload.

	Why an enum driver offering 9014 is the right choice for a 9000 payload, and
	why a numeric one is asked for 9014 rather than 9000. Drivers that count the
	payload instead simply get 14 bytes more headroom than asked for, which costs
	nothing: what the host actually SENDS is the IP interface MTU, set separately
	and exactly. An uplink able to carry a little more than the interface is the
	normal arrangement; an uplink able to carry less is the fault.
*/
const JumboHeaderBytes = 14

/*
JumboNumericFor picks a value for a NUMERIC *JumboPacket.

	A good many server NICs — HPE's Embedded FlexibleLOM among them — expose
	jumbo frames as a range rather than a menu, so ValidDisplayValues is empty
	and there is nothing to choose from. Reading only the list made a perfectly
	capable card report "a jumbo-frame setting with no size this agent could
	read. It offered: ." for ever.

	Aims at want + headers so the interface can actually reach want, and never
	goes above max. A driver whose max sits between want and want+headers still
	gets its max, which is the most it can do and is worth having.

	step is honoured downwards, so the value written is one the driver will
	accept rather than one it silently rounds. A rounded value that lands below
	want is refused rather than written: reporting jumbo while carrying less
	than asked is the failure this whole file exists to prevent.
*/
func JumboNumericFor(min, max, step, want int) (int, bool) {
	if want <= 0 || max <= 0 || max < want {
		return 0, false
	}
	v := want + JumboHeaderBytes
	if v > max {
		v = max
	}
	if step > 1 && min > 0 && v > min {
		v = min + ((v-min)/step)*step
	}
	if v < min {
		v = min
	}
	if v < want {
		return 0, false
	}
	return v, true
}

// JumboValueSize exposes the size a driver's display value stands for, so a
// caller can compare what is SELECTED against what was wanted. Drivers spell
// these variously ("9014", "9014 Bytes"), and the units are not the point.
func JumboValueSize(v string) (int, bool) { return jumboNumber(v) }

// jumboNumber reads the size out of a driver's display value. Drivers spell
// these variously ("9014", "9014 Bytes", "9000"), so the leading digits are
// taken and the rest ignored.
func jumboNumber(v string) (int, bool) {
	v = strings.TrimSpace(v)
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

/*
DescribeJumboRefusal explains an adapter that cannot carry what was asked.

	Written for the operator rather than the developer, and it names the one thing
	they can act on. Three genuinely different facts, which used to be two:

	  no keyword      the card exposes no jumbo setting at all. A fact about the
	                  hardware or its driver; nothing here changes it.
	  a ceiling       the driver offers sizes, and the largest falls short. Names
	                  the actual limit rather than saying "cannot".
	  neither         the property exists and reports no sizes AND no range. This
	                  is the message the HPE FlexibleLOM produced — "It offered: ."
	                  — because only the enumerated list was being read. It should
	                  now be rare, and when it happens it is a genuinely unreadable
	                  driver rather than an unread one, so it says that.
*/
func DescribeJumboRefusal(adapter, keyword string, values []string, max, want int) string {
	if keyword == "" {
		return fmt.Sprintf("%s exposes no jumbo-frame setting, so it cannot carry an MTU of %d. "+
			"Either the adapter does not support jumbo frames or its driver does not expose them; "+
			"a driver update is the only thing that changes this.", adapter, want)
	}
	largest := max
	for _, v := range values {
		if n, ok := jumboNumber(v); ok && n > largest {
			largest = n
		}
	}
	if largest == 0 {
		return fmt.Sprintf("%s has a jumbo-frame setting (%s) that reports neither a list of sizes nor a range, "+
			"so there is no value this agent can safely write. Set it by hand in the adapter's advanced "+
			"properties and Ballast will read it back. Nothing was changed.", adapter, keyword)
	}
	offered := "a maximum of " + strconv.Itoa(largest)
	if len(values) > 0 {
		offered = "it offered: " + strings.Join(values, ", ")
	}
	return fmt.Sprintf("%s cannot carry an MTU of %d: the largest its driver allows is %d (%s). Nothing was changed.",
		adapter, want, largest, offered)
}

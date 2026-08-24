package types

import (
	"fmt"
	"net"
	"strings"
)

// Storage networking: what has to be true for redundancy to be real.
//
// Both storage models multiplex across INTERFACES. SMB Multichannel does it for
// S2D, MPIO does it for iSCSI, and neither can manufacture a second interface
// that is not there. So the arrangement both need is the same: two or more
// storage vNICs, on separate subnets, each pinned to a different physical uplink.
//
// None of that was expressible, so none of it was checked, and a fleet could
// look multipathed while every path shared one cable. Observed 2026-08-24: three
// hosts, three declared portals each, one path each, MPIO reporting "in effect"
// with nothing to fail over to.
//
// These checks are deliberately in api/types rather than in the REST handler.
// The console is not the only way to author a spec, and a rule that lives in a
// form is a rule the API does not have.

// StorageNetworkProblem is one thing wrong with a host's storage networking.
type StorageNetworkProblem struct {
	// VNIC names the vNIC at fault, or "" for a problem with the set as a whole.
	VNIC string
	// Why is written for the operator, not the developer: what is wrong, what it
	// costs, and what to do.
	Why string
	// Fatal separates "this cannot work" from "this works and is not redundant".
	// A single storage path is a legitimate lab arrangement and an unacceptable
	// production one, and Ballast is not the judge of which this is — so it says
	// so and does not refuse.
	Fatal bool
}

// ValidateStorageNetwork checks a host's storage vNICs against what the two
// storage models actually require.
//
// requireMultipath is true when the storage model needs more than one path —
// S2D always, iSCSI when MPIO is in force. It only affects whether a single
// storage vNIC is worth reporting; everything else is checked regardless.
func ValidateStorageNetwork(n HostNetworkingSpec, requireMultipath bool) []StorageNetworkProblem {
	var out []StorageNetworkProblem
	vnics := n.StorageVNICs()
	if len(vnics) == 0 {
		return nil // nothing declared; the caller decides whether that matters
	}

	switchOf := map[string]VirtualSwitchSpec{}
	for _, sw := range n.Switches {
		switchOf[strings.ToLower(sw.Name)] = sw
	}

	byAdapter := map[string][]string{}
	bySubnet := map[string][]string{}
	for _, v := range vnics {
		// An unpinned storage vNIC is the whole defect in one field. SET places it
		// on whichever uplink it likes, so two of them can land on the same port
		// and the second path is imaginary.
		if v.TeamMemberAdapter == "" {
			if sw, ok := switchOf[strings.ToLower(v.SwitchName)]; ok && len(sw.TeamMembers) > 1 {
				out = append(out, StorageNetworkProblem{
					VNIC: v.Name,
					Why: "is a storage vNIC on the teamed switch " + v.SwitchName + " but is not pinned to a physical adapter. " +
						"SET will place it on whichever uplink it chooses, so it can share one with another storage vNIC — " +
						"and then the second path exists only on paper. Pin it to one of: " + strings.Join(sortedFold(sw.TeamMembers), ", "),
					Fatal: false,
				})
			}
		} else {
			key := strings.ToLower(v.TeamMemberAdapter)
			byAdapter[key] = append(byAdapter[key], v.Name)
			if sw, ok := switchOf[strings.ToLower(v.SwitchName)]; ok && !hasFold(sw.TeamMembers, v.TeamMemberAdapter) {
				out = append(out, StorageNetworkProblem{
					VNIC: v.Name,
					Why: "is pinned to " + v.TeamMemberAdapter + ", which is not a member of the switch " + v.SwitchName +
						" (its members are " + strings.Join(sortedFold(sw.TeamMembers), ", ") + "). The mapping cannot be applied, so the vNIC is placed wherever SET likes.",
					Fatal: true,
				})
			}
		}

		if v.IPConfig == nil || v.IPConfig.Address == "" {
			out = append(out, StorageNetworkProblem{
				VNIC: v.Name,
				Why: "is a storage vNIC with no address. Storage paths are chosen by subnet, so a vNIC on DHCP " +
					"cannot be matched to an array portal or relied on to keep the same path across a reboot.",
				Fatal: true,
			})
			continue
		}
		if sub, ok := subnetOf(v.IPConfig.Address); ok {
			bySubnet[sub] = append(bySubnet[sub], v.Name)
		}
	}

	// Two storage vNICs on ONE uplink is the failure this exists to catch. They
	// look like two paths everywhere — two IPs, two sessions, MPIO "in effect" —
	// and one cable takes both.
	for adapter, names := range byAdapter {
		if len(names) > 1 {
			out = append(out, StorageNetworkProblem{
				Why: strings.Join(sortedFold(names), " and ") + " are both pinned to the same physical adapter (" + adapter + "), " +
					"so they are one path wearing two addresses. A single cable, port or NIC failure takes all of it. Pin them to different adapters.",
				Fatal: false,
			})
		}
	}

	// Same subnet is the quieter version of the same mistake: the routing table
	// picks one interface and the other never carries anything.
	for sub, names := range bySubnet {
		if len(names) > 1 {
			out = append(out, StorageNetworkProblem{
				Why: strings.Join(sortedFold(names), " and ") + " are both on " + sub + ". Storage multipathing selects an interface per subnet, " +
					"so a second vNIC on the same one adds an address and no path. Give each storage vNIC its own subnet.",
				Fatal: false,
			})
		}
	}

	if requireMultipath && len(vnics) == 1 {
		out = append(out, StorageNetworkProblem{
			VNIC: vnics[0].Name,
			Why: "is the only storage vNIC on this host, so there is no redundant path to the storage — " +
				"a single link failure takes it away entirely, which is the thing multipathing exists to prevent. " +
				"Declare a second on its own subnet and its own uplink.",
			Fatal: false,
		})
	}
	return out
}

// FatalStorageNetworkProblems returns only the problems that make the
// configuration unworkable, which is what an authoring path should refuse on.
// The rest are reported and honoured: a single storage path is a legitimate lab
// arrangement, and Ballast is not the judge of which this is.
func FatalStorageNetworkProblems(ps []StorageNetworkProblem) []StorageNetworkProblem {
	var out []StorageNetworkProblem
	for _, p := range ps {
		if p.Fatal {
			out = append(out, p)
		}
	}
	return out
}

// Error renders a problem the way an operator reads it.
func (p StorageNetworkProblem) Error() string {
	if p.VNIC == "" {
		return p.Why
	}
	return p.VNIC + " " + p.Why
}

// subnetOf reduces a CIDR address to its network, which is what decides whether
// two vNICs are on the same path.
func subnetOf(cidr string) (string, bool) {
	_, n, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		// A bare address with no prefix cannot be compared; treated as its own
		// subnet rather than guessing a mask, because guessing /24 would silently
		// declare two unrelated addresses to be on one network.
		return "", false
	}
	return n.String(), true
}

func hasFold(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

// sortedFold orders names so a message reads the same way every time; an
// alarm whose wording shuffles between passes looks like a new alarm.
func sortedFold(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && strings.ToLower(out[j]) < strings.ToLower(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// DescribeStorageNetwork summarises a host's storage networking in one line, for
// a console that has to say whether redundancy is real without making the
// operator read a topology.
func DescribeStorageNetwork(n HostNetworkingSpec) string {
	vnics := n.StorageVNICs()
	if len(vnics) == 0 {
		return "no storage vNICs declared"
	}
	adapters := map[string]bool{}
	for _, v := range vnics {
		if v.TeamMemberAdapter != "" {
			adapters[strings.ToLower(v.TeamMemberAdapter)] = true
		}
	}
	return fmt.Sprintf("%d storage vNICs across %d physical adapters", len(vnics), len(adapters))
}

// StorageAddresses returns the CIDR addresses of a host's storage vNICs, which
// is what an initiator binds a session to.
func (n HostNetworkingSpec) StorageAddresses() []string {
	var out []string
	for _, v := range n.StorageVNICs() {
		if v.IPConfig != nil && v.IPConfig.Address != "" {
			out = append(out, v.IPConfig.Address)
		}
	}
	return out
}

// InitiatorFor picks the local address an iSCSI session to portal should leave
// through, from the host's declared storage addresses.
//
// Without this the initiator asks the routing table, and the routing table
// answers with ONE interface. That is how three declared portals became three
// sessions out of one vNIC on the rig 2026-08-24 — MPIO in effect, three paths
// reported, one cable carrying all of them. Binding each session to the storage
// address on the portal's own subnet is what makes the paths distinct.
//
// Returns "" when no storage address shares the portal's subnet: nothing is
// bound rather than something guessed, because binding a session to the wrong
// source address does not fail cleanly — it fails at login with a message about
// the array.
func InitiatorFor(portal string, storageAddresses []string) string {
	pip := net.ParseIP(hostPart(portal))
	if pip == nil {
		return ""
	}
	for _, a := range storageAddresses {
		ip, subnet, err := net.ParseCIDR(strings.TrimSpace(a))
		if err != nil || !subnet.Contains(pip) {
			continue
		}
		return ip.String()
	}
	return ""
}

// hostPart strips a ":3260" from a portal, which the spec allows and an IP
// parser does not.
func hostPart(portal string) string {
	p := strings.TrimSpace(portal)
	if h, _, err := net.SplitHostPort(p); err == nil {
		return h
	}
	return p
}

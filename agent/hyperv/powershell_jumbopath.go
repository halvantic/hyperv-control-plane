package hyperv

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Does a frame that size actually cross the wire?

   Every other MTU check in Ballast reads configuration. This one is the only
   thing that establishes truth, because the piece that most often breaks jumbo
   frames is the piece Ballast has no presence on: the physical switch between
   the hosts. Both ends can be perfectly configured and every frame over 1500
   still dies in a switch port nobody enabled, with no error anywhere — the host
   sends, nothing comes back, and TCP quietly degrades or stalls.

   Don't-fragment is the entire mechanism. Without DF the IP stack splits an
   oversized packet into 1500-byte pieces and the test passes on a path that
   cannot carry jumbo at all — which is worse than not testing, because it
   produces a green result for a broken fabric.

   The payload is the MTU minus 28: 20 bytes of IPv4 header and 8 of ICMP. So an
   MTU of 9000 is tested with `ping -f -l 8972`. Getting this wrong by 28 bytes
   is the classic way a jumbo test reports failure on a path that works.

   Source-address binding matters as much as the size. A host with a storage
   vNIC on 10.0.1.0/24 and management on 192.168.1.0/24 routes an unpinned ping
   out of whichever interface the routing table prefers, so an untargeted test
   measures the management path while claiming to measure storage. -S pins it.
*/

// JumboProbe is one source-to-destination test.
type JumboProbe struct {
	// VNIC is the management vNIC the probe left from, for reporting.
	VNIC string `json:"vnic"`
	// Source and Target are the addresses actually used.
	Source string `json:"source"`
	Target string `json:"target"`
	// MTU is the payload MTU tested, not the ICMP payload sent.
	MTU int `json:"mtu"`
	// OK is whether a frame that size crossed and came back.
	OK bool `json:"ok"`
	// StandardOK is whether an ordinary ping crossed.
	//
	// The field that turns a failure into a diagnosis. A jumbo probe that fails
	// where a standard one succeeds is an MTU problem on a reachable path — the
	// switch. Both failing is not an MTU problem at all; the peer is down or
	// firewalled, and reporting that as a jumbo fault would send an operator to
	// configure a switch that is fine.
	StandardOK bool `json:"standardOK"`
	// Detail is what the probe saw, verbatim enough to argue with.
	Detail string `json:"detail,omitempty"`
}

/*
jumboPair is one source-to-destination probe to run.

	Paired in Go rather than in the script. The rule — only probe where source and
	target share a subnet — needs a real CIDR comparison, and a jumbo probe that
	crosses a router measures the router's MTU and names the wrong culprit. Doing
	it here also makes the rule testable without a Windows host.
*/
type jumboPair struct {
	VNIC   string
	Source string
	Target string
}

/*
jumboPairs works out which probes are worth running.

	A vNIC and a target on different subnets are skipped, not failed: there is
	nothing wrong with either, they simply do not share a path this test can say
	anything about.
*/
func jumboPairs(vnics []types.ManagementVNICSpec, targets []string) []jumboPair {
	var out []jumboPair
	for _, v := range vnics {
		if v.IPConfig == nil || strings.TrimSpace(v.IPConfig.Address) == "" {
			continue
		}
		ip, network, err := net.ParseCIDR(strings.TrimSpace(v.IPConfig.Address))
		if err != nil {
			// A bare address with no prefix cannot be compared. Skipped rather
			// than assumed to be a /24 — guessing a mask would run probes across
			// unrelated networks and report their routers as broken switches.
			continue
		}
		for _, t := range targets {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			ti := net.ParseIP(t)
			if ti == nil || !network.Contains(ti) {
				continue
			}
			out = append(out, jumboPair{VNIC: v.Name, Source: ip.String(), Target: t})
		}
	}
	return out
}

/*
TestJumboPath probes every source/target pair that shares a subnet.

	Sources are the addresses on the host's own vNICs; targets are what the caller
	supplies. Returns one probe per pair with both a jumbo and an ordinary result,
	so a failure can be attributed rather than merely reported.
*/
func (p *PowerShell) TestJumboPath(ctx context.Context, vnics []types.ManagementVNICSpec, targets []string, mtu int) ([]JumboProbe, error) {
	if mtu <= 0 {
		mtu = types.DefaultJumboMTU
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no addresses to test against: give the job a target, or declare the peers whose " +
			"storage addresses this host should reach")
	}
	pairs := jumboPairs(vnics, targets)
	if len(pairs) == 0 {
		// Not an error. The caller says "nothing shared a subnet", which is a
		// different and more useful sentence than a failure.
		return nil, nil
	}

	out, err := p.run(ctx, jumboPathScript(pairs, mtu))
	if err != nil {
		return nil, fmt.Errorf("jumbo path test: %w", err)
	}
	var probes []JumboProbe
	if err := decodeJSON(out, &probes); err != nil {
		return nil, fmt.Errorf("jumbo path test: %w", err)
	}
	for i := range probes {
		probes[i].Detail = firstPingLine(probes[i].Detail)
	}
	return probes, nil
}

/*
jumboPathScript runs the probes in one invocation.

	One PowerShell for the whole test: a fresh session costs more than every ping
	in it put together.

	Both pings run for every pair, always. The ordinary one is what makes a
	failure attributable — without it, an unreachable peer and a switch that will
	not carry jumbo produce the same result, and only one of them is an MTU fault.
*/
func jumboPathScript(pairs []jumboPair, mtu int) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'SilentlyContinue'\n$out = @()\n")
	// The ICMP payload is the MTU less 20 bytes of IPv4 header and 8 of ICMP.
	// Getting this wrong by 28 bytes is the classic way a jumbo test reports
	// failure on a path that works.
	payload := mtu - 28
	for _, pr := range pairs {
		fmt.Fprintf(&b, `
$j = & ping.exe -n 2 -w 1500 -f -l %[1]d -S %[2]s %[3]s 2>&1 | Out-String
$s = & ping.exe -n 2 -w 1500 -S %[2]s %[3]s 2>&1 | Out-String
$out += [pscustomobject]@{
  vnic = %[4]s; source = %[2]s; target = %[3]s; mtu = %[5]d
  ok = [bool]($j -match 'Received = [1-9]')
  standardOK = [bool]($s -match 'Received = [1-9]')
  detail = [string]$j
}
`, payload, psQuote(pr.Source), psQuote(pr.Target), psQuote(pr.VNIC), mtu)
	}
	b.WriteString("ConvertTo-Json -Compress -Depth 4 -InputObject @($out)\n")
	return b.String()
}

/*
firstPingLine reduces ping's transcript to the line that says what happened.

	ping repeats itself and prefixes a header; a whole transcript in a job result
	tells an operator nothing the pass/fail flag does not already say. The
	interesting line is the first that is neither blank nor the "Pinging x with n
	bytes of data" header — usually "Packet needs to be fragmented but DF set",
	which is the exact string that proves the MTU verdict.
*/
func firstPingLine(out string) string {
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Pinging ") {
			continue
		}
		return line
	}
	return ""
}

/*
DescribeJumboProbes turns the probes into the sentence an operator acts on.

	The point of the whole job. A list of failed pings is a symptom; naming which
	of the three layers is wrong is the diagnosis, and it is knowable here:

	  - jumbo fails, standard succeeds → the path is up and cannot carry the
	    frame. Both hosts are configured, so what is left is the switch between
	    them. Ballast has no presence on it, which is exactly the case CLAUDE.md
	    says must name the one step required rather than failing obscurely.
	  - both fail → not an MTU fault at all. Saying "jumbo frames are not working"
	    here would send somebody to reconfigure a switch that is fine.
	  - all pass → the only positive statement about jumbo frames anywhere in
	    Ballast that is actually evidence rather than configuration.
*/
func DescribeJumboProbes(probes []JumboProbe, mtu int) string {
	if len(probes) == 0 {
		return "no path was tested: none of the targets share a subnet with a storage vNIC on this host, so a probe " +
			"would have crossed a router and measured its MTU instead"
	}
	var passed, mtuFail, unreachable []string
	for _, pr := range probes {
		label := pr.VNIC + " → " + pr.Target
		switch {
		case pr.OK:
			passed = append(passed, label)
		case pr.StandardOK:
			mtuFail = append(mtuFail, label)
		default:
			unreachable = append(unreachable, label)
		}
	}

	var parts []string
	if len(passed) > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d path(s) carried a %d-byte frame: %s",
			len(passed), len(probes), mtu, strings.Join(passed, ", ")))
	}
	if len(mtuFail) > 0 {
		parts = append(parts, fmt.Sprintf(
			"%s reachable but could NOT carry %d bytes — an ordinary ping crosses and a don't-fragment one at this "+
				"size does not. Both hosts are configured for it, so what is left is the physical switch between "+
				"them: jumbo frames need enabling on those switch ports. Ballast has no presence on the switch and "+
				"cannot do this",
			strings.Join(mtuFail, ", "), mtu))
	}
	if len(unreachable) > 0 {
		parts = append(parts, fmt.Sprintf(
			"%s did not answer an ordinary ping either, so this is not an MTU fault — the peer is down, its address "+
				"is wrong, or a firewall is dropping ICMP", strings.Join(unreachable, ", ")))
	}
	return strings.Join(parts, ". ") + "."
}

// JumboProbesFailed reports whether any probe found a genuine MTU fault. An
// unreachable peer is not one and must not turn the job red — the operator would
// go and configure a switch that was never the problem.
func JumboProbesFailed(probes []JumboProbe) bool {
	for _, pr := range probes {
		if !pr.OK && pr.StandardOK {
			return true
		}
	}
	return false
}

// ParseMTUParam reads a job's mtu parameter, defaulting when absent or unusable.
func ParseMTUParam(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < types.StandardMTU || n > 9216 {
		return types.DefaultJumboMTU
	}
	return n
}

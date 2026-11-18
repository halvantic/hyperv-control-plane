package reconcile

import (
	"fmt"
	"strings"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// vnicsWithOwnDNS returns the names of the management vNICs whose spec declares
// DNS servers of its own, which EnsureHostDNS must leave to the vNIC reconciler,
// and a note for each ROUTABLE one whose list differs from the host-level list.
//
// Only a routable vNIC (one with a gateway) is worth a note. An isolated fabric
// vNIC that declares DNS is unusual but deliberate; the host-level list was never
// going to land there.
func vnicsWithOwnDNS(vnics []types.ManagementVNICSpec, hostDNS []string) (own, differ []string) {
	for _, v := range vnics {
		if v.IPConfig == nil || len(v.IPConfig.DNSServers) == 0 {
			continue
		}
		own = append(own, v.Name)
		mine := normalizeDNS(v.IPConfig.DNSServers)
		if v.IPConfig.Gateway != "" && !sameStrings(mine, normalizeDNS(hostDNS)) {
			differ = append(differ, fmt.Sprintf("vNIC %q keeps its own DNS %s (host-level %s)",
				v.Name, strings.Join(mine, ","), strings.Join(hostDNS, ",")))
		}
	}
	return own, differ
}

// sameStrings compares element for element, order included: DNS order is
// resolver priority.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// normalizeDNS splits any entry that holds more than one server. A list saved
// as ["192.168.1.80; 192.168.1.81"] — one entry, typed with a semicolon — was
// applied as two servers by Windows, compared against the single stored entry,
// found to differ, and re-applied on every pass (prod, 2026-11-19). Normalised
// here so a list already stored that way works without anyone re-entering it.
// Returns the input unchanged when nothing needs splitting.
func normalizeDNS(in []string) []string {
	split := false
	for _, s := range in {
		if strings.ContainsAny(s, ",; \t") {
			split = true
			break
		}
	}
	if !split {
		return in
	}
	var out []string
	for _, s := range in {
		out = append(out, strings.FieldsFunc(s, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		})...)
	}
	return out
}

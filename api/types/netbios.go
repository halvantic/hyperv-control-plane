package types

import (
	"fmt"
	"strings"
)

// MaxNetBIOSName is the longest name Active Directory will hold as a computer
// object's NetBIOS (sAMAccountName) name. Windows does not reject a longer one —
// it silently TRUNCATES it, which is the whole reason this constant exists.
const MaxNetBIOSName = 15

// ValidateNetBIOSName reports why a cluster-facing name is unusable, or nil when
// it is fine. Cluster names, client access points and Replica Broker names all
// become AD computer objects and so all carry the NetBIOS length limit.
//
// A longer name fails in the worst possible way: nothing rejects it. The role is
// created, the client access point comes online, DNS registers the FULL name, the
// Network Name resource reports StatusDNS 0 and StatusKerberos 0, and every
// condition reads healthy. Meanwhile AD holds the object under the TRUNCATED name
// and the service's listener registers its SPN against that — a name with no DNS
// record. The only symptom is a listener failing with "No such host is known
// (0x80072AF9)", naming a host nobody configured.
//
// Observed on a live cluster: "bcluster2-Broker6" (17 characters) became the AD
// object "bcluster2-Broke", and three successive brokers over the 15-character
// limit all truncated onto that same object and all failed identically, while a
// 12-character name on the same rig had always worked.
func ValidateNetBIOSName(kind, name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if len(n) > MaxNetBIOSName {
		return fmt.Errorf("%s name %q is %d characters; Active Directory truncates a computer object's name to %d, "+
			"so the name that gets registered (%q) will not resolve and the role's listener fails with "+
			"\"No such host is known\" while everything else reports healthy — use %d characters or fewer",
			kind, n, len(n), MaxNetBIOSName, n[:MaxNetBIOSName], MaxNetBIOSName)
	}
	// A computer name cannot contain these, and a cluster refuses the role rather
	// than truncating, so this is the ordinary kind of validation error.
	if i := strings.IndexAny(n, `\/:*?"<>|,+=;[]`+"\t"); i >= 0 {
		return fmt.Errorf("%s name %q contains %q, which is not valid in a computer name", kind, n, n[i:i+1])
	}
	return nil
}

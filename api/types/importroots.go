package types

import (
	"sort"
	"strings"
)

/* Where to look for VMs that are on the storage but not registered.

   The roots come from the host's OWN reported volumes rather than from a
   configured list, which is what makes the cluster and standalone cases the
   same code. A cluster member reports its CSVs as C:\ClusterStorage\<name>; a
   standalone host reports its data drives as D:\, E:\. Both are storage the
   host can see and neither is the operating system, which is the only
   distinction that matters here.

   The brief asks for the standalone variant to be considered when a cluster
   capability is added, rather than after somebody hits the gap. For this
   capability the answer is that there is no variant to build: the question
   "what VMs are sitting on this storage" is identical on a CSV and on a data
   volume, so deriving the roots from reported volumes gets both at once. */

// ImportScanRoots is the set of paths worth walking for unregistered VMs.
//
// The system volume is excluded. It holds Windows, the page file and the
// default VM path, so walking it costs far more than the rest combined and the
// VMs on it are the ones already registered — the opposite of what this looks
// for. Cluster Shared Volumes are the exception and are always included: they
// mount UNDER the system drive at C:\ClusterStorage but are not part of it.
func ImportScanRoots(res HostResources) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		// Trailing separators make D:\ and D: different keys for one volume.
		k := strings.ToLower(strings.TrimRight(p, `\/`))
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, p)
	}
	for _, v := range res.Volumes {
		p := strings.TrimSpace(v.Path)
		if p == "" {
			continue
		}
		if isCSVPath(p) {
			add(p)
			continue
		}
		if isSystemVolume(p) {
			continue
		}
		add(p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// isCSVPath reports a Cluster Shared Volume mount. Matched on the path because
// that is what Windows guarantees — Add-ClusterSharedVolume mounts at
// C:\ClusterStorage\<name> regardless of which disk backs it.
func isCSVPath(p string) bool {
	return strings.Contains(strings.ToLower(strings.ReplaceAll(p, "/", `\`)), `\clusterstorage\`)
}

// isSystemVolume reports the drive Windows is installed on. Only the bare root
// counts: C:\ is the system volume, C:\ClusterStorage\DS1 is not, and the
// caller checks the CSV case first regardless.
func isSystemVolume(p string) bool {
	t := strings.ToLower(strings.TrimRight(strings.TrimSpace(p), `\/`))
	return t == "c:"
}

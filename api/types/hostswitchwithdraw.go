package types

// RemoveHostSwitch drops the switch with the given name from a host's
// desired-state switch list. Returns whether the slice changed.
//
// Exported (moved out of centre/rest, 2026-09-20) because cluster
// decommission needs the exact same withdrawal centre/rest's own cluster-edit
// path already performs: a switch declared at the CLUSTER level is fanned out
// into each MEMBER host's own desired state (see applyClusterToHosts), so
// deleting the cluster object alone leaves that fan-out in place. The agent
// then has no reason not to honour it — it never invents intent, and a
// desired-state switch it was never told to drop is not invented, it is
// simply still there. Reproduced live on WLGDC, 2026-09-20: decommissioning
// the cluster with switch cleanup selected removed the live switch via the
// RemoveSwitch job, but every member's own desired state still declared it,
// so the very next reconcile pass recreated it.
func RemoveHostSwitch(switches *[]VirtualSwitchSpec, name string) bool {
	out := (*switches)[:0]
	removed := false
	for _, sw := range *switches {
		if sw.Name == name {
			removed = true
			continue
		}
		out = append(out, sw)
	}
	*switches = out
	return removed
}

// RemoveHostVNICsOnSwitch drops every management vNIC attached to
// switchName from a host's desired state. Returns whether the slice changed.
// See RemoveHostSwitch's doc comment for why this is exported here.
func RemoveHostVNICsOnSwitch(vnics *[]ManagementVNICSpec, switchName string) bool {
	out := (*vnics)[:0]
	removed := false
	for _, v := range *vnics {
		if v.SwitchName == switchName {
			removed = true
			continue
		}
		out = append(out, v)
	}
	*vnics = out
	return removed
}

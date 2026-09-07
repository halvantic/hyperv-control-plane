package types

import "strings"

/* A guest that is running and not answering Key-Value Pair Exchange.

   Hyper-V's integration services are two halves. The transport is in the guest
   KERNEL — on Linux that is hv_utils, shipped in mainline and present on every
   Rocky 8/9 boot, which is why there are no drivers to deploy. The USERSPACE
   daemons are a separate package, and Key-Value Pair Exchange is one of them:
   hypervkvpd on Linux, the Hyper-V Data Exchange service on Windows.

   KVP is how a guest's OS name and IP addresses reach the host. Without the
   daemon, Hyper-V still reports the service "enabled" — the setting is on the
   VM, not in the guest — and the console simply shows a running machine with
   no OS and no address, with nothing anywhere saying why. That is a fact
   Ballast can establish and was leaving as blanks for somebody to research.

   HEARTBEAT IS THE DISCRIMINATOR, and it is what makes this precise rather
   than a guess. Heartbeat comes from the kernel module; KVP needs the daemon.
   A guest answering the first and not the second has the drivers and is
   missing the package — which is exactly one remedy, nameable in the message.
   A guest answering neither is simply not running the integration services at
   all, or has not finished booting, and gets no accusation.
*/

// KVPServiceName is what Hyper-V calls the service, in its own spelling, so a
// message quoting it matches what Get-VMIntegrationService prints.
const KVPServiceName = "Key-Value Pair Exchange"

/* kvpSettleSeconds is how long a guest is given before its silence means
   anything.

   The daemons start with the rest of userspace, so a machine thirty seconds
   into boot is expected to be quiet. Accusing it of a missing package is the
   false-alarm pattern this codebase keeps paying for, so the window is
   generous — a real missing daemon is still missing in three minutes. */
const kvpSettleSeconds = 180

/*
GuestKVPProblem reports a running guest whose KVP daemon is not answering.

	Returns "" when there is nothing to say, which is almost always. Every input
	is something the VM already reports, so this costs nothing and needs no
	agent change.

	kvpEnabled is whether Hyper-V has the service switched on for this VM;
	nothing is inferred when it is off, because then the silence is the setting
	working as asked.
*/
func GuestKVPProblem(st VMStatus, kvpEnabled bool) string {
	if st.PowerState != VMPowerRunning || !kvpEnabled {
		return ""
	}
	// Not long enough up for silence to mean anything.
	if st.UptimeSeconds < kvpSettleSeconds {
		return ""
	}
	// Something is already coming back, so the daemon is answering.
	if strings.TrimSpace(st.GuestOS) != "" || strings.TrimSpace(st.IPAddress) != "" {
		return ""
	}
	/* No heartbeat either. The guest is not running the integration services at
	   all — an appliance, a live ISO, a kernel without the modules — and that is
	   a different situation with a different answer. Naming a package here
	   would send somebody to install one thing for a machine missing another. */
	if !heartbeatAnswering(st.Heartbeat) {
		return ""
	}
	return "This guest answers Hyper-V's heartbeat but not " + KVPServiceName + ", so its OS name and IP addresses " +
		"never reach the console — which is why both are blank above. The heartbeat comes from the guest's kernel " +
		"and is working; the exchange needs a userspace daemon that is not running. On Rocky or another RHEL-like " +
		"distribution: install hyperv-daemons and enable hypervkvpd. On Windows: start the Hyper-V Data Exchange " +
		"service. There are no Hyper-V drivers to install on a modern Linux guest — they are in the kernel already."
}

/* heartbeatAnswering reads Hyper-V's own verdict on the guest.

   The values are "Ok", "OkApplicationsHealthy", "OkApplicationsUnknown",
   "Error", "Lost", "Disabled", "Paused", or empty when nothing is reported.
   Only the Ok family means a guest is answering; empty means nothing was said,
   which is not the same as a fault and must not be read as one.
*/
func heartbeatAnswering(h string) bool {
	return strings.HasPrefix(strings.TrimSpace(h), "Ok")
}

/* KVPEnabledIn reports whether the observed service list has KVP switched on.

   Absent means the host has not reported the services yet, and that is NOT the
   same as disabled — a VM nobody has read must not be assumed to have the
   service off, which would silence the check on exactly the machines nobody has
   looked at.
*/
func KVPEnabledIn(list []VMIntegrationServiceState) (enabled, known bool) {
	for _, s := range list {
		if strings.EqualFold(s.Name, KVPServiceName) {
			return s.Enabled, true
		}
	}
	return false, false
}

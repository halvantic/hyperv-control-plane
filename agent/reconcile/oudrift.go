package reconcile

import (
	"strings"
	"time"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// ouDriftCondition compares a host's observed AD OU against its declared
// HostSpec.DomainJoin.OUPath and reports the fact plainly. It never suggests
// Ballast can fix a mismatch itself: moving a computer object needs
// Create/Delete Child Objects on both OUs, which the least-privilege posture
// this exists for deliberately does not grant (see
// docs/agent-least-privilege-ad.md). Naming the cause and the one manual step
// is what CLAUDE.md asks for when the fix genuinely sits outside what the
// account is allowed to do.
//
// DNs are case-insensitive in AD, so the comparison is too. observedOU is
// trusted as already the OU portion of a distinguishedName (see
// hyperv.PowerShell.GetComputerOU / ouFromDN) — this function does no DN
// parsing of its own.
func ouDriftCondition(observedOU, desiredOU string, now time.Time) types.Condition {
	c := types.Condition{Type: "DomainJoin/OU", LastTransitionTime: now}
	if strings.EqualFold(strings.TrimSpace(observedOU), strings.TrimSpace(desiredOU)) {
		c.Status = true
		c.Reason = "InDeclaredOU"
		c.Message = "computer object is in the declared OU"
		return c
	}
	c.Status = false
	c.Reason = "WrongOU"
	c.Message = "computer object is in " + observedOU + ", declared OUPath is " + desiredOU +
		". Ballast does not move computer objects between OUs (it would need broader AD rights than this account is delegated) — " +
		"move it yourself: Move-ADObject -Identity <the computer's DN> -TargetPath '" + desiredOU + "'"
	return c
}

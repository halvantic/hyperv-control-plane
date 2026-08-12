package reconcile

import (
	"fmt"
	"strings"
	"time"

	"github.com/joshua-fourie/ballast/agent/hyperv"
)

// Pass timing.
//
// Every host on the rig reported 100-170 seconds apart on a 15-second heartbeat,
// with a 90-second staleness threshold — so all five sat permanently on the wrong
// side of "online". A host that reads as offline shows its network as unknown,
// drops out of the agent update list, and is skipped when the centre picks a
// member to report a cluster. Three separate symptoms, none of them about
// networks, agents or clusters.
//
// Which step is slow was not knowable from any of that. Guessing produced two
// wrong causes in one evening; the wins came from making the failing step report
// what it saw. This is that, applied to time.
//
// That original chain no longer holds and is kept here as the reason this exists,
// not as a description of what happens now: the agent's keepalive reports on its
// own interval, so a slow pass costs freshness rather than liveness. Timing the
// pass is worth as much either way — it is the only thing that says WHICH call
// went slow — but do not re-derive the offline symptoms from it.
// passTimer measures the pass as a whole. WHICH call was slow is recorded at the
// PowerShell boundary instead of around each step: every step shells out, so that
// one place catches all of them — including the ones nobody thought to
// instrument, which is where a surprise will be.
type passTimer struct {
	start time.Time
}

func newPassTimer() *passTimer { return &passTimer{start: time.Now()} }

func (p *passTimer) total() time.Duration {
	if p == nil {
		return 0
	}
	return time.Since(p.start)
}

// slowPassThreshold is twice the default heartbeat. Past it the pass, not the
// heartbeat, is what sets how often this host is looked at — passes start running
// back to back and the cadence the agent was configured with stops being the one
// it keeps.
//
// It used to be justified as "well below the centre's staleness cutoff", which
// was the right reasoning when a slow pass could make a host read offline. The
// keepalive ended that; see slowPassMessage.
const slowPassThreshold = 30 * time.Second

// slowPassMessage names the calls that account for a slow pass.
//
// The agent's reconcile cadence is deliberately tiered — some observations run
// every pass and some every Nth — so a heavy pass is not the same as a slow one.
// Naming the calls is what tells them apart: a heavy pass is several reads that
// each cost what they always cost, and a slow one is a single call that has
// stopped answering in the time it used to.
//
// The CONSEQUENCE this states was rewritten when the agent gained its keepalive.
// It used to say that a slow pass makes the host report late and therefore read
// as offline, with its network unknown and its name missing from the agent update
// list. That was true when the only report was the one at the end of a pass; the
// keepalive resends the last status on a fixed 25s interval precisely so it is
// not, so the sentence outlived the thing it described and told an operator to
// expect symptoms that can no longer occur.
//
// What a slow pass costs now is freshness: the pass is how often this host is
// actually looked at, so everything observed in it ages with it, while the
// keepalive holds the host online regardless. That is the gap HostStatus.
// ObservedAt exists to show, and pointing at it beats describing it.
func slowPassMessage(calls []hyperv.CallTiming, total, threshold time.Duration) string {
	if total < threshold {
		return ""
	}
	var parts []string
	for _, c := range calls {
		if len(parts) >= 4 || c.Took < 500*time.Millisecond {
			break
		}
		part := c.Name + " " + round(c.Took)
		if c.Calls > 1 {
			part += fmt.Sprintf(" (%d calls)", c.Calls)
		}
		parts = append(parts, part)
	}
	// One consequence sentence, shared, so the two shapes cannot drift apart.
	const cost = " A pass is how often this host is actually read, so everything on it — metrics, inventory, VM and cluster state — is up to that old, and a change to desired state waits that long to be applied. The agent keeps reporting between passes, so the host still reads online; \"Readings taken\" on its card is the age of what is shown."
	if len(parts) == 0 {
		return "this pass took " + round(total) + ", and no single host call accounts for it." + cost
	}
	return "this pass took " + round(total) + " — slowest: " + strings.Join(parts, ", ") + "." + cost
}

// round trims the precision to something an operator reads rather than parses.
func round(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

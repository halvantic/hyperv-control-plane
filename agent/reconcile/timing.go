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

// slowPassThreshold is well below the centre's staleness cutoff: a pass that is
// merely slow is worth seeing BEFORE it is slow enough to make the host vanish.
const slowPassThreshold = 30 * time.Second

// slowPassMessage names the calls that account for a slow pass.
//
// The agent's reconcile cadence is deliberately tiered — some observations run
// every pass and some every Nth — so a heavy pass is not the same as a slow one.
// Naming the calls is what tells them apart: a heavy pass is several reads that
// each cost what they always cost, and a slow one is a single call that has
// stopped answering in the time it used to.
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
	if len(parts) == 0 {
		return "this pass took " + round(total) + ", and no single host call accounts for it."
	}
	return "this pass took " + round(total) + " — slowest: " + strings.Join(parts, ", ") +
		". A pass longer than the agent's heartbeat makes this host report late, and a host that reports late reads as offline: its network shows as unknown, it drops out of the agent update list, and it is passed over when a cluster needs a member to report it."
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

package reconcile

import (
	"fmt"
	"sort"
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
//
// WHICH call was slow is recorded at the PowerShell boundary instead of around
// each step: every step shells out, so that one place catches all of them —
// including the ones nobody thought to instrument, which is where a surprise
// will be.
//
// THE INVARIANT, and it has now been broken twice: the accumulator is a single
// shared bucket, and TakeTimings claims EVERYTHING put in it since the last
// take. So whoever drains it must be timing the same window. Pair them or the
// list is attributed to the wrong pass — and a list of calls against a duration
// they were not part of is not a slow number, it is a wrong one.
//
// It first broke on the early-return paths inside Reconcile, which drained only
// at the bottom and left their calls for the next pass to report as its own. It
// broke again the other way round: the timer covered Reconcile while the drain
// swept up ReconcileCluster too, which the runner calls AFTERWARDS, so a
// four-minute GetClusterState from one cycle was billed to the next cycle's
// 1m14s pass. Seen on the rig 2026-08-13.
//
// Hence both now live in the runner, at the cycle boundary: one timer, one
// drain, the same window, covering everything the cycle does — the runner's own
// collections, the host reconcile and the cluster reconcile alike. A cycle is
// also what the message's closing sentence actually describes.

// SlowPassThreshold is twice the default heartbeat. Past it the pass, not the
// heartbeat, is what sets how often this host is looked at — passes start running
// back to back and the cadence the agent was configured with stops being the one
// it keeps.
//
// It used to be justified as "well below the centre's staleness cutoff", which
// was the right reasoning when a slow pass could make a host read offline. The
// keepalive ended that; see SlowPassMessage.
const SlowPassThreshold = 30 * time.Second

// PhaseTiming is one stage of a cycle and what it cost — metrics, inventory,
// hostReconcile, journal, deliver, clusterReconcile.
//
// It exists because the CALL list cannot explain every slow pass. Host calls are
// timed at the PowerShell boundary, so anything that is not a cmdlet is invisible
// to them: a local journal fsync on a busy disk, a round trip to the centre, a
// process descheduled under memory pressure. HVNEW04 — a healthy host — spent
// more than half of a 1m49s pass outside any host call, and the slowest-call list
// could only name 7% of it while looking like an explanation.
//
// The phases cover the whole cycle, so between them they always account for it.
type PhaseTiming struct {
	Name string
	Took time.Duration
}

// SlowPassMessage names the calls that account for a slow pass.
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
func SlowPassMessage(calls []hyperv.CallTiming, phases []PhaseTiming, total, threshold time.Duration, cutOff bool) string {
	// A cut-off pass is always worth reporting, however long it ran: it did not
	// finish, so everything after the stall never happened at all.
	if total < threshold && !cutOff {
		return ""
	}
	var parts []string
	var named, all time.Duration
	for _, c := range calls {
		all += c.Took
	}
	for _, c := range calls {
		if len(parts) >= 4 || c.Took < 500*time.Millisecond {
			break
		}
		part := c.Name + " " + round(c.Took)
		if c.Calls > 1 {
			part += fmt.Sprintf(" (%d calls)", c.Calls)
		}
		parts = append(parts, part)
		named += c.Took
	}
	// When the named calls are not most of the pass, say so. The list is capped at
	// four, so a pass whose cost is spread thin reads exactly like one with a
	// culprit — and those are opposite diagnoses. A pass of 3m30s whose four
	// slowest calls total 1m37s has no single thing to fix, and an operator sent
	// hunting for one is being sent by the message rather than by the evidence.
	remainder := ""
	if len(parts) > 0 && named*2 < total {
		remainder = " Those are " + round(named) + " of it"
		if rest := all - named; rest > time.Second {
			remainder += fmt.Sprintf(", a further %d host calls account for %s", len(calls)-len(parts), round(rest))
		}
		remainder += ", and the rest was not spent in a host call."
		// Which is only half an answer without saying where it WAS spent. The
		// phases cover the whole cycle including the parts no cmdlet touches, so
		// they turn "not in a host call" from an intriguing fact into an
		// actionable one — and save someone reading the agent's log on the host to
		// find out, which is the thing CLAUDE.md calls a defect.
		if p := topPhases(phases, total); p != "" {
			remainder += " Where it went: " + p + "."
		}
	}
	// One consequence sentence, shared, so the two shapes cannot drift apart.
	const cost = " A pass is how often this host is actually read, so everything on it — metrics, inventory, VM and cluster state — is up to that old, and a change to desired state waits that long to be applied. The agent keeps reporting between passes, so the host still reads online; \"Readings taken\" on its card is the age of what is shown."
	// "The last completed pass", not "this pass": the timings are drained when a
	// cycle ends, so the message travels on the NEXT status report. Calling it
	// "this" would put a finished pass's cost against the one now running, which
	// is a smaller version of the mistake this whole file is about.
	// "Cut off", not "took": a cycle killed at its cap did not complete, and
	// calling it completed hides the fact that the steps after the stall never
	// ran — which is what NotAttempted conditions elsewhere are trying to say.
	lead := "the last completed pass took " + round(total)
	if cutOff {
		lead = "the last pass was CUT OFF after " + round(total) + " — it hit the cycle limit and did not finish, so anything after the slow step never ran"
	}
	if len(parts) == 0 {
		return lead + ", and no single host call accounts for it." + cost
	}
	return lead + " — slowest: " + strings.Join(parts, ", ") + "." + remainder + cost
}

// topPhases renders the costliest stages of the cycle, biggest first.
//
// Phases that are ENTIRELY host calls are dropped: naming "metrics 9s" beside
// "CollectMetrics 9s" says the same thing twice and pushes the useful line off
// the end. What earns its place is a phase the call list cannot see into —
// journal, deliver — or one large enough that the calls inside it do not add up.
//
// Sized against the PASS, not against a fixed floor: a phase worth naming is one
// that is a real share of what took so long. Three seconds is trivia in a
// two-minute pass and most of the story in a thirty-second one, and a list that
// includes both stops being read.
func topPhases(phases []PhaseTiming, total time.Duration) string {
	floor := total / 20 // 5% of the pass
	if floor < time.Second {
		floor = time.Second
	}
	sorted := make([]PhaseTiming, 0, len(phases))
	for _, p := range phases {
		if p.Took >= floor {
			sorted = append(sorted, p)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Took > sorted[j].Took })
	var out []string
	for _, p := range sorted {
		if len(out) >= 4 {
			break
		}
		out = append(out, p.Name+" "+round(p.Took))
	}
	return strings.Join(out, ", ")
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

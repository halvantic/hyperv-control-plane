package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/joshua-fourie/ballast/agent/hyperv"
)

/* Every host reported 100-170 seconds apart on a 15-second heartbeat, so all five
   sat permanently past the 90-second staleness cutoff. That showed up as unknown
   networks, hosts missing from the agent update list, and clusters with no member
   to report them — three symptoms, none naming the cause, and two wrong causes
   guessed before anything was measured. */

func TestAQuickPassSaysNothing(t *testing.T) {
	got := slowPassMessage([]hyperv.CallTiming{{Name: "CollectInventory", Took: time.Second}},
		2*time.Second, slowPassThreshold)
	if got != "" {
		t.Fatalf("a healthy pass must stay silent, got %q", got)
	}
}

// The cadence is tiered on purpose, so a heavy pass is not a slow one. Naming the
// calls is what tells them apart.
func TestASlowPassNamesItsSlowestCalls(t *testing.T) {
	got := slowPassMessage([]hyperv.CallTiming{
		{Name: "GetClusterState", Took: 90 * time.Second, Calls: 1},
		{Name: "ObserveVMs", Took: 12 * time.Second, Calls: 3},
		{Name: "CollectMetrics", Took: 20 * time.Millisecond, Calls: 1},
	}, 105*time.Second, slowPassThreshold)

	if !strings.Contains(got, "GetClusterState 1m30s") {
		t.Errorf("the slowest call must be named with its cost: %q", got)
	}
	if !strings.Contains(got, "(3 calls)") {
		t.Errorf("a call made repeatedly must say so — three at four seconds is a different problem from one at twelve: %q", got)
	}
	// Sub-second calls are noise: a list of them buries the one that matters.
	if strings.Contains(got, "CollectMetrics") {
		t.Errorf("trivial calls must not pad the list: %q", got)
	}
}

// The consequence is the part an operator cannot see for themselves — and it
// changed when the agent gained its keepalive.
//
// The message used to say a slow pass makes the host read offline, with its
// network unknown and its name gone from the agent update list. The keepalive
// reports on its own interval precisely so that no longer happens, so the
// sentence had outlived what it described and sent an operator looking for
// symptoms that cannot occur. What a slow pass costs now is freshness: the pass
// is how often the host is read, while it goes on reading online throughout.
func TestTheMessageExplainsWhySlownessMatters(t *testing.T) {
	got := slowPassMessage([]hyperv.CallTiming{{Name: "GetClusterState", Took: 90 * time.Second, Calls: 1}},
		105*time.Second, slowPassThreshold)
	for _, want := range []string{"how often this host is actually read", "still reads online", "Readings taken"} {
		if !strings.Contains(got, want) {
			t.Errorf("the message must connect slowness to what it costs (%q): %q", want, got)
		}
	}
	// The old chain must not come back: it describes a failure the keepalive
	// removed, and an operator acting on it would be chasing nothing.
	for _, gone := range []string{"reads as offline", "agent update list"} {
		if strings.Contains(got, gone) {
			t.Errorf("the message still claims %q, which the keepalive made false: %q", gone, got)
		}
	}
}

// Both shapes of the message carry the consequence: a slow pass with nothing to
// blame costs exactly as much freshness as one with a named culprit, and an
// operator reading the first should not have to know the second exists.
func TestTheConsequenceIsStatedEvenWithNoCulprit(t *testing.T) {
	got := slowPassMessage(nil, 60*time.Second, slowPassThreshold)
	if !strings.Contains(got, "how often this host is actually read") {
		t.Errorf("a slow pass with no named call still costs freshness and must say so: %q", got)
	}
}

// A slow pass with nothing to blame is still worth reporting: the time went
// somewhere, and saying so is honest where naming a suspect would not be.
func TestASlowPassWithNoCulpritStillReports(t *testing.T) {
	got := slowPassMessage(nil, 60*time.Second, slowPassThreshold)
	if !strings.Contains(got, "no single host call accounts for it") {
		t.Fatalf("it must still report, without inventing a cause: %q", got)
	}
}

package hyperv

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Call timing.
//
// Every host on the rig reported 100-170 seconds apart on a 15-second heartbeat,
// against a 90-second staleness threshold, so all five sat permanently on the
// wrong side of "online" — which showed up as unknown networks, hosts missing
// from the agent update list, and clusters with no member to report them. None of
// those symptoms named the cause, and reasoning about which step was slow
// produced two wrong answers in one evening.
//
// Timing lives HERE rather than around each reconcile step because every step
// shells out to PowerShell, so this one boundary catches all of them — including
// the ones nobody thought to instrument, which is where a surprise will be.
//
// The caller's own method name is the label. It needs no annotation at each call
// site, so it cannot fall out of date with the code, and it names something an
// engineer can find: GetClusterState, EnsureSwitch, ObserveVMs.
type callTimings struct {
	mu    sync.Mutex
	total map[string]time.Duration
	count map[string]int
}

func newCallTimings() *callTimings {
	return &callTimings{total: map[string]time.Duration{}, count: map[string]int{}}
}

func (c *callTimings) add(name string, d time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total[name] += d
	c.count[name]++
}

// CallTiming is one method's share of a pass.
type CallTiming struct {
	Name  string
	Took  time.Duration
	Calls int
}

// TakeTimings returns the calls made since the last take, slowest first, and
// clears the record. Taken rather than read so each pass reports its own cost
// instead of an average that hides a single slow pass among fast ones.
func (p *PowerShell) TakeTimings() []CallTiming {
	c := p.timings
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CallTiming, 0, len(c.total))
	for name, d := range c.total {
		out = append(out, CallTiming{Name: name, Took: d, Calls: c.count[name]})
	}
	c.total = map[string]time.Duration{}
	c.count = map[string]int{}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Took > out[j].Took })
	return out
}

// timed records how long a call took against the exported method that made it.
// Deferred at the top of the run helpers, so every script is covered by the two
// places that actually execute one.
func (p *PowerShell) timed(start time.Time) {
	if p == nil || p.timings == nil {
		return
	}
	p.timings.add(callerName(), time.Since(start))
}

// callerName walks out of this package's own plumbing to the method a reconciler
// called. Without the walk every timing reads "run", which names the mechanism
// and not the work.
func callerName() string {
	var pcs [12]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		name := f.Function
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		// Skip the helpers that every call passes through on its way out.
		switch name {
		case "timed", "run", "run2", "runStream", "runWithEnvOut", "runWithEnv", "func1", "":
		default:
			return name
		}
		if !more {
			break
		}
	}
	return "powershell"
}

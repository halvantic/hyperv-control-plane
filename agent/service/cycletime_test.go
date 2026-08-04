package main

import (
	"testing"
	"time"
)

// A phase that did not run must not be reported at all. Logging "inventory=0ms"
// beside a cycle that actually collected it means the opposite thing, and a zero
// that means "skipped" is exactly how a throttled observation gets read as a
// fast one — which would send someone optimising the wrong phase.
func TestSkippedPhasesAreNotReported(t *testing.T) {
	var pt phaseTimer
	pt.mark("metrics")()

	fields := pt.fields()
	if len(fields) != 2 || fields[0] != "metrics" {
		t.Fatalf("only the phase that ran should appear, got %v", fields)
	}
	for _, f := range fields {
		if s, ok := f.(string); ok && s == "inventory" {
			t.Fatal("a phase that never ran must not be logged as zero")
		}
	}
}

// A phase that runs per VM or per vNIC is reported as its total. That total is
// the figure that decides whether batching the calls is worth doing, so summing
// rather than overwriting is the point of the type.
func TestRepeatedPhasesAccumulate(t *testing.T) {
	var pt phaseTimer
	for i := 0; i < 3; i++ {
		done := pt.mark("vmReconcile")
		time.Sleep(2 * time.Millisecond)
		done()
	}
	fields := pt.fields()
	if len(fields) != 2 {
		t.Fatalf("repeats must collapse to one total, got %v", fields)
	}
	d, ok := fields[1].(time.Duration)
	if !ok {
		t.Fatalf("phase value should be a duration, got %T", fields[1])
	}
	if d < 5*time.Millisecond {
		t.Fatalf("three 2ms runs should total ~6ms, got %v", d)
	}
}

// Order is the order phases first ran, so the log line reads as the cycle did.
func TestPhasesReportInTheOrderTheyRan(t *testing.T) {
	var pt phaseTimer
	pt.mark("metrics")()
	pt.mark("hostReconcile")()
	pt.mark("metrics")() // a repeat must not reorder anything
	pt.mark("vmReconcile")()

	fields := pt.fields()
	var names []string
	for i := 0; i < len(fields); i += 2 {
		names = append(names, fields[i].(string))
	}
	want := []string{"metrics", "hostReconcile", "vmReconcile"}
	if len(names) != len(want) {
		t.Fatalf("got phases %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got phases %v, want %v", names, want)
		}
	}
}

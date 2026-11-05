package worker

import (
	"log/slog"
	"testing"
	"time"
)

// testBreaker returns a breaker on a fake clock and a function that moves the clock.
func testBreaker(threshold int, cooldown time.Duration) (*breaker, func(time.Duration)) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	b := newBreaker(threshold, cooldown, slog.New(slog.DiscardHandler))
	b.now = func() time.Time { return now }
	return b, func(d time.Duration) { now = now.Add(d) }
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b, _ := testBreaker(3, time.Minute)

	b.record(false, true)
	b.record(false, true)
	b.record(false, false) // a success resets the run
	b.record(false, true)
	b.record(false, true)
	if !b.closed() {
		t.Fatal("opened after 2 failures in a row; threshold is 3")
	}
	b.record(false, true)
	if b.closed() {
		t.Fatal("still closed after 3 failures in a row")
	}
	if _, wait, ok := b.gate(); ok || wait != time.Minute {
		t.Errorf("gate while open = ok %t, wait %v; want to wait the full cool-down", ok, wait)
	}
}

func TestBreakerProbesAfterTheCooldown(t *testing.T) {
	b, advance := testBreaker(1, time.Minute)
	b.record(false, true) // open

	advance(40 * time.Second)
	if _, wait, ok := b.gate(); ok || wait != 20*time.Second {
		t.Errorf("gate 40s into the cool-down = ok %t, wait %v; want to wait 20s", ok, wait)
	}

	advance(20 * time.Second)
	probe, _, ok := b.gate()
	if !ok || !probe {
		t.Fatalf("gate after the cool-down = probe %t, ok %t; want the probe", probe, ok)
	}
	if b.closed() {
		t.Error("ordinary sends allowed while half-open")
	}
	// Only one probe at a time.
	if probe, wait, ok := b.gate(); ok || probe || wait != 0 {
		t.Errorf("second gate while probing = probe %t, wait %v, ok %t; want to wait a poll interval", probe, wait, ok)
	}

	b.record(true, false) // the provider is back
	if !b.closed() {
		t.Error("a successful probe did not close the breaker")
	}
	if probe, _, ok := b.gate(); !ok || probe {
		t.Errorf("gate after recovery = probe %t, ok %t; want normal claims", probe, ok)
	}
}

func TestBreakerFailedProbeStartsANewCooldown(t *testing.T) {
	b, advance := testBreaker(1, time.Minute)
	b.record(false, true)
	advance(time.Minute)
	if probe, _, _ := b.gate(); !probe {
		t.Fatal("no probe after the cool-down")
	}

	advance(5 * time.Second) // the probe took 5 s and failed
	b.record(true, true)
	if _, wait, ok := b.gate(); ok || wait != time.Minute {
		t.Errorf("gate after a failed probe = ok %t, wait %v; want a fresh full cool-down", ok, wait)
	}
}

// Ordinary sends that started before the breaker opened finish later. Their results
// must neither close an open breaker nor count towards the next opening.
func TestBreakerIgnoresLateResults(t *testing.T) {
	b, advance := testBreaker(2, time.Minute)
	b.record(false, true)
	b.record(false, true) // open

	b.record(false, false)
	if b.closed() {
		t.Error("a late success closed the breaker without a probe")
	}

	advance(time.Minute)
	b.gate()              // half-open, probe out
	b.record(false, true) // a late failure is not the probe's answer
	b.record(true, false)
	if !b.closed() {
		t.Fatal("probe success did not close the breaker")
	}
	b.record(false, true)
	if !b.closed() {
		t.Error("one failure after closing opened the breaker; the run must start from zero")
	}
}

func TestBreakerAbortProbeFreesTheSlot(t *testing.T) {
	b, advance := testBreaker(1, time.Second)
	b.record(false, true)
	advance(time.Second)
	b.gate()
	b.abortProbe() // nothing was due, so nothing was sent
	if probe, _, ok := b.gate(); !ok || !probe {
		t.Errorf("gate after abortProbe = probe %t, ok %t; want a new probe", probe, ok)
	}
}

package worker

import (
	"testing"
	"time"
)

// TestBackoffCeilings checks the ADR-0002 curve with the defaults: the waits are
// bounded by 2, 4, 8, ... seconds and never exceed the 5 minute cap.
func TestBackoffCeilings(t *testing.T) {
	b := newBackoff(2*time.Second, 5*time.Minute)
	want := []time.Duration{
		2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second,
		5 * time.Minute, 5 * time.Minute,
	}
	for i, w := range want {
		if got := b.ceiling(i + 1); got != w {
			t.Errorf("ceiling after %d failed attempts = %v, want %v", i+1, got, w)
		}
	}
	if got := b.ceiling(1000); got != 5*time.Minute {
		t.Errorf("ceiling after 1000 attempts = %v, want the cap", got)
	}
}

// TestBackoffFullJitter pins the random source to its extremes: the wait can be as
// short as zero and as long as the ceiling, inclusive.
func TestBackoffFullJitter(t *testing.T) {
	lowest := backoff{base: 2 * time.Second, max: time.Minute, randN: func(int64) int64 { return 0 }}
	highest := backoff{base: 2 * time.Second, max: time.Minute, randN: func(n int64) int64 { return n - 1 }}

	for attempt := 1; attempt <= 8; attempt++ {
		if got := lowest.delay(attempt, 0); got != 0 {
			t.Errorf("attempt %d: lowest delay = %v, want 0", attempt, got)
		}
		if got, want := highest.delay(attempt, 0), highest.ceiling(attempt); got != want {
			t.Errorf("attempt %d: highest delay = %v, want the ceiling %v", attempt, got, want)
		}
	}
}

func TestBackoffRealRandomStaysInBounds(t *testing.T) {
	b := newBackoff(10*time.Millisecond, time.Second)
	for attempt := 1; attempt <= 10; attempt++ {
		c := b.ceiling(attempt)
		for range 200 {
			if d := b.delay(attempt, 0); d < 0 || d > c {
				t.Fatalf("attempt %d: delay %v outside [0, %v]", attempt, d, c)
			}
		}
	}
}

func TestBackoffHonoursRetryAfterWithinTheCap(t *testing.T) {
	b := backoff{base: time.Second, max: time.Minute, randN: func(int64) int64 { return 0 }}
	if got := b.delay(1, 7*time.Second); got != 7*time.Second {
		t.Errorf("Retry-After 7s: delay = %v, want 7s", got)
	}
	if got := b.delay(1, time.Hour); got != time.Minute {
		t.Errorf("Retry-After 1h: delay = %v, want the 1m cap", got)
	}
}

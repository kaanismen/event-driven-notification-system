package worker

import (
	"log/slog"
	"sync"
	"time"
)

// breakerState is the state of one channel's circuit breaker (ADR-0015).
type breakerState int

const (
	// breakerClosed is normal operation: rows are claimed and sent.
	breakerClosed breakerState = iota
	// breakerOpen means the provider looks down: nothing is claimed or sent until the
	// cool-down is over, so waiting messages keep their attempts.
	breakerOpen
	// breakerHalfOpen means the cool-down is over: exactly one probe message is sent to
	// find out whether the provider is back.
	breakerHalfOpen
)

func (s breakerState) String() string {
	switch s {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// breaker stops a channel from sending into a provider outage. It opens after threshold
// transient failures in a row, stays open for cooldown, then lets one probe through:
// success closes it, failure opens it again for another cool-down.
//
// Only transient failures count. Any other answer, a 400 included, proves the provider
// is up and reachable, so it counts as healthy here; whether the message itself is
// valid is the retry policy's business, not the breaker's.
type breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time // time.Now in production; tests move a fake clock
	logger    *slog.Logger

	mu       sync.Mutex
	state    breakerState
	failures int       // transient failures in a row while closed
	openedAt time.Time // when the current open period started
	probing  bool      // a probe is out and its result not yet recorded
}

func newBreaker(threshold int, cooldown time.Duration, logger *slog.Logger) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown, now: time.Now, logger: logger}
}

// gate tells the dispatcher what it may do before its next claim:
//
//   - ok and not probe: closed, claim as usual.
//   - ok and probe: the cool-down is over; claim one row and send it as the probe. The
//     probe slot is now taken, and is freed by record or abortProbe.
//   - not ok: wait. wait is the rest of the cool-down, or 0 while a probe is out (the
//     dispatcher then waits one poll interval).
func (b *breaker) gate() (probe bool, wait time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return false, 0, true
	case breakerOpen:
		if left := b.cooldown - b.now().Sub(b.openedAt); left > 0 {
			return false, left, false
		}
		b.setState(breakerHalfOpen)
	}
	if b.probing {
		return false, 0, false
	}
	b.probing = true
	return true, 0, true
}

// closed reports whether ordinary (non-probe) sends may go ahead. A sender checks it
// right before sending, because the breaker may have opened while it waited for a rate
// slot; it then hands the row back instead of sending into the outage.
func (b *breaker) closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == breakerClosed
}

// record reports the outcome of a send. providerDown is true for transient failures.
// Results of ordinary sends that started before the breaker opened can still arrive
// while it is open or half-open; they say nothing new and are ignored.
func (b *breaker) record(probe, providerDown bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if probe {
		b.probing = false
		if providerDown {
			b.open()
		} else {
			b.failures = 0
			b.setState(breakerClosed)
		}
		return
	}
	if b.state != breakerClosed {
		return
	}
	if !providerDown {
		b.failures = 0
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.open()
	}
}

// abortProbe frees the probe slot when the probe was never sent (no row was due, the
// claim failed, or the engine is shutting down).
func (b *breaker) abortProbe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

// open must be called with mu held.
func (b *breaker) open() {
	b.openedAt = b.now()
	b.failures = 0
	b.setState(breakerOpen)
}

// setState must be called with mu held. Every change is logged, so an outage and the
// recovery are visible in the logs (a metric follows in Phase 5).
func (b *breaker) setState(to breakerState) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	b.logger.Warn("circuit breaker state changed", "from", from.String(), "to", to.String(),
		"threshold", b.threshold, "cooldown", b.cooldown.String())
}

package worker

import (
	"math/rand/v2"
	"time"
)

// backoff computes the wait before the next attempt with "full jitter" (ADR-0002): a
// random wait between zero and an exponentially growing ceiling. The randomness spreads
// out the retries of messages that failed together, for example during a provider
// outage, so they do not all hit the provider again at the same moment.
type backoff struct {
	base, max time.Duration
	// randN returns a random number in [0, n). Tests replace it to check the bounds.
	randN func(n int64) int64
}

func newBackoff(base, maxDelay time.Duration) backoff {
	// The top-level math/rand/v2 functions are safe for concurrent use and seeded
	// randomly; this is not a security decision, so crypto/rand is not needed.
	return backoff{base: base, max: maxDelay, randN: rand.Int64N}
}

// ceiling is the upper bound of the wait after the given number of failed attempts:
// base, 2*base, 4*base, ... and never more than max.
func (b backoff) ceiling(failedAttempts int) time.Duration {
	c := b.base
	// Doubling in a loop that stops at max cannot overflow, unlike base << (n-1).
	for i := 1; i < failedAttempts && c < b.max; i++ {
		c *= 2
	}
	return min(c, b.max)
}

// delay returns the wait after failedAttempts attempts. A provider's Retry-After is a
// lower bound, because sending earlier would only be refused again; max still caps it
// so a misbehaving provider cannot park a message for hours (A-30).
func (b backoff) delay(failedAttempts int, retryAfter time.Duration) time.Duration {
	d := time.Duration(b.randN(int64(b.ceiling(failedAttempts)) + 1))
	return min(max(d, retryAfter), b.max)
}

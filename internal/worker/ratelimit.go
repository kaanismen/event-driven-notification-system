package worker

import (
	"context"
	"sync"
	"time"
)

// windowMargin widens the one-second window by one send interval (10 ms at 100/s). The
// cap is exact for the moments at which we start sends; between that moment and the
// provider receiving the request, goroutine scheduling and the network add jitter that
// can squeeze two sends at the window edges together. Measured at the mock provider: about
// 0.5 ms on an idle machine, and more than 2 ms with the CPU saturated (2 ms let 101 through,
// 5 ms and 10 ms held). The cost is about 1% of throughput: 100 sends per 1.01 s (ADR-0003).
const windowMargin = 10 * time.Millisecond

// windowCap allows at most n events in any time window of the given length, measured on
// the actual event times. The token bucket in front of it paces sends 10 ms apart, but
// it schedules them exactly one second apart per 100, so timer jitter alone can put 101
// sends into one second; this cap makes "at most 100 per second" hold for real.
//
// It keeps the times of the last n events in a ring buffer. A new event may happen only
// when the event n places back is at least one window old, so any n+1 consecutive events
// span at least the window and no window can contain more than n.
type windowCap struct {
	window time.Duration

	mu    sync.Mutex
	times []time.Time // ring buffer; zero values mean "fewer than n events so far"
	next  int         // index of the oldest entry, which the next event replaces
}

func newWindowCap(n int, window time.Duration) *windowCap {
	return &windowCap{window: window, times: make([]time.Time, n)}
}

// Wait blocks until one more event fits in the window, or ctx is done.
func (c *windowCap) Wait(ctx context.Context) error {
	_, err := c.wait(ctx)
	return err
}

// wait is Wait that also returns the recorded event time, so tests can check the
// guarantee on exactly the times the cap used.
func (c *windowCap) wait(ctx context.Context) (time.Time, error) {
	for {
		c.mu.Lock()
		now := time.Now()
		oldest := c.times[c.next]
		if oldest.IsZero() || now.Sub(oldest) >= c.window {
			c.times[c.next] = now
			c.next = (c.next + 1) % len(c.times)
			c.mu.Unlock()
			return now, nil
		}
		delay := c.window - now.Sub(oldest)
		c.mu.Unlock()

		// Another waiter may take the slot first; the loop then simply waits again.
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return time.Time{}, ctx.Err()
		case <-t.C:
		}
	}
}

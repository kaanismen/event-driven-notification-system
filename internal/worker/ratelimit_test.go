package worker

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestWindowCapIsExact hammers the cap from many goroutines and checks the guarantee on
// the very timestamps it recorded: no window may ever hold more than n events.
func TestWindowCapIsExact(t *testing.T) {
	const (
		n      = 10
		window = 100 * time.Millisecond
		events = 60
	)
	c := newWindowCap(n, window)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		times []time.Time
	)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range events / 20 {
				at, err := c.wait(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				times = append(times, at)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if peak := maxPerWindow(times, window); peak > n {
		t.Errorf("%d events within one window, cap is %d", peak, n)
	}
	// 60 events at 10 per 100 ms need at least 5 full windows after the first burst.
	if span := times[len(times)-1].Sub(times[0]); span < 5*window {
		t.Errorf("60 events took only %v", span)
	}
}

func TestWindowCapHonoursCancel(t *testing.T) {
	c := newWindowCap(1, time.Hour)
	if err := c.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Wait(ctx); err == nil {
		t.Fatal("second event fit into a one-hour window of size 1")
	}
	if time.Since(start) > time.Second {
		t.Error("Wait ignored the cancelled context")
	}
}

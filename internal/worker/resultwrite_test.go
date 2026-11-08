package worker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/config"
	"github.com/kaanismen/event-driven-notification-system/internal/logging"
	"github.com/kaanismen/event-driven-notification-system/internal/metrics"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/provider/mock"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

// flakyResultStore is the real store whose first failSent MarkSent calls fail, as if the
// database became unreachable just after the provider accepted the message. Claims,
// leases and reclaims are the real SQL, so a row left in processing really is sent again.
type flakyResultStore struct {
	*store.Store
	failSent  atomic.Int32
	sentCalls atomic.Int32
}

func (s *flakyResultStore) MarkSent(ctx context.Context, id uuid.UUID, attempt int, msgID string) (bool, time.Duration, error) {
	s.sentCalls.Add(1)
	if s.failSent.Add(-1) >= 0 {
		return false, 0, errors.New("dial tcp: connection refused")
	}
	return s.Store.MarkSent(ctx, id, attempt, msgID)
}

func TestResultWriteRetriesUntilTheDatabaseIsBack(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 1, 8)
	flaky := &flakyResultStore{Store: st}
	flaky.failSent.Store(3)

	cfg := testConfig()
	// Short, so that without retries the unrecorded row is reclaimed and sent again within
	// the test, yet longer than the safety margin, so the retries have a window.
	cfg.Lease = 3 * time.Second
	m := mock.NewRecorder(mock.Options{})
	met := metrics.New()
	startEngineWith(t, flaky, m, cfg, time.Second, func(e *Engine) { e.metrics = met })

	final := waitFinal(t, st, ids, 20*time.Second)

	n := final[ids[0]]
	if n.Status != notification.StatusSent || n.Attempts != 1 {
		t.Errorf("row ended %s after %d attempts, want sent after 1 (never reclaimed)", n.Status, n.Attempts)
	}
	if got := len(m.Requests()); got != 1 {
		t.Errorf("provider received %d sends, want 1", got)
	}
	if got := flaky.sentCalls.Load(); got != 4 {
		t.Errorf("MarkSent called %d times, want 4 (3 failures, then success)", got)
	}
	if got := scrape(t, met)[`notifier_result_write_retries_total{channel="sms",outcome="recovered"}`]; got != "3" {
		t.Errorf("recovered result-write retries = %q, want 3", got)
	}
}

// writeJob is a claimed row whose lease ends after lease.
func writeJob(lease time.Duration) job {
	return job{n: rowsFor(notification.ChannelSMS, 1)[0], leaseEnd: time.Now().Add(lease)}
}

func TestResultWriteGivesUpBeforeTheLeaseEnds(t *testing.T) {
	var logs safeBuffer
	m := metrics.New()
	e := New(nil, nil, logging.New(&logs, slog.LevelDebug), m, testConfig(), noTracer)
	j := writeJob(1500 * time.Millisecond)
	giveUp := j.leaseEnd.Add(-config.LeaseSafetyMargin)
	ctx := logging.WithRequestID(context.Background(), "req-write")

	calls := 0
	_, err := e.writeResult(ctx, e.logger, j, "mark sent", func(ctx context.Context) (bool, error) {
		calls++
		// Every retry must be over before the margin starts, so it cannot race a reclaim.
		if d, ok := ctx.Deadline(); calls > 1 && (!ok || d.After(giveUp)) {
			t.Errorf("retry %d may run until %v, after the give-up time %v", calls-1, d, giveUp)
		}
		return false, errors.New("dial tcp: connection refused")
	})
	if returned := time.Now(); returned.After(j.leaseEnd) {
		t.Errorf("gave up %v after the lease ended", returned.Sub(j.leaseEnd))
	}
	if err == nil || !strings.Contains(err.Error(), "lease is about to end") {
		t.Errorf("err = %v, want a give-up because the lease is about to end", err)
	}
	if calls < 2 {
		t.Fatalf("write attempted %d times, want retries", calls)
	}
	if got := scrape(t, m)[`notifier_result_write_retries_total{channel="sms",outcome="gave_up"}`]; got != strconv.Itoa(calls-1) {
		t.Errorf("gave_up retries = %q, want %d", got, calls-1)
	}
	if !strings.Contains(logs.String(), `"msg":"result write failed, retrying"`) || !strings.Contains(logs.String(), `"request_id":"req-write"`) {
		t.Errorf("want debug retry lines carrying the request ID, got:\n%s", logs.String())
	}
}

func TestResultWriteIsNotRetriedWhenTheRowIsNotOwned(t *testing.T) {
	e := New(nil, nil, slog.New(slog.DiscardHandler), metrics.New(), testConfig(), noTracer)
	calls := 0
	owned, err := e.writeResult(context.Background(), e.logger, writeJob(30*time.Second), "mark sent", func(context.Context) (bool, error) {
		calls++
		return false, nil // the ownership guard did not match: another worker has the row
	})
	if owned || err != nil || calls != 1 {
		t.Errorf("owned=%t err=%v after %d calls, want not owned, no error, exactly 1 call", owned, err, calls)
	}
}

func TestResultWriteStopsRetryingAtShutdown(t *testing.T) {
	e := New(nil, nil, slog.New(slog.DiscardHandler), metrics.New(), testConfig(), noTracer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := e.writeResult(ctx, e.logger, writeJob(30*time.Second), "mark sent", func(ctx context.Context) (bool, error) {
		calls++
		if ctx.Err() != nil {
			t.Error("the write attempt inherited the shutdown cancellation")
		}
		return false, errors.New("dial tcp: connection refused")
	})
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("calls=%d err=%v, want one attempt and a give-up because of shutdown", calls, err)
	}
}

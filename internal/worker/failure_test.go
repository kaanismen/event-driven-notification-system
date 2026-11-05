package worker

// Failure-injection tests (Phase 4 gate): the real engine, the real store on PostgreSQL
// and the mock provider with injected failures. Backoff is shortened to milliseconds so
// the tests run fast; the logic is the same as with the production defaults.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/config"
	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/provider"
	"github.com/kaanismen/insider-assessment/internal/provider/mock"
	"github.com/kaanismen/insider-assessment/internal/store"
	"github.com/kaanismen/insider-assessment/internal/testdb"
)

func newRealStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := testdb.New(t)
	if err := store.Migrate(dbURL, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := store.Open(context.Background(), dbURL, 20)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// enqueue creates n SMS notifications with the given retry budget.
func enqueue(t *testing.T, st *store.Store, n, maxAttempts int) []uuid.UUID {
	t.Helper()
	reqs := make([]notification.Request, n)
	for i := range reqs {
		reqs[i] = notification.Request{
			Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "code 1234",
			Priority: notification.PriorityNormal,
		}
	}
	res, err := st.Create(context.Background(), store.CreateParams{Requests: reqs, Batch: true, MaxAttempts: maxAttempts})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ids := make([]uuid.UUID, n)
	for i, item := range res.Notifications {
		ids[i] = item.ID
	}
	return ids
}

// startEngine runs an engine against a mock provider and returns the mock and a stop
// function that cancels the engine and waits for it to return.
func startEngine(t *testing.T, st *store.Store, opts mock.Options, cfg config.Worker, providerTimeout time.Duration) (*mock.Server, func()) {
	t.Helper()
	m := mock.NewRecorder(opts)
	return m, startEngineWith(t, st, m, cfg, providerTimeout)
}

// startEngineWith is startEngine with any provider handler.
func startEngineWith(t *testing.T, st *store.Store, h http.Handler, cfg config.Worker, providerTimeout time.Duration) func() {
	t.Helper()
	srv := httptest.NewServer(h)
	e := New(st, provider.NewClient(srv.URL, providerTimeout), slog.New(slog.DiscardHandler), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(stopped)
	}()
	var done bool
	stop := func() {
		if done {
			return
		}
		done = true
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10 s")
		}
		srv.Close()
	}
	t.Cleanup(stop)
	return stop
}

// waitFinal polls until every notification is sent or failed and returns them.
func waitFinal(t *testing.T, st *store.Store, ids []uuid.UUID, timeout time.Duration) map[uuid.UUID]notification.Notification {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out := map[uuid.UUID]notification.Notification{}
		for _, id := range ids {
			n, err := st.Get(context.Background(), id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if n.Status == notification.StatusSent || n.Status == notification.StatusFailed {
				out[id] = n
			}
		}
		if len(out) == len(ids) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d notifications final after %v", len(out), len(ids), timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func requestsByKey(m *mock.Server) map[string][]mock.Request {
	out := map[string][]mock.Request{}
	for _, r := range m.Requests() {
		out[r.IdempotencyKey] = append(out[r.IdempotencyKey], r)
	}
	return out
}

func fastRetries() config.Worker {
	cfg := testConfig()
	cfg.RetryBaseDelay, cfg.RetryMaxDelay = time.Millisecond, 10*time.Millisecond
	return cfg
}

// TestFlakyProviderEventuallyDelivers: every message fails twice, then succeeds. All
// must end up sent on the third attempt, and every attempt must carry the same
// Idempotency-Key, so a provider that dedupes could drop the repeats (DR-03, PE-05).
func TestFlakyProviderEventuallyDelivers(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 30, 8)
	m, _ := startEngine(t, st, mock.Options{FailFirst: 2}, fastRetries(), time.Second)

	final := waitFinal(t, st, ids, 10*time.Second)
	byKey := requestsByKey(m)
	for _, id := range ids {
		n := final[id]
		if n.Status != notification.StatusSent || n.Attempts != 3 || n.LastError != "" || n.ProviderMessageID == "" {
			t.Errorf("%s: status %s, attempts %d, last error %q, message id %q; want sent on attempt 3",
				id, n.Status, n.Attempts, n.LastError, n.ProviderMessageID)
		}
		if got := byKey[id.String()]; len(got) != 3 {
			t.Errorf("%s: provider got %d requests with its Idempotency-Key, want 3", id, len(got))
		}
	}
	if len(byKey) != len(ids) {
		t.Errorf("provider saw %d distinct Idempotency-Keys, want %d (one per notification)", len(byKey), len(ids))
	}
}

// TestProviderDownDeadLetters: a provider that always answers 503 uses up the retry
// budget, and the message ends in the dead-letter state with the last error (DR-04).
func TestProviderDownDeadLetters(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 5, 4)
	m, _ := startEngine(t, st, mock.Options{FailureRate: 1}, fastRetries(), time.Second)

	final := waitFinal(t, st, ids, 10*time.Second)
	byKey := requestsByKey(m)
	for _, id := range ids {
		n := final[id]
		if n.Status != notification.StatusFailed || n.Attempts != 4 || !strings.Contains(n.LastError, "HTTP 503") {
			t.Errorf("%s: status %s, attempts %d, last error %q; want failed after 4 attempts with the 503",
				id, n.Status, n.Attempts, n.LastError)
		}
		if got := len(byKey[id.String()]); got != 4 {
			t.Errorf("%s: provider got %d requests, want exactly max_attempts (4)", id, got)
		}
	}
}

// TestPermanentErrorIsNotRetried: a 400 means the request itself is wrong (DR-02).
func TestPermanentErrorIsNotRetried(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 3, 8)
	m, _ := startEngine(t, st, mock.Options{FailureRate: 1, FailureStatus: 400}, fastRetries(), time.Second)

	final := waitFinal(t, st, ids, 5*time.Second)
	for _, id := range ids {
		if n := final[id]; n.Status != notification.StatusFailed || n.Attempts != 1 || !strings.Contains(n.LastError, "HTTP 400") {
			t.Errorf("%s: status %s, attempts %d, last error %q; want failed after one attempt", id, n.Status, n.Attempts, n.LastError)
		}
	}
	if got := len(m.Requests()); got != len(ids) {
		t.Errorf("provider got %d requests, want %d (no retries)", got, len(ids))
	}
}

// TestRetryAfterIsRespected: a 429 with Retry-After: 1 must not be retried sooner,
// even though the backoff alone would retry within milliseconds (A-30).
func TestRetryAfterIsRespected(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 3, 8)
	cfg := fastRetries()
	cfg.RetryMaxDelay = 5 * time.Second // the cap must allow the requested wait
	m, _ := startEngine(t, st, mock.Options{FailFirst: 1, FailureStatus: 429, RetryAfter: time.Second}, cfg, time.Second)

	final := waitFinal(t, st, ids, 10*time.Second)
	byKey := requestsByKey(m)
	for _, id := range ids {
		if final[id].Status != notification.StatusSent {
			t.Errorf("%s: status %s, want sent", id, final[id].Status)
		}
		got := byKey[id.String()]
		if len(got) != 2 {
			t.Fatalf("%s: %d requests, want 2", id, len(got))
		}
		if gap := got[1].At.Sub(got[0].At); gap < time.Second {
			t.Errorf("%s: retried after %v, before the provider's Retry-After of 1s", id, gap)
		}
	}
}

// TestSlowProviderTimesOutAndRetries: a provider slower than our timeout must not hold
// a worker; each attempt times out, is retried, and the message is dead-lettered with
// the timeout as its last error (DR-07).
func TestSlowProviderTimesOutAndRetries(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 2, 3)
	start := time.Now()
	m, _ := startEngine(t, st, mock.Options{Latency: 5 * time.Second}, fastRetries(), 100*time.Millisecond)

	final := waitFinal(t, st, ids, 5*time.Second)
	for _, id := range ids {
		if n := final[id]; n.Status != notification.StatusFailed || n.Attempts != 3 || !strings.Contains(n.LastError, "deadline exceeded") {
			t.Errorf("%s: status %s, attempts %d, last error %q; want failed after 3 timeouts", id, n.Status, n.Attempts, n.LastError)
		}
	}
	// Three 100 ms timeouts, not three 5 s waits.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v; the timeout did not bound the slow calls", elapsed)
	}
	if got := len(m.Requests()); got != 6 {
		t.Errorf("provider got %d requests, want 6", got)
	}
}

// TestCrashedWorkerIsRecovered: worker A claims rows and dies without sending. Once the
// lease expires, worker B claims and delivers them, and the provider sees each message
// exactly once (DR-05).
func TestCrashedWorkerIsRecovered(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 5, 8)

	// Worker A: claims with a short lease, then "crashes" (never sends or records).
	claimed, err := st.Claim(context.Background(), store.ClaimParams{
		Channel: notification.ChannelSMS, Limit: 10, Weights: [3]int{70, 20, 10}, Lease: 300 * time.Millisecond,
	})
	if err != nil || len(claimed) != len(ids) {
		t.Fatalf("worker A claimed %d rows (%v), want %d", len(claimed), err, len(ids))
	}

	start := time.Now()
	m, _ := startEngine(t, st, mock.Options{}, fastRetries(), time.Second) // worker B
	final := waitFinal(t, st, ids, 5*time.Second)

	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("rows delivered after %v, before worker A's lease expired", elapsed)
	}
	byKey := requestsByKey(m)
	for _, id := range ids {
		if n := final[id]; n.Status != notification.StatusSent || n.Attempts != 2 {
			t.Errorf("%s: status %s, attempts %d; want sent on attempt 2 (the crash used one)", id, n.Status, n.Attempts)
		}
		if got := len(byKey[id.String()]); got != 1 {
			t.Errorf("%s: provider got %d requests, want 1", id, got)
		}
	}
}

// TestRetriesRespectTheRateLimit: every message fails once, so the provider receives
// twice as many requests as there are messages. Retries pass the same limiter as first
// attempts, so no one-second window may exceed 100 requests (DR-06, PE-02).
func TestRetriesRespectTheRateLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 3 seconds")
	}
	st := newRealStore(t)
	ids := enqueue(t, st, 150, 8)
	m, _ := startEngine(t, st, mock.Options{FailFirst: 1}, fastRetries(), time.Second)

	final := waitFinal(t, st, ids, 15*time.Second)
	for _, id := range ids {
		if final[id].Status != notification.StatusSent {
			t.Fatalf("%s: status %s, want sent", id, final[id].Status)
		}
	}
	var times []time.Time
	for _, r := range m.Requests() {
		times = append(times, r.At)
	}
	if len(times) != 300 {
		t.Errorf("provider got %d requests, want 300 (150 failures, 150 successes)", len(times))
	}
	peak := maxPerWindow(times, time.Second)
	t.Logf("peak %d requests in any 1 s window, retries included", peak)
	if peak > 100 {
		t.Errorf("%d requests within one second, limit is 100", peak)
	}
}

// TestShutdownReleasesUnsentRows: with a slow rate, most claimed rows are still waiting
// for a rate slot when shutdown starts. They must go back to pending at once with their
// attempt given back; no row may stay in processing (DR-08, ADR-0014).
func TestShutdownReleasesUnsentRows(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 20, 8)
	cfg := fastRetries()
	cfg.RatePerSecond, cfg.SendersPerChannel = 2, 2 // one send now, the next in 500 ms
	_, stop := startEngine(t, st, mock.Options{}, cfg, time.Second)

	time.Sleep(250 * time.Millisecond)
	stop()

	counts := map[notification.Status]int{}
	for _, id := range ids {
		n, err := st.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		counts[n.Status]++
		if n.Status == notification.StatusPending && n.Attempts != 0 {
			t.Errorf("%s: released with attempts %d, want 0", id, n.Attempts)
		}
	}
	t.Logf("after shutdown: %v", counts)
	if counts[notification.StatusProcessing] != 0 {
		t.Errorf("%d rows left in processing after shutdown", counts[notification.StatusProcessing])
	}
	if counts[notification.StatusSent] < 1 || counts[notification.StatusSent]+counts[notification.StatusPending] != len(ids) {
		t.Errorf("status counts %v; want at least one sent and the rest pending", counts)
	}
}

// TestBreakerSavesAttemptsDuringAnOutage: the provider is down for 1.2 s, then recovers.
// Without a breaker every waiting message would burn through its attempts (see
// TestProviderDownDeadLetters). With it, the channel stops after a few failures, sends one
// probe per cool-down, and resumes once a probe succeeds: no message is dead-lettered and
// the provider sees only a handful of calls during the outage (ADR-0015).
func TestBreakerSavesAttemptsDuringAnOutage(t *testing.T) {
	st := newRealStore(t)
	ids := enqueue(t, st, 40, 8)
	cfg := fastRetries()
	cfg.BreakerThreshold, cfg.BreakerCooldown = 5, 200*time.Millisecond

	var down atomic.Bool
	var callsWhileDown atomic.Int64
	down.Store(true)
	m := mock.NewRecorder(mock.Options{})
	outage := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			callsWhileDown.Add(1)
			http.Error(w, "maintenance", http.StatusServiceUnavailable)
			return
		}
		m.ServeHTTP(w, r)
	})
	startEngineWith(t, st, outage, cfg, time.Second)

	time.Sleep(1200 * time.Millisecond)
	down.Store(false)
	outageCalls := callsWhileDown.Load()

	final := waitFinal(t, st, ids, 10*time.Second)
	maxAttempts := 0
	for _, id := range ids {
		n := final[id]
		if n.Status != notification.StatusSent {
			t.Errorf("%s: status %s after %d attempts, want sent", id, n.Status, n.Attempts)
		}
		maxAttempts = max(maxAttempts, n.Attempts)
	}
	t.Logf("1.2 s outage: %d provider calls, at most %d attempts used by any message", outageCalls, maxAttempts)

	// 5 to open, then one probe per 200 ms cool-down (about 6), plus a few sends that
	// were already under way when the breaker opened. Without the breaker the rate limit
	// alone would allow about 120 calls in 1.2 s.
	if outageCalls > 25 {
		t.Errorf("%d provider calls during the outage; the breaker did not stop the channel", outageCalls)
	}
	if maxAttempts > 3 {
		t.Errorf("a message used %d attempts; the outage should cost at most a couple", maxAttempts)
	}
	if got := len(m.Requests()); got != len(ids) {
		t.Errorf("provider accepted %d messages after recovery, want %d (each exactly once)", got, len(ids))
	}
}

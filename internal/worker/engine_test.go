package worker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/config"
	"github.com/kaanismen/insider-assessment/internal/logging"
	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/provider"
	"github.com/kaanismen/insider-assessment/internal/provider/mock"
	"github.com/kaanismen/insider-assessment/internal/store"
)

// fakeStore is an in-memory queue. Priority order and SQL details are tested in the
// store package; here we only need rows to come out and results to be recorded.
type fakeStore struct {
	mu      sync.Mutex
	pending map[notification.Channel][]notification.Notification
	sent    map[uuid.UUID]string // id -> provider message id
	failed  map[uuid.UUID]string // id -> reason
	done    chan struct{}        // closed when every row has a result
	total   int
}

func newFakeStore(rows []notification.Notification) *fakeStore {
	f := &fakeStore{
		pending: map[notification.Channel][]notification.Notification{},
		sent:    map[uuid.UUID]string{},
		failed:  map[uuid.UUID]string{},
		done:    make(chan struct{}),
		total:   len(rows),
	}
	for _, n := range rows {
		f.pending[n.Channel] = append(f.pending[n.Channel], n)
	}
	return f
}

func (f *fakeStore) Claim(_ context.Context, p store.ClaimParams) ([]notification.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.pending[p.Channel]
	n := min(p.Limit, len(q))
	batch := append([]notification.Notification(nil), q[:n]...)
	f.pending[p.Channel] = q[n:]
	for i := range batch {
		batch[i].Status = notification.StatusProcessing
		batch[i].Attempts++
	}
	return batch, nil
}

func (f *fakeStore) MarkSent(_ context.Context, id uuid.UUID, _ int, msgID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[id] = msgID
	f.checkDone()
	return true, nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id uuid.UUID, _ int, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = reason
	f.checkDone()
	return true, nil
}

func (f *fakeStore) checkDone() {
	if len(f.sent)+len(f.failed) == f.total {
		close(f.done)
	}
}

func rowsFor(c notification.Channel, n int) []notification.Notification {
	rows := make([]notification.Notification, n)
	for i := range rows {
		rows[i] = notification.Notification{
			ID: uuid.Must(uuid.NewV7()), Channel: c, Recipient: "r", Content: fmt.Sprintf("m%d", i),
			Priority: notification.PriorityNormal, Status: notification.StatusPending,
			CorrelationID: fmt.Sprintf("req-%s-%d", c, i),
		}
	}
	return rows
}

func testConfig() config.Worker {
	return config.Worker{
		RatePerSecond: 100, SendersPerChannel: 50, ClaimBatchSize: 20,
		PollInterval: 20 * time.Millisecond, Lease: 30 * time.Second, PriorityWeights: [3]int{70, 20, 10},
	}
}

// runUntilDone starts the engine and stops it once every row has a result.
func runUntilDone(t *testing.T, e *Engine, f *fakeStore, timeout time.Duration) time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	start := time.Now()
	go func() {
		e.Run(ctx)
		close(stopped)
	}()

	select {
	case <-f.done:
	case <-time.After(timeout):
		cancel()
		t.Fatalf("not every notification got a result within %v", timeout)
	}
	elapsed := time.Since(start)
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after cancel")
	}
	return elapsed
}

// maxPerWindow returns the largest number of timestamps inside any half-open window of
// the given width.
func maxPerWindow(times []time.Time, width time.Duration) int {
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	best, lo := 0, 0
	for hi := range times {
		for times[hi].Sub(times[lo]) >= width {
			lo++
		}
		best = max(best, hi-lo+1)
	}
	return best
}

// TestRateLimitPerChannel sends 250 messages on each of the three channels at once. Each
// channel must stay at or under 100 per second, and the channels must not slow each
// other down: together they should finish in about 2.5 s, not 7.5 s.
func TestRateLimitPerChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 3 seconds")
	}
	const perChannel = 250
	var rows []notification.Notification
	for _, c := range []notification.Channel{notification.ChannelSMS, notification.ChannelEmail, notification.ChannelPush} {
		rows = append(rows, rowsFor(c, perChannel)...)
	}
	f := newFakeStore(rows)
	m := mock.NewRecorder()
	srv := httptest.NewServer(m)
	defer srv.Close()

	e := New(f, provider.NewClient(srv.URL, 5*time.Second), slog.New(slog.DiscardHandler), testConfig())
	elapsed := runUntilDone(t, e, f, 15*time.Second)

	byChannel := map[string][]time.Time{}
	for _, r := range m.Requests() {
		byChannel[r.Channel] = append(byChannel[r.Channel], r.At)
	}
	for c, times := range byChannel {
		if len(times) != perChannel {
			t.Errorf("%s: provider got %d requests, want %d", c, len(times), perChannel)
		}
		// Measured where it matters: when requests arrive at the provider.
		peak := maxPerWindow(times, time.Second)
		t.Logf("%s: peak %d requests in any 1 s window", c, peak)
		if peak > 100 {
			t.Errorf("%s: %d requests within one second, limit is 100", c, peak)
		}
		if span := times[len(times)-1].Sub(times[0]); span < 2300*time.Millisecond {
			t.Errorf("%s: %d requests took only %v; the limit was not applied", c, perChannel, span)
		}
	}
	if elapsed > 4*time.Second {
		t.Errorf("three channels took %v; they should run in parallel (about 2.5 s)", elapsed)
	}
	if len(f.failed) != 0 {
		t.Errorf("%d deliveries failed", len(f.failed))
	}
}

func TestDeliveryRecordsResultsAndCarriesTheRequestID(t *testing.T) {
	rows := rowsFor(notification.ChannelSMS, 4)
	rows[1].Content, rows[3].Content = "reject", "reject"

	m := mock.NewRecorder()
	// Reject messages whose content is "reject"; hand everything else to the mock.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"content":"reject"`) {
			http.Error(w, `{"error":"bad recipient"}`, http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw)) // the mock reads the body again
		m.ServeHTTP(w, r)
	}))
	defer srv.Close()

	var logs safeBuffer
	f := newFakeStore(rows)
	e := New(f, provider.NewClient(srv.URL, 5*time.Second), logging.New(&logs, slog.LevelInfo), testConfig())
	runUntilDone(t, e, f, 5*time.Second)

	if len(f.sent) != 2 || len(f.failed) != 2 {
		t.Fatalf("sent %d, failed %d; want 2 and 2", len(f.sent), len(f.failed))
	}
	byKey := map[string]mock.Request{}
	for _, r := range m.Requests() {
		byKey[r.IdempotencyKey] = r
	}
	for _, n := range []notification.Notification{rows[0], rows[2]} {
		got, ok := byKey[n.ID.String()]
		if !ok {
			t.Fatalf("provider never received %s with its ID as Idempotency-Key", n.ID)
		}
		if f.sent[n.ID] != got.MessageID || got.RequestID != n.CorrelationID {
			t.Errorf("%s: stored messageId %q (provider said %q), X-Request-ID %q (want %q)",
				n.ID, f.sent[n.ID], got.MessageID, got.RequestID, n.CorrelationID)
		}
	}
	if reason := f.failed[rows[1].ID]; !strings.Contains(reason, "HTTP 400") {
		t.Errorf("failure reason = %q, want the HTTP status", reason)
	}

	// OBS-05: the worker's log line for a notification carries the API request ID.
	if want := `"request_id":"` + rows[0].CorrelationID + `"`; !strings.Contains(logs.String(), want) {
		t.Errorf("worker logs do not contain %s:\n%s", want, logs.String())
	}
}

// TestShutdownFinishesInFlightSends cancels the engine while a send is in progress. The
// send must complete and its result must be recorded; abandoning it would leave a
// message the provider accepted marked as processing, which later becomes a duplicate.
func TestShutdownFinishesInFlightSends(t *testing.T) {
	received := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(received)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"messageId":"slow-1"}`))
	}))
	defer srv.Close()

	rows := rowsFor(notification.ChannelEmail, 1)
	f := newFakeStore(rows)
	e := New(f, provider.NewClient(srv.URL, 5*time.Second), slog.New(slog.DiscardHandler), testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(stopped)
	}()

	<-received
	cancel() // shutdown starts while the provider is still answering

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent[rows[0].ID] != "slow-1" {
		t.Errorf("in-flight send was not recorded after shutdown: sent=%v failed=%v", f.sent, f.failed)
	}
}

func TestIdleEngineStopsPromptly(t *testing.T) {
	f := newFakeStore(nil)
	e := New(f, provider.NewClient("http://127.0.0.1:1", time.Second), slog.New(slog.DiscardHandler), testConfig())

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(stopped)
	}()
	time.Sleep(100 * time.Millisecond) // let it poll a few times
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("idle engine did not stop within a second")
	}
}

// safeBuffer is a bytes.Buffer safe for concurrent writers, for capturing logs.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

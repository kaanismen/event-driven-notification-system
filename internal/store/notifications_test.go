package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/testdb"
)

// newTestStore returns a Store on a fresh, migrated database.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbURL := testdb.New(t)
	if err := Migrate(dbURL, discardLogger()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := Open(context.Background(), dbURL, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}

func smsRequest(i int) notification.Request {
	return notification.Request{
		Channel:   notification.ChannelSMS,
		Recipient: fmt.Sprintf("+90555%07d", i),
		Content:   fmt.Sprintf("message %d", i),
		Priority:  notification.PriorityNormal,
	}
}

func single(req notification.Request) CreateParams {
	return CreateParams{Requests: []notification.Request{req}, MaxAttempts: 8, CorrelationID: "req-1"}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestCreateSingle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	req := smsRequest(1)
	req.Priority = notification.PriorityHigh
	res, err := s.Create(ctx, single(req))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Replayed || res.BatchID != nil || len(res.Notifications) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}

	n := res.Notifications[0]
	if n.Status != notification.StatusPending || n.Priority != notification.PriorityHigh ||
		n.Channel != notification.ChannelSMS || n.Recipient != req.Recipient || n.Content != req.Content ||
		n.MaxAttempts != 8 || n.Attempts != 0 || n.CorrelationID != "req-1" || n.BatchID != nil || n.SentAt != nil {
		t.Errorf("unexpected notification: %+v", n)
	}
	if n.ID.Version() != 7 {
		t.Errorf("ID version = %d, want 7", n.ID.Version())
	}
	if n.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt not in UTC: %v", n.CreatedAt)
	}

	got, err := s.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != n {
		t.Errorf("Get = %+v\nwant  %+v", got, n)
	}
}

func TestCreateBatchKeepsRequestOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	reqs := make([]notification.Request, 50)
	for i := range reqs {
		reqs[i] = smsRequest(i)
	}
	res, err := s.Create(ctx, CreateParams{Requests: reqs, Batch: true, MaxAttempts: 8})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.BatchID == nil || len(res.Notifications) != len(reqs) {
		t.Fatalf("unexpected result: batch=%v items=%d", res.BatchID, len(res.Notifications))
	}

	items, err := s.GetBatch(ctx, *res.BatchID)
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	for i := range reqs {
		if res.Notifications[i].Content != reqs[i].Content {
			t.Fatalf("create result item %d = %q, want %q", i, res.Notifications[i].Content, reqs[i].Content)
		}
		if items[i].Content != reqs[i].Content || *items[i].BatchID != *res.BatchID {
			t.Fatalf("GetBatch item %d = %q (batch %v), want %q", i, items[i].Content, items[i].BatchID, reqs[i].Content)
		}
	}
}

func TestCreateFullBatch(t *testing.T) {
	s := newTestStore(t)
	reqs := make([]notification.Request, 1000)
	for i := range reqs {
		reqs[i] = smsRequest(i)
	}
	start := time.Now()
	if _, err := s.Create(context.Background(), CreateParams{Requests: reqs, Batch: true, MaxAttempts: 8}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("inserted 1000 rows in %v", time.Since(start))
	if n := countRows(t, s, "notifications"); n != 1000 {
		t.Errorf("rows = %d, want 1000", n)
	}
}

func TestNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Get(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get unknown id: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetBatch(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBatch unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestIdempotentReplayReturnsCurrentState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := single(smsRequest(1))
	p.IdempotencyKey, p.RequestHash = "key-1", []byte("hash-a")

	first, err := s.Create(ctx, p)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	id := first.Notifications[0].ID

	// Simulate delivery between the two requests: the replay must show the new state.
	if _, err := s.pool.Exec(ctx, `UPDATE notifications SET status = 'sent', sent_at = now() WHERE id = $1`, id.String()); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	second, err := s.Create(ctx, p)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if !second.Replayed || second.Notifications[0].ID != id {
		t.Fatalf("second Create = %+v, want replay of %s", second, id)
	}
	if second.Notifications[0].Status != notification.StatusSent {
		t.Errorf("replayed status = %q, want current status sent", second.Notifications[0].Status)
	}
	if n := countRows(t, s, "notifications"); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestIdempotencyConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := single(smsRequest(1))
	p.IdempotencyKey, p.RequestHash = "key-1", []byte("hash-a")
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	different := single(smsRequest(2))
	different.IdempotencyKey, different.RequestHash = "key-1", []byte("hash-b")
	if _, err := s.Create(ctx, different); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("different body: err = %v, want ErrIdempotencyConflict", err)
	}

	// The same key on the batch endpoint is a different request, even with the same hash.
	asBatch := p
	asBatch.Batch = true
	if _, err := s.Create(ctx, asBatch); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("same key as batch: err = %v, want ErrIdempotencyConflict", err)
	}

	if n := countRows(t, s, "notifications"); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestIdempotentBatchReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	reqs := []notification.Request{smsRequest(1), smsRequest(2), smsRequest(3)}
	p := CreateParams{Requests: reqs, Batch: true, MaxAttempts: 8, IdempotencyKey: "batch-key", RequestHash: []byte("h")}

	first, err := s.Create(ctx, p)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := s.Create(ctx, p)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if !second.Replayed || *second.BatchID != *first.BatchID || len(second.Notifications) != 3 {
		t.Fatalf("second Create = %+v, want replay of batch %s", second, *first.BatchID)
	}
	for i := range reqs {
		if second.Notifications[i].ID != first.Notifications[i].ID {
			t.Errorf("item %d: id %s, want %s", i, second.Notifications[i].ID, first.Notifications[i].ID)
		}
	}
	if n := countRows(t, s, "notifications"); n != 3 {
		t.Errorf("rows = %d, want 3", n)
	}
}

// TestConcurrentDuplicateKeys fires the same keyed request from many goroutines at once.
// Exactly one may create the notification; all others must get it back as a replay.
func TestConcurrentDuplicateKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := single(smsRequest(1))
	p.IdempotencyKey, p.RequestHash = "race-key", []byte("h")

	const workers = 20
	var (
		wg      sync.WaitGroup
		results = make([]CreateResult, workers)
		errs    = make([]error, workers)
		start   = make(chan struct{})
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release every goroutine at the same moment
			results[i], errs[i] = s.Create(ctx, p)
		}()
	}
	close(start)
	wg.Wait()

	created := 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if !results[i].Replayed {
			created++
		}
		if results[i].Notifications[0].ID != results[0].Notifications[0].ID {
			t.Errorf("worker %d got id %s, worker 0 got %s", i, results[i].Notifications[0].ID, results[0].Notifications[0].ID)
		}
	}
	if created != 1 {
		t.Errorf("%d workers created a notification, want exactly 1", created)
	}
	if n := countRows(t, s, "notifications"); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

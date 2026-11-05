package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

var defaultWeights = [3]int{70, 20, 10}

func TestSplitQuota(t *testing.T) {
	tests := []struct {
		limit   int
		weights [3]int
		want    [3]int
	}{
		{20, defaultWeights, [3]int{14, 4, 2}},
		{10, defaultWeights, [3]int{7, 2, 1}},
		{3, [3]int{98, 1, 1}, [3]int{1, 1, 1}}, // nobody starves
		{100, [3]int{1, 1, 1}, [3]int{34, 33, 33}},
		{2, defaultWeights, [3]int{1, 1, 0}},
		{1, defaultWeights, [3]int{1, 0, 0}},
	}
	for _, tt := range tests {
		got := splitQuota(tt.limit, tt.weights)
		if got != tt.want {
			t.Errorf("splitQuota(%d, %v) = %v, want %v", tt.limit, tt.weights, got, tt.want)
		}
		if sum := got[0] + got[1] + got[2]; sum != tt.limit {
			t.Errorf("splitQuota(%d, %v) uses %d slots", tt.limit, tt.weights, sum)
		}
	}
}

// createN inserts n pending notifications with one channel and priority.
func createN(t *testing.T, s *Store, c notification.Channel, p notification.Priority, n int) []uuid.UUID {
	t.Helper()
	recipient := map[notification.Channel]string{
		notification.ChannelSMS: "+905551234567", notification.ChannelEmail: "a@example.com", notification.ChannelPush: "tok",
	}[c]
	reqs := make([]notification.Request, n)
	for i := range reqs {
		reqs[i] = notification.Request{Channel: c, Recipient: recipient, Content: "x", Priority: p}
	}
	res, err := s.Create(context.Background(), CreateParams{Requests: reqs, Batch: true, MaxAttempts: 8})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ids := make([]uuid.UUID, n)
	for i, item := range res.Notifications {
		ids[i] = item.ID
	}
	return ids
}

func claimSMS(t *testing.T, s *Store, limit int) []notification.Notification {
	t.Helper()
	got, err := s.Claim(context.Background(), ClaimParams{
		Channel: notification.ChannelSMS, Limit: limit, Weights: defaultWeights, Lease: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return got
}

func countByPriority(items []notification.Notification) map[notification.Priority]int {
	m := map[notification.Priority]int{}
	for _, n := range items {
		m[n.Priority]++
	}
	return m
}

func TestClaimWeightedShares(t *testing.T) {
	s := newTestStore(t)
	createN(t, s, notification.ChannelSMS, notification.PriorityLow, 100)
	createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 100)
	createN(t, s, notification.ChannelSMS, notification.PriorityHigh, 100)

	got := claimSMS(t, s, 20)

	counts := countByPriority(got)
	if counts[notification.PriorityHigh] != 14 || counts[notification.PriorityNormal] != 4 || counts[notification.PriorityLow] != 2 {
		t.Errorf("claimed %v, want high 14, normal 4, low 2", counts)
	}
	for i, n := range got {
		if n.Status != notification.StatusProcessing || n.Attempts != 1 {
			t.Errorf("claimed row %d: status %s, attempts %d", i, n.Status, n.Attempts)
		}
		if i > 0 && n.Priority.Rank() < got[i-1].Priority.Rank() {
			t.Errorf("result not sorted by priority at index %d", i)
		}
	}
	var leased int
	err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notifications
		WHERE status = 'processing' AND locked_until > now() + interval '25 seconds'`).Scan(&leased)
	if err != nil || leased != 20 {
		t.Errorf("rows with a 30s lease = %d (err %v), want 20", leased, err)
	}
}

func TestClaimGivesUnusedShareToOtherLevels(t *testing.T) {
	s := newTestStore(t)
	createN(t, s, notification.ChannelSMS, notification.PriorityHigh, 2)
	createN(t, s, notification.ChannelSMS, notification.PriorityLow, 50)

	got := claimSMS(t, s, 20)

	counts := countByPriority(got)
	if len(got) != 20 || counts[notification.PriorityHigh] != 2 || counts[notification.PriorityLow] != 18 {
		t.Errorf("claimed %d rows %v, want 2 high and 18 low", len(got), counts)
	}
}

func TestClaimOnlyTakesDueRowsOfItsChannel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	due := createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 1)[0]
	future := createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 1)[0]
	cancelled := createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 1)[0]
	createN(t, s, notification.ChannelEmail, notification.PriorityHigh, 5)

	if _, err := s.pool.Exec(ctx, `UPDATE notifications SET next_attempt_at = now() + interval '1 hour' WHERE id = $1`, future.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, cancelled); err != nil {
		t.Fatal(err)
	}

	got := claimSMS(t, s, 20)
	if len(got) != 1 || got[0].ID != due {
		t.Fatalf("claimed %d rows, want only the due SMS row", len(got))
	}
}

func TestClaimRecoversExpiredLease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := createN(t, s, notification.ChannelSMS, notification.PriorityLow, 1)[0]
	createN(t, s, notification.ChannelSMS, notification.PriorityHigh, 30)

	// A worker claimed the row and died: the lease ran out a second ago.
	if _, err := s.pool.Exec(ctx, `
		UPDATE notifications SET status = 'processing', attempts = 1, locked_until = now() - interval '1 second'
		WHERE id = $1`, id.String()); err != nil {
		t.Fatal(err)
	}

	got := claimSMS(t, s, 20)
	var recovered *notification.Notification
	for i := range got {
		if got[i].ID == id {
			recovered = &got[i]
		}
	}
	if recovered == nil {
		t.Fatal("expired lease not reclaimed although 30 high-priority rows were waiting")
	}
	if recovered.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", recovered.Attempts)
	}

	// A live lease must not be taken.
	if again := claimSMS(t, s, 20); len(again) != 11 { // 30 high - 19 already claimed
		t.Errorf("second claim took %d rows, want the 11 remaining high rows", len(again))
	}
}

func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	s := newTestStore(t)
	createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 200)

	const workers = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seen  = map[uuid.UUID]int{}
		start = make(chan struct{})
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				got, err := s.Claim(context.Background(), ClaimParams{
					Channel: notification.ChannelSMS, Limit: 7, Weights: defaultWeights, Lease: time.Minute,
				})
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, n := range got {
					seen[n.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(seen) != 200 {
		t.Errorf("claimed %d distinct rows, want 200", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s claimed %d times", id, n)
		}
	}
}

func TestMarkSentAndFailedAreFenced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 2)
	claimed := claimSMS(t, s, 2)
	a, b := claimed[0], claimed[1]

	if ok, err := s.MarkSent(ctx, a.ID, a.Attempts+1, "x"); err != nil || ok {
		t.Errorf("MarkSent with a stale attempt = %t, %v; want no update", ok, err)
	}
	if ok, err := s.MarkSent(ctx, a.ID, a.Attempts, "provider-1"); err != nil || !ok {
		t.Fatalf("MarkSent = %t, %v", ok, err)
	}
	if ok, _ := s.MarkSent(ctx, a.ID, a.Attempts, "provider-2"); ok {
		t.Error("MarkSent succeeded twice on the same row")
	}
	sent, _ := s.Get(ctx, a.ID)
	if sent.Status != notification.StatusSent || sent.ProviderMessageID != "provider-1" || sent.SentAt == nil {
		t.Errorf("after MarkSent: %+v", sent)
	}

	if ok, err := s.MarkFailed(ctx, b.ID, b.Attempts, "provider returned HTTP 400"); err != nil || !ok {
		t.Fatalf("MarkFailed = %t, %v", ok, err)
	}
	failed, _ := s.Get(ctx, b.ID)
	if failed.Status != notification.StatusFailed || failed.LastError != "provider returned HTTP 400" {
		t.Errorf("after MarkFailed: %+v", failed)
	}
	if !notification.CanTransition(notification.StatusProcessing, sent.Status) ||
		!notification.CanTransition(notification.StatusProcessing, failed.Status) {
		t.Error("store transitions disagree with notification.CanTransition")
	}
}

// TestCancelRacesRealClaim repeats the Phase 2 race test with the real claim query:
// for every row, exactly one of cancel and claim wins.
func TestCancelRacesRealClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i := range 40 {
		id := createN(t, s, notification.ChannelPush, notification.PriorityNormal, 1)[0]

		var (
			wg        sync.WaitGroup
			start     = make(chan struct{})
			cancelErr error
			claimed   []notification.Notification
			claimErr  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = s.Cancel(ctx, id)
		}()
		go func() {
			defer wg.Done()
			<-start
			claimed, claimErr = s.Claim(ctx, ClaimParams{
				Channel: notification.ChannelPush, Limit: 1, Weights: defaultWeights, Lease: time.Minute,
			})
		}()
		close(start)
		wg.Wait()
		if claimErr != nil {
			t.Fatalf("Claim: %v", claimErr)
		}

		final, _ := s.Get(ctx, id)
		var nc *NotCancellableError
		switch {
		case cancelErr == nil && len(claimed) == 0 && final.Status == notification.StatusCancelled:
		case errors.As(cancelErr, &nc) && len(claimed) == 1 && final.Status == notification.StatusProcessing:
			// The claimed row is done with, so the next iteration's claim cannot see it.
			if _, err := s.MarkSent(ctx, id, claimed[0].Attempts, ""); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("iteration %d: cancelErr=%v claimed=%d final=%s; want exactly one winner", i, cancelErr, len(claimed), final.Status)
		}
	}
}

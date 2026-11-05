package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

func TestMarkRetrySchedulesTheNextAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createN(t, s, notification.ChannelSMS, notification.PriorityHigh, 2)
	claimed := claimSMS(t, s, 2)
	later, now := claimed[0], claimed[1]

	if ok, err := s.MarkRetry(ctx, later.ID, later.Attempts+1, 0, "x"); err != nil || ok {
		t.Errorf("MarkRetry with a stale attempt = %t, %v; want no update", ok, err)
	}
	if ok, err := s.MarkRetry(ctx, later.ID, later.Attempts, time.Hour, "provider returned HTTP 503"); err != nil || !ok {
		t.Fatalf("MarkRetry = %t, %v", ok, err)
	}
	if ok, err := s.MarkRetry(ctx, now.ID, now.Attempts, 0, "send to provider: timeout"); err != nil || !ok {
		t.Fatalf("MarkRetry = %t, %v", ok, err)
	}

	got, _ := s.Get(ctx, later.ID)
	if got.Status != notification.StatusPending || got.Attempts != 1 || got.LastError != "provider returned HTTP 503" {
		t.Errorf("after MarkRetry: status %s, attempts %d, last error %q", got.Status, got.Attempts, got.LastError)
	}
	if wait := time.Until(got.NextAttemptAt); wait < 59*time.Minute || wait > 61*time.Minute {
		t.Errorf("next attempt in %v, want about 1h", wait)
	}
	if !notification.CanTransition(notification.StatusProcessing, notification.StatusPending) {
		t.Error("store retries disagree with notification.CanTransition")
	}

	// Only the row whose retry is due comes back, and its attempt counter keeps counting.
	again := claimSMS(t, s, 10)
	if len(again) != 1 || again[0].ID != now.ID || again[0].Attempts != 2 {
		t.Fatalf("claim after retry = %+v, want only %s at attempt 2", again, now.ID)
	}
}

func TestReleaseGivesTheAttemptBack(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 1)
	n := claimSMS(t, s, 1)[0]

	if ok, err := s.Release(ctx, n.ID, n.Attempts+1); err != nil || ok {
		t.Errorf("Release with a stale attempt = %t, %v; want no update", ok, err)
	}
	if ok, err := s.Release(ctx, n.ID, n.Attempts); err != nil || !ok {
		t.Fatalf("Release = %t, %v", ok, err)
	}
	if ok, _ := s.Release(ctx, n.ID, n.Attempts); ok {
		t.Error("Release succeeded twice on the same row")
	}

	got, _ := s.Get(ctx, n.ID)
	if got.Status != notification.StatusPending || got.Attempts != 0 || !got.NextAttemptAt.Equal(n.NextAttemptAt) {
		t.Errorf("after Release: status %s, attempts %d, due %v (was %v)", got.Status, got.Attempts, got.NextAttemptAt, n.NextAttemptAt)
	}
	if again := claimSMS(t, s, 1); len(again) != 1 || again[0].Attempts != 1 {
		t.Errorf("released row not claimable as a first attempt: %+v", again)
	}
}

// TestExpiredLeaseOnFinalAttemptFails covers a worker that dies during the last
// attempt: the row must reach failed instead of being claimed beyond max_attempts or
// staying in processing forever.
func TestExpiredLeaseOnFinalAttemptFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := createN(t, s, notification.ChannelSMS, notification.PriorityNormal, 2)
	final, early := ids[0], ids[1]

	expire := func(id uuid.UUID, attempts int) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, `
			UPDATE notifications SET status = 'processing', attempts = $2, locked_until = now() - interval '1 second'
			WHERE id = $1`, id.String(), attempts); err != nil {
			t.Fatal(err)
		}
	}
	expire(final, 8) // max_attempts is 8 in createN
	expire(early, 3)

	moved, err := s.FailExhaustedLeases(ctx, notification.ChannelSMS)
	if err != nil || moved != 1 {
		t.Fatalf("FailExhaustedLeases = %d, %v; want 1 row", moved, err)
	}
	got, _ := s.Get(ctx, final)
	if got.Status != notification.StatusFailed || got.Attempts != 8 || got.LastError != exhaustedLeaseError {
		t.Errorf("exhausted row: status %s, attempts %d, last error %q", got.Status, got.Attempts, got.LastError)
	}

	// The row with attempts left is reclaimed as usual, and nothing beyond the budget is.
	claimed := claimSMS(t, s, 10)
	if len(claimed) != 1 || claimed[0].ID != early || claimed[0].Attempts != 4 {
		t.Errorf("claim = %+v, want only %s at attempt 4", claimed, early)
	}

	// Other channels are not touched.
	if moved, err := s.FailExhaustedLeases(ctx, notification.ChannelEmail); err != nil || moved != 0 {
		t.Errorf("email: FailExhaustedLeases = %d, %v; want 0", moved, err)
	}
}

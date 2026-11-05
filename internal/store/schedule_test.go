package store

import (
	"context"
	"testing"
	"time"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

func createScheduled(t *testing.T, s *Store, at *time.Time) notification.Notification {
	t.Helper()
	req := notification.Request{
		Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "x",
		Priority: notification.PriorityNormal, ScheduledAt: at,
	}.Normalized()
	res, err := s.Create(context.Background(), CreateParams{Requests: []notification.Request{req}, MaxAttempts: 8})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return res.Notifications[0]
}

// A scheduled row is invisible to the claim until its time, then claimed like any other
// row; queue depth counts it as scheduled until then (ADR-0017).
func TestScheduledRowWaitsForItsTime(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	at := time.Now().Add(500 * time.Millisecond).UTC().Truncate(time.Microsecond)
	scheduled := createScheduled(t, s, &at)
	immediate := createScheduled(t, s, nil)

	if scheduled.ScheduledAt == nil || !scheduled.ScheduledAt.Equal(at) || !scheduled.NextAttemptAt.Equal(at) {
		t.Fatalf("stored scheduledAt %v, nextAttemptAt %v; want both %v", scheduled.ScheduledAt, scheduled.NextAttemptAt, at)
	}
	if immediate.ScheduledAt != nil {
		t.Errorf("unscheduled row has scheduledAt %v", immediate.ScheduledAt)
	}

	depth, err := s.QueueDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]int64{}
	for _, d := range depth {
		states[d.State] += d.Count
	}
	if states[StateScheduled] != 1 || states[StatePending] != 1 {
		t.Errorf("queue depth by state = %v, want 1 scheduled and 1 pending", states)
	}

	if got := claimSMS(t, s, 10); len(got) != 1 || got[0].ID != immediate.ID {
		t.Fatalf("claim before the scheduled time = %d rows, want only the immediate one", len(got))
	}

	time.Sleep(time.Until(at) + 50*time.Millisecond)
	got := claimSMS(t, s, 10)
	if len(got) != 1 || got[0].ID != scheduled.ID {
		t.Fatalf("claim after the scheduled time = %d rows, want the scheduled one", len(got))
	}
	if got[0].ScheduledAt == nil || !got[0].ScheduledAt.Equal(at) {
		t.Errorf("claimed row lost its scheduledAt: %v", got[0].ScheduledAt)
	}
}

// Cancelling a scheduled notification works like any pending one (A-14).
func TestCancelScheduled(t *testing.T) {
	s := newTestStore(t)
	at := time.Now().Add(time.Hour)
	n := createScheduled(t, s, &at)
	got, err := s.Cancel(context.Background(), n.ID)
	if err != nil || got.Status != notification.StatusCancelled {
		t.Fatalf("Cancel = %s, %v; want cancelled", got.Status, err)
	}
}

func TestListScheduledFilter(t *testing.T) {
	s := newTestStore(t)
	at := time.Now().Add(time.Hour)
	scheduled := createScheduled(t, s, &at)
	createScheduled(t, s, nil)
	createScheduled(t, s, nil)

	for want, count := range map[bool]int{true: 1, false: 2} {
		page, err := s.List(context.Background(), ListFilter{Scheduled: &want, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != count {
			t.Errorf("scheduled=%t: %d rows, want %d", want, len(page.Items), count)
		}
		if want && len(page.Items) == 1 && page.Items[0].ID != scheduled.ID {
			t.Errorf("scheduled=true returned %s", page.Items[0].ID)
		}
	}
}

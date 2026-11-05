package store

import (
	"context"
	"testing"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

func TestQueueDepthCountsOnlyOpenRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createN(t, s, notification.ChannelSMS, notification.PriorityHigh, 4)
	email := createN(t, s, notification.ChannelEmail, notification.PriorityLow, 3)

	claimed := claimSMS(t, s, 2) // two SMS rows become processing
	if ok, _, err := s.MarkSent(ctx, claimed[0].ID, claimed[0].Attempts, ""); err != nil || !ok {
		t.Fatalf("MarkSent = %t, %v", ok, err)
	}
	if _, err := s.Cancel(ctx, email[0]); err != nil {
		t.Fatal(err)
	}

	rows, err := s.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	got := map[QueueDepth]bool{}
	for _, r := range rows {
		got[r] = true
	}
	want := []QueueDepth{
		{notification.ChannelSMS, notification.PriorityHigh, StatePending, 2},
		{notification.ChannelSMS, notification.PriorityHigh, StateProcessing, 1},
		{notification.ChannelEmail, notification.PriorityLow, StatePending, 2},
	}
	if len(rows) != len(want) {
		t.Errorf("QueueDepth returned %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %+v in %+v", w, rows)
		}
	}
}

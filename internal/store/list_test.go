package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

// seed creates n notifications with channels in rotation, created one minute apart
// starting at base, and every fourth one marked sent (4 and 3 share no factor, so every
// channel has both sent and pending rows).
func seed(t *testing.T, s *Store, n int, base time.Time) []notification.Notification {
	t.Helper()
	ctx := context.Background()
	channels := []notification.Channel{notification.ChannelSMS, notification.ChannelEmail, notification.ChannelPush}
	recipients := map[notification.Channel]string{
		notification.ChannelSMS: "+905551234567", notification.ChannelEmail: "a@example.com", notification.ChannelPush: "tok",
	}

	out := make([]notification.Notification, 0, n)
	for i := range n {
		c := channels[i%len(channels)]
		res, err := s.Create(ctx, single(notification.Request{
			Channel: c, Recipient: recipients[c], Content: "x", Priority: notification.PriorityNormal,
		}))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		status := notification.StatusPending
		if i%4 == 0 {
			status = notification.StatusSent
		}
		created := base.Add(time.Duration(i) * time.Minute)
		_, err = s.pool.Exec(ctx, `UPDATE notifications SET created_at = $2, status = $3 WHERE id = $1`,
			res.Notifications[0].ID.String(), created, string(status))
		if err != nil {
			t.Fatalf("set created_at: %v", err)
		}
		got, err := s.Get(ctx, res.Notifications[0].ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		out = append(out, got)
	}
	return out
}

func TestListFilters(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	all := seed(t, s, 30, base)

	from, to := base.Add(5*time.Minute), base.Add(20*time.Minute)
	tests := []struct {
		name   string
		filter ListFilter
		match  func(notification.Notification) bool
	}{
		{"no filter", ListFilter{}, func(notification.Notification) bool { return true }},
		{"status", ListFilter{Status: notification.StatusSent},
			func(n notification.Notification) bool { return n.Status == notification.StatusSent }},
		{"channel", ListFilter{Channel: notification.ChannelEmail},
			func(n notification.Notification) bool { return n.Channel == notification.ChannelEmail }},
		{"date range is from-inclusive, to-exclusive", ListFilter{From: from, To: to},
			func(n notification.Notification) bool { return !n.CreatedAt.Before(from) && n.CreatedAt.Before(to) }},
		{"all combined", ListFilter{Status: notification.StatusPending, Channel: notification.ChannelSMS, From: from, To: to},
			func(n notification.Notification) bool {
				return n.Status == notification.StatusPending && n.Channel == notification.ChannelSMS &&
					!n.CreatedAt.Before(from) && n.CreatedAt.Before(to)
			}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.filter.Limit = 100
			page, err := s.List(context.Background(), tt.filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			// Expected: matching rows, newest first.
			var want []uuid.UUID
			for i := len(all) - 1; i >= 0; i-- {
				if tt.match(all[i]) {
					want = append(want, all[i].ID)
				}
			}
			if len(want) == 0 {
				t.Fatal("test data matches nothing; the case proves nothing")
			}
			if len(page.Items) != len(want) {
				t.Fatalf("got %d rows, want %d", len(page.Items), len(want))
			}
			for i := range want {
				if page.Items[i].ID != want[i] {
					t.Fatalf("row %d = %s, want %s", i, page.Items[i].ID, want[i])
				}
			}
			if page.Next != nil {
				t.Error("Next set on a page that holds every match")
			}
		})
	}
}

// TestListPaginationWithConcurrentInserts walks the list page by page while new rows
// arrive between pages. Every row that existed at the start must appear exactly once,
// in strictly descending order, and no page may repeat a row.
func TestListPaginationWithConcurrentInserts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	original := seed(t, s, 25, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))

	seen := map[uuid.UUID]int{}
	var prev *Cursor
	filter := ListFilter{Limit: 7}
	for pageNo := 0; ; pageNo++ {
		if pageNo > 10 {
			t.Fatal("pagination did not terminate")
		}
		page, err := s.List(ctx, filter)
		if err != nil {
			t.Fatalf("List page %d: %v", pageNo, err)
		}
		for _, n := range page.Items {
			seen[n.ID]++
			cur := Cursor{CreatedAt: n.CreatedAt, ID: n.ID}
			if prev != nil && !comesAfter(cur, *prev) {
				t.Fatalf("row %s is not after %s in list order", n.ID, prev.ID)
			}
			prev = &cur
		}

		// New traffic between page requests, as in production.
		if _, err := s.Create(ctx, single(smsRequest(pageNo))); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if page.Next == nil {
			break
		}
		filter.After = page.Next
	}

	for _, n := range original {
		if seen[n.ID] != 1 {
			t.Errorf("row %s seen %d times, want 1", n.ID, seen[n.ID])
		}
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("row %s repeated %d times", id, count)
		}
	}
}

func TestListLastPageExactlyFull(t *testing.T) {
	s := newTestStore(t)
	seed(t, s, 6, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))

	page, err := s.List(context.Background(), ListFilter{Limit: 3})
	if err != nil || len(page.Items) != 3 || page.Next == nil {
		t.Fatalf("page 1: %d items, next=%v, err=%v", len(page.Items), page.Next, err)
	}
	page, err = s.List(context.Background(), ListFilter{Limit: 3, After: page.Next})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("page 2: %d items, err=%v", len(page.Items), err)
	}
	if page.Next != nil {
		t.Error("Next set on the last page, which would make clients fetch an empty page")
	}
}

// comesAfter reports whether a follows b in the list order (created_at DESC, id DESC).
// UUID strings compare like the uuid column: both are fixed-width lowercase hex.
func comesAfter(a, b Cursor) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID.String() < b.ID.String()
}

package worker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/provider/mock"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

// TestScheduledNotificationIsNotSentEarly runs the real engine: a notification scheduled
// one second ahead reaches the provider after its time and not before, while an
// unscheduled one created at the same moment goes out at once (ADR-0017, A-38).
func TestScheduledNotificationIsNotSentEarly(t *testing.T) {
	st := newRealStore(t)
	at := time.Now().Add(time.Second).UTC().Truncate(time.Microsecond)
	reqs := []notification.Request{
		{Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "later", ScheduledAt: &at},
		{Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "now"},
	}
	for i := range reqs {
		reqs[i] = reqs[i].Normalized()
	}
	res, err := st.Create(context.Background(), store.CreateParams{Requests: reqs, Batch: true, MaxAttempts: 8})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	later, now := res.Notifications[0].ID, res.Notifications[1].ID

	m, _ := startEngine(t, st, mock.Options{}, fastRetries(), time.Second)
	final := waitFinal(t, st, []uuid.UUID{later, now}, 5*time.Second)

	byKey := requestsByKey(m)
	if len(byKey[later.String()]) != 1 || len(byKey[now.String()]) != 1 {
		t.Fatalf("provider requests: %d for the scheduled, %d for the immediate; want 1 each",
			len(byKey[later.String()]), len(byKey[now.String()]))
	}
	// The guarantee is on the database clock: the claim compares next_attempt_at with the
	// database's now(). sent_at is written by the same clock after the claim, so it must
	// never be earlier than scheduled_at.
	if s := final[later].SentAt; s == nil || s.Before(at) {
		t.Errorf("sent_at %v is before scheduled_at %v on the database clock", s, at)
	}
	// The mock stamps arrival with this machine's clock, which can differ from the
	// database's by a little (0.24 ms measured with Postgres in Docker), so this check
	// allows that much and no more.
	const clockSkew = 20 * time.Millisecond
	sentAt := byKey[later.String()][0].At
	if sentAt.Before(at.Add(-clockSkew)) {
		t.Errorf("scheduled notification reached the provider %v before its time", at.Sub(sentAt))
	}
	// Within a poll interval (testConfig: 20 ms) plus scheduling slack.
	if late := sentAt.Sub(at); late > 500*time.Millisecond {
		t.Errorf("scheduled notification sent %v after its time", late)
	}
	if !byKey[now.String()][0].At.Before(at) {
		t.Error("the immediate notification waited for the scheduled one")
	}
	for id, n := range final {
		if n.Status != notification.StatusSent {
			t.Errorf("%s: status %s, want sent", id, n.Status)
		}
	}
	t.Logf("scheduled for %s, reached the provider %v later", at.Format(time.RFC3339Nano), sentAt.Sub(at))
}

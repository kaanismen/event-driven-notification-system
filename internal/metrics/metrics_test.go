package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/store"
)

type fakeDepth struct {
	rows []store.QueueDepth
	err  error
}

func (f fakeDepth) QueueDepth(context.Context) ([]store.QueueDepth, error) { return f.rows, f.err }

func get(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d", rec.Code)
	}
	return rec.Body.String()
}

func TestQueueDepthIsReportedWithZeros(t *testing.T) {
	m := New()
	m.WatchQueue(fakeDepth{rows: []store.QueueDepth{
		{Channel: notification.ChannelSMS, Priority: notification.PriorityHigh, Status: notification.StatusPending, Count: 42},
	}}, slog.New(slog.DiscardHandler))

	body := get(t, m)
	for _, want := range []string{
		`notifier_queue_depth{channel="sms",priority="high",status="pending"} 42`,
		// Empty queues are reported as 0 rather than left out.
		`notifier_queue_depth{channel="push",priority="low",status="processing"} 0`,
		// Runtime metrics come with the registry.
		`go_goroutines `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output lacks %q", want)
		}
	}
	if n := strings.Count(body, "notifier_queue_depth{"); n != 18 {
		t.Errorf("%d queue depth series, want 18 (3 channels x 3 priorities x 2 statuses)", n)
	}
}

// A failing database query must not take the other metrics down with it.
func TestQueueDepthErrorKeepsOtherMetrics(t *testing.T) {
	m := New()
	m.WatchQueue(fakeDepth{err: errors.New("database unavailable")}, slog.New(slog.DiscardHandler))
	m.Delivery(notification.ChannelEmail, OutcomeSent)

	body := get(t, m)
	if !strings.Contains(body, `notifier_deliveries_total{channel="email",outcome="sent"} 1`) {
		t.Errorf("delivery counter missing when the depth query fails:\n%s", body)
	}
	if strings.Contains(body, "notifier_queue_depth{") {
		t.Error("queue depth reported although the query failed")
	}
}

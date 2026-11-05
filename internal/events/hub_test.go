package events

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/metrics"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

func connectedHub(t *testing.T, buffer int) (*Hub, *metrics.Metrics) {
	t.Helper()
	m := metrics.New()
	h := NewHub(m, buffer)
	h.SetConnected(true)
	return h, m
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func event(id uuid.UUID, batch *uuid.UUID, version int64) Event {
	return Event{ID: id, BatchID: batch, Status: notification.StatusProcessing, Version: version}
}

// metricLine returns the sample line of a metric (with labels, if given) from /metrics.
func metricLine(t *testing.T, m *metrics.Metrics, prefix string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(rec.Body.String()) {
		if strings.HasPrefix(line, prefix+" ") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func TestHubRoutesByNotificationAndBatch(t *testing.T) {
	h, _ := connectedHub(t, 8)
	batch := newID()
	inBatch, other := newID(), newID()

	byID, err := h.SubscribeNotification(inBatch)
	if err != nil {
		t.Fatal(err)
	}
	byBatch, err := h.SubscribeBatch(batch)
	if err != nil {
		t.Fatal(err)
	}

	h.Publish(event(inBatch, &batch, 1))
	h.Publish(event(other, nil, 1))
	h.Publish(event(newID(), &batch, 1))

	if n := len(byID.Events()); n != 1 {
		t.Errorf("notification subscriber got %d events, want 1", n)
	}
	if n := len(byBatch.Events()); n != 2 {
		t.Errorf("batch subscriber got %d events, want 2", n)
	}
	if ev := <-byID.Events(); ev.ID != inBatch {
		t.Errorf("event for %s, want %s", ev.ID, inBatch)
	}
}

// A subscriber that stops reading is cut off; the others keep receiving.
func TestHubDropsASlowSubscriber(t *testing.T) {
	h, m := connectedHub(t, 2)
	id := newID()
	slow, _ := h.SubscribeNotification(id)
	fast, _ := h.SubscribeNotification(id)

	for v := int64(1); v <= 3; v++ {
		h.Publish(event(id, nil, v))
		<-fast.Events() // fast reads each event as it comes
	}

	select {
	case <-slow.Done():
		if slow.Reason() != ReasonSlow {
			t.Errorf("reason = %v, want ReasonSlow", slow.Reason())
		}
	default:
		t.Fatal("slow subscriber was not ended")
	}
	select {
	case <-fast.Done():
		t.Fatal("fast subscriber was ended")
	default:
	}
	if got := metricLine(t, m, `notifier_event_subscribers_dropped_total{reason="slow"}`); !strings.HasSuffix(got, " 1") {
		t.Errorf("dropped metric = %q, want 1", got)
	}
	if got := metricLine(t, m, "notifier_event_subscribers"); got != "notifier_event_subscribers 1" {
		t.Errorf("subscriber gauge = %q, want 1", got)
	}
}

func TestHubResyncAndShutdown(t *testing.T) {
	h, m := connectedHub(t, 8)
	a, _ := h.SubscribeNotification(newID())
	b, _ := h.SubscribeBatch(newID())

	h.SetConnected(false)
	for _, s := range []*Subscription{a, b} {
		<-s.Done()
		if s.Reason() != ReasonResync {
			t.Errorf("reason = %v, want ReasonResync", s.Reason())
		}
	}
	if _, err := h.SubscribeNotification(newID()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("subscribe while disconnected: err = %v, want ErrUnavailable", err)
	}

	h.SetConnected(true)
	c, err := h.SubscribeNotification(newID())
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	<-c.Done()
	if c.Reason() != ReasonShutdown {
		t.Errorf("reason = %v, want ReasonShutdown", c.Reason())
	}
	if _, err := h.SubscribeNotification(newID()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("subscribe after Close: err = %v, want ErrUnavailable", err)
	}
	if got := metricLine(t, m, "notifier_event_subscribers"); got != "notifier_event_subscribers 0" {
		t.Errorf("subscriber gauge = %q, want 0", got)
	}
}

func TestSubscriptionCloseUnsubscribes(t *testing.T) {
	h, m := connectedHub(t, 1)
	id := newID()
	s, _ := h.SubscribeNotification(id)
	s.Close()
	s.Close() // twice is fine

	// Publishing after Close must neither block nor end anything.
	h.Publish(event(id, nil, 1))
	h.Publish(event(id, nil, 2))
	select {
	case <-s.Done():
		t.Error("a closed subscription was ended by the hub")
	default:
	}
	if got := metricLine(t, m, "notifier_event_subscribers"); got != "notifier_event_subscribers 0" {
		t.Errorf("subscriber gauge = %q, want 0", got)
	}
}

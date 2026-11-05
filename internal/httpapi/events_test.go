package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/events"
	"github.com/kaanismen/event-driven-notification-system/internal/metrics"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

// eventServer is a real HTTP server (WebSockets need a real connection to hijack) with a
// fake store and a hub the test publishes to.
type eventServer struct {
	t     *testing.T
	url   string
	store *fakeStore
	hub   *events.Hub
}

func newEventServer(t *testing.T, configure ...func(*http.Server)) *eventServer {
	t.Helper()
	fs := &fakeStore{}
	deps := testDeps(slog.New(slog.DiscardHandler), fs)
	srv := httptest.NewUnstartedServer(NewHandler(deps))
	for _, c := range configure {
		c(srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return &eventServer{t: t, url: "ws" + strings.TrimPrefix(srv.URL, "http"), store: fs, hub: deps.Events}
}

func (s *eventServer) dial(path string) *websocket.Conn {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, s.url+path, nil)
	if err != nil {
		s.t.Fatalf("dial %s: %v", path, err)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	s.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// readMessage reads one JSON message; on a close frame it returns the close status.
func readMessage(t *testing.T, conn *websocket.Conn) (map[string]any, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var msg map[string]any
	err := wsjson.Read(ctx, conn, &msg)
	if code := websocket.CloseStatus(err); code != -1 {
		return nil, code
	}
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg, -1
}

func expectClose(t *testing.T, conn *websocket.Conn, want websocket.StatusCode) {
	t.Helper()
	if msg, code := readMessage(t, conn); code != want {
		t.Fatalf("got message %v / close %d, want close %d", msg, code, want)
	}
}

func pendingNotification(batch *uuid.UUID) notification.Notification {
	n := pendingFrom(notification.Request{Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "x"}, batch)
	n.Version = 2
	return n
}

func (s *eventServer) publish(n notification.Notification, status notification.Status, version int64) {
	s.hub.Publish(events.Event{ID: n.ID, BatchID: n.BatchID, Status: status, Attempts: 1, Version: version, UpdatedAt: fixedTime})
}

// The snapshot is written after the handler subscribed, so once a test has read it, an
// event published afterwards cannot race ahead of the subscription.
func TestNotificationEventsStream(t *testing.T) {
	s := newEventServer(t)
	n := pendingNotification(nil)
	s.store.getFn = func(uuid.UUID) (notification.Notification, error) { return n, nil }

	conn := s.dial("/v1/notifications/" + n.ID.String() + "/events")
	snap, _ := readMessage(t, conn)
	if snap["type"] != "snapshot" || snap["notification"].(map[string]any)["status"] != "pending" {
		t.Fatalf("snapshot = %v", snap)
	}

	s.publish(n, notification.StatusProcessing, 2) // already in the snapshot: dropped
	s.publish(n, notification.StatusProcessing, 3)
	s.publish(n, notification.StatusSent, 4)

	msg, _ := readMessage(t, conn)
	if msg["type"] != "status" || msg["status"] != "processing" || msg["version"] != float64(3) || msg["id"] != n.ID.String() {
		t.Errorf("first event = %v, want processing at version 3", msg)
	}
	msg, _ = readMessage(t, conn)
	if msg["status"] != "sent" {
		t.Errorf("second event = %v, want sent", msg)
	}
	// Everything watched is final: the server closes normally.
	expectClose(t, conn, websocket.StatusNormalClosure)
}

func TestBatchEventsCloseWhenAllFinal(t *testing.T) {
	s := newEventServer(t)
	batch := uuid.Must(uuid.NewV7())
	a, b := pendingNotification(&batch), pendingNotification(&batch)
	s.store.getBatchFn = func(uuid.UUID) ([]notification.Notification, error) {
		return []notification.Notification{a, b}, nil
	}

	conn := s.dial("/v1/batches/" + batch.String() + "/events")
	snap, _ := readMessage(t, conn)
	if total := snap["batch"].(map[string]any)["total"]; total != float64(2) {
		t.Fatalf("snapshot = %v", snap)
	}

	s.publish(a, notification.StatusSent, 3)
	if msg, _ := readMessage(t, conn); msg["id"] != a.ID.String() {
		t.Fatalf("event = %v, want item a", msg)
	}
	s.publish(b, notification.StatusFailed, 3)
	if msg, _ := readMessage(t, conn); msg["status"] != "failed" {
		t.Fatalf("event = %v, want item b failed", msg)
	}
	expectClose(t, conn, websocket.StatusNormalClosure)
}

func TestEventsForAFinishedNotificationCloseAfterTheSnapshot(t *testing.T) {
	s := newEventServer(t)
	n := pendingNotification(nil)
	n.Status = notification.StatusSent
	s.store.getFn = func(uuid.UUID) (notification.Notification, error) { return n, nil }

	conn := s.dial("/v1/notifications/" + n.ID.String() + "/events")
	if snap, _ := readMessage(t, conn); snap["type"] != "snapshot" {
		t.Fatalf("snapshot = %v", snap)
	}
	expectClose(t, conn, websocket.StatusNormalClosure)
}

// The hub ending a subscription becomes a close code that tells the client what to do.
func TestEventsCloseCodes(t *testing.T) {
	tests := []struct {
		name string
		end  func(*events.Hub)
		want websocket.StatusCode
	}{
		{"listener lost the database", func(h *events.Hub) { h.SetConnected(false) }, websocket.StatusServiceRestart},
		{"server shutting down", func(h *events.Hub) { h.Close() }, websocket.StatusGoingAway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newEventServer(t)
			n := pendingNotification(nil)
			s.store.getFn = func(uuid.UUID) (notification.Notification, error) { return n, nil }
			conn := s.dial("/v1/notifications/" + n.ID.String() + "/events")
			readMessage(t, conn) // snapshot: the subscription exists
			tt.end(s.hub)
			expectClose(t, conn, tt.want)
		})
	}
}

func TestSlowEventClientIsDropped(t *testing.T) {
	fs := &fakeStore{}
	deps := testDeps(slog.New(slog.DiscardHandler), fs)
	deps.Events = events.NewHub(metrics.New(), 1) // room for a single queued event
	deps.Events.SetConnected(true)
	srv := httptest.NewServer(NewHandler(deps))
	t.Cleanup(srv.Close)
	s := &eventServer{t: t, url: "ws" + strings.TrimPrefix(srv.URL, "http"), store: fs, hub: deps.Events}

	n := pendingNotification(nil)
	fs.getFn = func(uuid.UUID) (notification.Notification, error) { return n, nil }
	conn := s.dial("/v1/notifications/" + n.ID.String() + "/events")
	readMessage(t, conn)

	// A burst faster than the handler can forward: the buffer of one overflows. Publish
	// holds the hub's lock, so the burst is not interleaved with the handler's reads.
	for v := int64(3); v < 3+50; v++ {
		s.publish(n, notification.StatusProcessing, v)
	}
	for {
		_, code := readMessage(t, conn)
		if code == -1 {
			continue // events that made it before the drop
		}
		if code != websocket.StatusPolicyViolation {
			t.Fatalf("close %d, want %d (policy violation)", code, websocket.StatusPolicyViolation)
		}
		return
	}
}

// The server's ReadTimeout and WriteTimeout (15 s in production) must not cut long-lived
// WebSockets. net/http clears those deadlines when a connection is hijacked; this test
// guards that behaviour, which the handler relies on.
func TestEventsOutliveServerTimeouts(t *testing.T) {
	s := newEventServer(t, func(srv *http.Server) {
		srv.ReadTimeout = 200 * time.Millisecond
		srv.WriteTimeout = 200 * time.Millisecond
	})
	n := pendingNotification(nil)
	s.store.getFn = func(uuid.UUID) (notification.Notification, error) { return n, nil }
	conn := s.dial("/v1/notifications/" + n.ID.String() + "/events")
	readMessage(t, conn)

	time.Sleep(600 * time.Millisecond) // three times the server timeouts
	s.publish(n, notification.StatusSent, 3)
	if msg, code := readMessage(t, conn); msg["status"] != "sent" {
		t.Fatalf("after the server timeouts: message %v, close %d", msg, code)
	}
}

func TestEventsHTTPErrors(t *testing.T) {
	s := newTestServer(t)
	id := uuid.Must(uuid.NewV7()).String()
	ws := func(path string) *httptest.ResponseRecorder {
		return s.do(http.MethodGet, path, "", "Upgrade", "websocket", "Connection", "Upgrade")
	}

	expectError(t, s.do(http.MethodGet, "/v1/notifications/"+id+"/events", ""), http.StatusUpgradeRequired, "upgrade_required")
	expectError(t, ws("/v1/notifications/nope/events"), http.StatusBadRequest, "invalid_id")
	// Unknown IDs are answered before the upgrade, as ordinary JSON errors.
	expectError(t, ws("/v1/notifications/"+id+"/events"), http.StatusNotFound, "not_found")
	expectError(t, ws("/v1/batches/"+id+"/events"), http.StatusNotFound, "not_found")

	// While the listener is reconnecting the hub refuses subscriptions: 503, not a stream
	// that silently misses events.
	deps := testDeps(slog.New(slog.DiscardHandler), &fakeStore{
		getFn: func(uuid.UUID) (notification.Notification, error) {
			return notification.Notification{}, errors.New("not reached")
		},
	})
	deps.Events.SetConnected(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/"+id+"/events", nil)
	req.Header.Set("Upgrade", "websocket")
	NewHandler(deps).ServeHTTP(rec, req)
	expectError(t, rec, http.StatusServiceUnavailable, "events_unavailable")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 without Retry-After")
	}
}

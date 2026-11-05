package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/events"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

const (
	// eventWriteTimeout cuts off a client whose TCP connection stopped taking data, on
	// top of the hub dropping clients whose buffer fills.
	eventWriteTimeout = 5 * time.Second
	// pingInterval keeps idle connections alive through proxies and detects dead peers.
	pingInterval = 30 * time.Second
)

// snapshotMessage is the first message on a status connection: the full current state.
type snapshotMessage struct {
	Type         string                `json:"type"` // always "snapshot"
	Notification *notificationResponse `json:"notification,omitempty"`
	Batch        *batchStatusResponse  `json:"batch,omitempty"`
}

// statusMessage is one status change. Embedding the event puts its fields at the top
// level of the JSON object, next to "type".
type statusMessage struct {
	Type string `json:"type"` // always "status"
	events.Event
}

// tracker remembers the newest version and status of every watched notification, so
// stale events are dropped and the connection can close once all of them are final.
type tracker struct {
	version map[uuid.UUID]int64
	open    map[uuid.UUID]bool // not yet in a final state
}

func newTracker(items []notification.Notification) *tracker {
	t := &tracker{version: map[uuid.UUID]int64{}, open: map[uuid.UUID]bool{}}
	for _, n := range items {
		t.version[n.ID] = n.Version
		if !n.Status.Final() {
			t.open[n.ID] = true
		}
	}
	return t
}

// apply reports whether ev is newer than what the client already has. An event can be
// older than the snapshot: the subscription starts before the snapshot is read, so the
// changes in between arrive twice, once in the snapshot and once as events.
func (t *tracker) apply(ev events.Event) bool {
	if ev.Version <= t.version[ev.ID] {
		return false
	}
	t.version[ev.ID] = ev.Version
	if ev.Status.Final() {
		delete(t.open, ev.ID)
	} else {
		t.open[ev.ID] = true
	}
	return true
}

func (t *tracker) done() bool { return len(t.open) == 0 }

// notificationEvents handles GET /v1/notifications/{id}/events (ADR-0020).
func (a *api) notificationEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !a.checkUpgrade(w, r) {
		return
	}
	// Subscribe first, then read the snapshot: a change committed in between is then in
	// the snapshot, in the event stream, or in both, but never lost.
	sub, err := a.events.SubscribeNotification(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer sub.Close()

	n, err := a.store.Get(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	resp := toResponse(n)
	a.stream(w, r, sub, snapshotMessage{Type: "snapshot", Notification: &resp}, newTracker([]notification.Notification{n}))
}

// batchEvents handles GET /v1/batches/{id}/events (ADR-0020).
func (a *api) batchEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !a.checkUpgrade(w, r) {
		return
	}
	sub, err := a.events.SubscribeBatch(id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer sub.Close()

	items, err := a.store.GetBatch(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	resp := toBatchStatus(id, items)
	a.stream(w, r, sub, snapshotMessage{Type: "snapshot", Batch: &resp}, newTracker(items))
}

// checkUpgrade answers a plain GET with a JSON 426 in the API's error format; the
// WebSocket library validates the rest of the handshake.
func (a *api) checkUpgrade(w http.ResponseWriter, r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return true
	}
	w.Header().Set("Upgrade", "websocket")
	writeError(w, http.StatusUpgradeRequired, "upgrade_required", "this endpoint is a WebSocket; connect with a WebSocket client")
	return false
}

// stream upgrades the connection, sends the snapshot, then every newer event until the
// watched notifications are all final, the client leaves, or the hub ends the
// subscription.
func (a *api) stream(w http.ResponseWriter, r *http.Request, sub *events.Subscription, snapshot snapshotMessage, t *tracker) {
	// Accept hijacks the connection, and net/http clears the server's ReadTimeout and
	// WriteTimeout deadlines when it hands a connection over; from here on the per-message
	// timeout and the pings below protect it.
	//
	// The default options reject cross-origin browser requests, which protects against
	// another website opening this socket in a visitor's browser.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		a.logger.InfoContext(r.Context(), "websocket handshake failed", "error", err)
		return // Accept has already answered the request
	}
	defer func() { _ = conn.CloseNow() }()

	// Clients send nothing; CloseRead handles their pings and close frames and cancels
	// ctx when they disconnect.
	ctx := conn.CloseRead(r.Context())
	write := func(v any) error {
		wctx, cancel := context.WithTimeout(ctx, eventWriteTimeout)
		defer cancel()
		return wsjson.Write(wctx, conn, v)
	}

	if err := write(snapshot); err != nil {
		return
	}
	if t.done() {
		_ = conn.Close(websocket.StatusNormalClosure, "all notifications are final")
		return
	}

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case ev := <-sub.Events():
			if !t.apply(ev) {
				continue
			}
			if err := write(statusMessage{Type: "status", Event: ev}); err != nil {
				return
			}
			if t.done() {
				_ = conn.Close(websocket.StatusNormalClosure, "all notifications are final")
				return
			}
		case <-sub.Done():
			code, reason := closeFor(sub.Reason())
			_ = conn.Close(code, reason)
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, eventWriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return // the client closed the connection
		}
	}
}

// closeFor maps why the hub ended a subscription to a WebSocket close code. After 1008
// and 1012 the client reconnects and gets a fresh snapshot.
func closeFor(r events.Reason) (websocket.StatusCode, string) {
	switch r {
	case events.ReasonSlow:
		return websocket.StatusPolicyViolation, "client too slow; reconnect for a fresh snapshot"
	case events.ReasonResync:
		return websocket.StatusServiceRestart, "events may have been missed; reconnect for a fresh snapshot"
	default:
		return websocket.StatusGoingAway, "server shutting down"
	}
}

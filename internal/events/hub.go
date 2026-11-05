// Package events delivers notification status changes to real-time subscribers
// (ADR-0020). A database trigger announces each change with NOTIFY; the Listener
// receives them on one dedicated connection per instance and the Hub fans each event
// out to the subscribers that watch that notification or its batch.
package events

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/metrics"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

// Event is one status change, as the trigger announces it.
type Event struct {
	ID        uuid.UUID           `json:"id"`
	BatchID   *uuid.UUID          `json:"batchId,omitempty"`
	Status    notification.Status `json:"status"`
	Attempts  int                 `json:"attempts"`
	Version   int64               `json:"version"`
	LastError string              `json:"lastError,omitempty"`
	UpdatedAt time.Time           `json:"updatedAt"`
}

// ErrUnavailable means the hub cannot promise to deliver events right now: the
// database listener is not connected, or the service is shutting down.
var ErrUnavailable = errors.New("real-time events are unavailable")

// Reason says why the hub ended a subscription.
type Reason int

const (
	// ReasonSlow: the subscriber's buffer was full. Waiting for it would delay every
	// other subscriber, so it is cut off and can reconnect for a fresh snapshot.
	ReasonSlow Reason = iota + 1
	// ReasonResync: the listener lost its database connection, so events may have
	// been missed. The subscriber must reconnect to get a fresh snapshot.
	ReasonResync
	// ReasonShutdown: the service is stopping.
	ReasonShutdown
)

// DefaultBuffer is how many events may wait for one subscriber. A 1000-item batch
// produces about two events per item within seconds; a client that reads normally
// never comes near this, while one that stops reading is noticed quickly.
const DefaultBuffer = 256

// Hub routes events to subscribers. It is safe for concurrent use.
type Hub struct {
	metrics *metrics.Metrics
	buffer  int

	mu             sync.Mutex
	connected      bool
	closed         bool
	byNotification map[uuid.UUID]map[*Subscription]struct{}
	byBatch        map[uuid.UUID]map[*Subscription]struct{}
}

// NewHub returns a hub that refuses subscriptions until the listener connects.
func NewHub(m *metrics.Metrics, buffer int) *Hub {
	return &Hub{
		metrics:        m,
		buffer:         buffer,
		byNotification: map[uuid.UUID]map[*Subscription]struct{}{},
		byBatch:        map[uuid.UUID]map[*Subscription]struct{}{},
	}
}

// Subscription receives the events of one notification or one batch until the
// subscriber closes it or the hub ends it.
type Subscription struct {
	hub    *Hub
	key    uuid.UUID
	batch  bool
	events chan Event
	done   chan struct{}
	reason Reason // set before done is closed
}

// Events delivers the events in commit order. It is never closed; select on Done too.
func (s *Subscription) Events() <-chan Event { return s.events }

// Done is closed when the hub ends the subscription; Reason then says why.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Reason is why the hub ended the subscription. Only valid after Done is closed.
func (s *Subscription) Reason() Reason { return s.reason }

// Close unsubscribes. It is safe to call more than once and after the hub ended it.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.remove(s)
}

// SubscribeNotification watches one notification.
func (h *Hub) SubscribeNotification(id uuid.UUID) (*Subscription, error) {
	return h.subscribe(id, false)
}

// SubscribeBatch watches every notification of one batch.
func (h *Hub) SubscribeBatch(id uuid.UUID) (*Subscription, error) {
	return h.subscribe(id, true)
}

func (h *Hub) subscribe(key uuid.UUID, batch bool) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Refusing is honest: a subscription made while the listener is down could silently
	// miss the events that happen before it reconnects.
	if !h.connected || h.closed {
		return nil, ErrUnavailable
	}
	s := &Subscription{
		hub: h, key: key, batch: batch,
		events: make(chan Event, h.buffer),
		done:   make(chan struct{}),
	}
	index := h.byNotification
	if batch {
		index = h.byBatch
	}
	if index[key] == nil {
		index[key] = map[*Subscription]struct{}{}
	}
	index[key][s] = struct{}{}
	h.metrics.Subscribers(1)
	return s, nil
}

// Publish hands an event to every subscriber of its notification and its batch. It
// never blocks: a subscriber whose buffer is full is ended with ReasonSlow instead, so
// one stuck client cannot hold up the listener or anyone else.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deliver(h.byNotification[ev.ID], ev)
	if ev.BatchID != nil {
		h.deliver(h.byBatch[*ev.BatchID], ev)
	}
}

func (h *Hub) deliver(subs map[*Subscription]struct{}, ev Event) {
	// Deleting from a map while ranging over it is allowed in Go.
	for s := range subs {
		select {
		case s.events <- ev:
		default:
			h.end(s, ReasonSlow)
		}
	}
}

// SetConnected records whether the listener is receiving events. Losing the connection
// ends every subscription with ReasonResync, because events may be missed until it is
// back.
func (h *Hub) SetConnected(connected bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connected && !connected {
		h.endAll(ReasonResync)
	}
	h.connected = connected
}

// Close ends every subscription with ReasonShutdown and refuses new ones. The HTTP
// server calls it when shutdown starts, because Server.Shutdown does not wait for or
// close WebSocket connections: they have left its control.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	h.endAll(ReasonShutdown)
}

func (h *Hub) endAll(r Reason) {
	for _, index := range []map[uuid.UUID]map[*Subscription]struct{}{h.byNotification, h.byBatch} {
		for _, subs := range index {
			for s := range subs {
				h.end(s, r)
			}
		}
	}
}

// end must be called with mu held.
func (h *Hub) end(s *Subscription, r Reason) {
	if !h.remove(s) {
		return
	}
	s.reason = r
	close(s.done)
	switch r {
	case ReasonSlow:
		h.metrics.SubscriberDropped(metrics.SubscriberSlow)
	case ReasonResync:
		h.metrics.SubscriberDropped(metrics.SubscriberResync)
	}
}

// remove must be called with mu held. It reports whether s was still subscribed.
func (h *Hub) remove(s *Subscription) bool {
	index := h.byNotification
	if s.batch {
		index = h.byBatch
	}
	subs := index[s.key]
	if _, ok := subs[s]; !ok {
		return false
	}
	delete(subs, s)
	if len(subs) == 0 {
		delete(index, s.key)
	}
	h.metrics.Subscribers(-1)
	return true
}

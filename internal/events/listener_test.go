package events

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
	"github.com/kaanismen/event-driven-notification-system/internal/testdb"
)

// startListener runs a listener against a fresh, migrated database and waits until it
// listens. It returns the store for making changes and the hub for subscribing.
func startListener(t *testing.T) (*store.Store, *Hub, func() error) {
	t.Helper()
	dbURL := testdb.New(t)
	logger := slog.New(slog.DiscardHandler)
	if err := store.Migrate(dbURL, logger); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(context.Background(), dbURL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	hub, _ := connectedHub(t, DefaultBuffer)
	hub.SetConnected(false) // the listener decides
	l := NewListener(pool.Config().ConnConfig.Copy(), hub, logger)
	l.minRetry = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitListening(t, hub)

	// kill ends the listener's database session from the outside, as a network failure
	// or a database restart would.
	kill := func() error {
		_, err := pool.Exec(context.Background(), `
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = current_database() AND query = 'LISTEN `+Channel+`'`)
		return err
	}
	return store.New(pool), hub, kill
}

func waitListening(t *testing.T, hub *Hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := hub.SubscribeNotification(uuid.Nil)
		if err == nil {
			s.Close()
			return
		}
		if !errors.Is(err, ErrUnavailable) || time.Now().After(deadline) {
			t.Fatalf("listener not ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func nextEvent(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case ev := <-s.Events():
		return ev
	case <-s.Done():
		t.Fatalf("subscription ended: reason %v", s.Reason())
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3 s")
	}
	return Event{}
}

func TestListenerDeliversDatabaseChanges(t *testing.T) {
	st, hub, _ := startListener(t)
	ctx := context.Background()

	req := notification.Request{Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "x"}.Normalized()
	res, err := st.Create(ctx, store.CreateParams{Requests: []notification.Request{req, req}, Batch: true, MaxAttempts: 8})
	if err != nil {
		t.Fatal(err)
	}
	first := res.Notifications[0]
	byID, err := hub.SubscribeNotification(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	byBatch, err := hub.SubscribeBatch(*res.BatchID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, byID)
	if ev.Status != notification.StatusCancelled || ev.Version != 1 || ev.BatchID == nil || *ev.BatchID != *res.BatchID {
		t.Errorf("event = %+v, want cancelled, version 1, in the batch", ev)
	}
	if ev := nextEvent(t, byBatch); ev.ID != first.ID {
		t.Errorf("batch subscriber got %s, want %s", ev.ID, first.ID)
	}

	if _, err := st.Cancel(ctx, res.Notifications[1].ID); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, byBatch); ev.ID != res.Notifications[1].ID {
		t.Errorf("batch subscriber got %s, want the second item", ev.ID)
	}
	if n := len(byID.Events()); n != 0 {
		t.Errorf("notification subscriber got %d events for another notification", n)
	}
}

// When the listener's connection dies, subscribers are told to resync, and the listener
// reconnects on its own.
func TestListenerReconnects(t *testing.T) {
	st, hub, kill := startListener(t)

	sub, err := hub.SubscribeNotification(uuid.Must(uuid.NewV7()))
	if err != nil {
		t.Fatal(err)
	}
	if err := kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Done():
		if sub.Reason() != ReasonResync {
			t.Errorf("reason = %v, want ReasonResync", sub.Reason())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber was not told to resync")
	}

	waitListening(t, hub)
	ctx := context.Background()
	req := notification.Request{Channel: notification.ChannelPush, Recipient: "device-1", Content: "x"}.Normalized()
	res, err := st.Create(ctx, store.CreateParams{Requests: []notification.Request{req}, MaxAttempts: 8})
	if err != nil {
		t.Fatal(err)
	}
	again, err := hub.SubscribeNotification(res.Notifications[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Cancel(ctx, res.Notifications[0].ID); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, again); ev.Status != notification.StatusCancelled {
		t.Errorf("after reconnect: event = %+v", ev)
	}
}

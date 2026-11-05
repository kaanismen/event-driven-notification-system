package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

// The update trigger bumps version on every update and announces status and attempt
// changes on the notification_status channel, at commit (ADR-0020).
func TestUpdatesBumpVersionAndAnnounceStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	listener, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err := listener.Exec(ctx, "LISTEN notification_status"); err != nil {
		t.Fatal(err)
	}

	n := createScheduled(t, s, nil)
	if n.Version != 0 {
		t.Fatalf("new row has version %d, want 0", n.Version)
	}

	claimed := claimSMS(t, s, 1)
	if len(claimed) != 1 || claimed[0].Version != 1 {
		t.Fatalf("claimed %d rows, version %d; want 1 row at version 1", len(claimed), claimed[0].Version)
	}
	expectEvent(t, listener, n.ID.String(), "processing", 1, 1)

	// A change that is neither status nor attempts bumps the version silently.
	if _, err := s.pool.Exec(ctx, `UPDATE notifications SET content = content WHERE id = $1`, n.ID.String()); err != nil {
		t.Fatal(err)
	}

	if ok, _, err := s.MarkSent(ctx, n.ID, 1, "provider-1"); err != nil || !ok {
		t.Fatalf("MarkSent: ok=%t err=%v", ok, err)
	}
	expectEvent(t, listener, n.ID.String(), "sent", 1, 3)

	got, err := s.Get(ctx, n.ID)
	if err != nil || got.Version != 3 {
		t.Errorf("stored version = %d (err %v), want 3", got.Version, err)
	}

	// A rolled-back change is never announced.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE notifications SET status = 'failed' WHERE id = $1`, n.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if msg, err := listener.WaitForNotification(waitCtx); err == nil {
		t.Errorf("got an event for a rolled-back update: %s", msg.Payload)
	}
}

func expectEvent(t *testing.T, conn *pgx.Conn, id, status string, attempts int, version int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg, err := conn.WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("no event for %s %s: %v", id, status, err)
	}
	var ev struct {
		ID        string              `json:"id"`
		Status    notification.Status `json:"status"`
		Attempts  int                 `json:"attempts"`
		Version   int64               `json:"version"`
		UpdatedAt time.Time           `json:"updatedAt"`
	}
	if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
		t.Fatalf("payload %q: %v", msg.Payload, err)
	}
	if ev.ID != id || string(ev.Status) != status || ev.Attempts != attempts || ev.Version != version || ev.UpdatedAt.IsZero() {
		t.Errorf("event = %+v, want id %s status %s attempts %d version %d", ev, id, status, attempts, version)
	}
}

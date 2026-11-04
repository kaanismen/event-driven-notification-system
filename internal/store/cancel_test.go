package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

func TestCancel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	create := func() uuid.UUID {
		t.Helper()
		res, err := s.Create(ctx, single(smsRequest(1)))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return res.Notifications[0].ID
	}
	setStatus := func(id uuid.UUID, status string) {
		t.Helper()
		_, err := s.pool.Exec(ctx,
			`UPDATE notifications SET status = $2, locked_until = now() + interval '30 seconds' WHERE id = $1`,
			id.String(), status)
		if err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
	}

	t.Run("pending becomes cancelled", func(t *testing.T) {
		id := create()
		n, err := s.Cancel(ctx, id)
		if err != nil || n.Status != notification.StatusCancelled {
			t.Fatalf("Cancel = %q, %v; want cancelled", n.Status, err)
		}
		if n.UpdatedAt.Before(n.CreatedAt) {
			t.Errorf("updated_at %v before created_at %v", n.UpdatedAt, n.CreatedAt)
		}
	})

	t.Run("cancelling twice succeeds", func(t *testing.T) {
		id := create()
		if _, err := s.Cancel(ctx, id); err != nil {
			t.Fatalf("first Cancel: %v", err)
		}
		n, err := s.Cancel(ctx, id)
		if err != nil || n.Status != notification.StatusCancelled {
			t.Fatalf("second Cancel = %q, %v; want cancelled", n.Status, err)
		}
	})

	for _, status := range []notification.Status{notification.StatusProcessing, notification.StatusSent, notification.StatusFailed} {
		t.Run(string(status)+" is not cancellable", func(t *testing.T) {
			id := create()
			setStatus(id, string(status))

			_, err := s.Cancel(ctx, id)
			var nc *NotCancellableError
			if !errors.As(err, &nc) || nc.Status != status {
				t.Fatalf("Cancel err = %v, want NotCancellableError{%s}", err, status)
			}
			if got, _ := s.Get(ctx, id); got.Status != status {
				t.Errorf("status changed to %q", got.Status)
			}
		})
	}

	t.Run("unknown id", func(t *testing.T) {
		if _, err := s.Cancel(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestCancelRacesClaim runs cancel and a worker-style claim against the same rows at the
// same moment. For every row exactly one side must win, never both and never neither.
// The real claim query arrives in Phase 3; this one uses the same guard (status = pending).
func TestCancelRacesClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const rows = 50
	ids := make([]uuid.UUID, rows)
	for i := range ids {
		res, err := s.Create(ctx, single(smsRequest(i)))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids[i] = res.Notifications[0].ID
	}

	for _, id := range ids {
		var (
			wg        sync.WaitGroup
			start     = make(chan struct{})
			cancelErr error
			claimed   int64
			claimErr  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = s.Cancel(ctx, id)
		}()
		go func() {
			defer wg.Done()
			<-start
			tag, err := s.pool.Exec(ctx, `
				UPDATE notifications
				SET status = 'processing', locked_until = now() + interval '30 seconds', attempts = attempts + 1
				WHERE id = $1 AND status = 'pending'`, id.String())
			claimed, claimErr = tag.RowsAffected(), err
		}()
		close(start)
		wg.Wait()

		if claimErr != nil {
			t.Fatalf("claim: %v", claimErr)
		}
		final, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		var nc *NotCancellableError
		switch {
		case cancelErr == nil && claimed == 0 && final.Status == notification.StatusCancelled:
			// cancel won
		case errors.As(cancelErr, &nc) && claimed == 1 && final.Status == notification.StatusProcessing:
			// claim won
		default:
			t.Fatalf("row %s: cancelErr=%v claimed=%d final=%s; want exactly one winner", id, cancelErr, claimed, final.Status)
		}
	}
}

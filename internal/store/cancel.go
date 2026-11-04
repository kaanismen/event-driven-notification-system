package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

// NotCancellableError means the notification exists but has left the pending state, so
// cancelling it would not be truthful (ADR-0007). Callers detect it with errors.As.
type NotCancellableError struct {
	Status notification.Status
}

func (e *NotCancellableError) Error() string {
	return fmt.Sprintf("notification is %s and can no longer be cancelled", e.Status)
}

// Cancel moves a pending notification to cancelled. The WHERE clause makes the check and
// the change one atomic step: if a worker claims the row at the same moment, Postgres
// row locking lets exactly one of the two updates match. Cancelling an already
// cancelled notification succeeds, so clients can retry safely.
func (s *Store) Cancel(ctx context.Context, id uuid.UUID) (notification.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `
		UPDATE notifications
		SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+notificationColumns, id.String()))
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return notification.Notification{}, fmt.Errorf("cancel notification: %w", err)
	}

	// Nothing matched: either the ID is unknown or the row is no longer pending.
	current, err := s.Get(ctx, id)
	if err != nil {
		return notification.Notification{}, err
	}
	if current.Status == notification.StatusCancelled {
		return current, nil
	}
	return notification.Notification{}, &NotCancellableError{Status: current.Status}
}

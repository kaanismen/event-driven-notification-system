package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

// Cursor is a position in the list order (created_at DESC, id DESC): the last row of
// the previous page. The HTTP layer encodes it as an opaque string.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListFilter selects notifications. Zero values mean "no filter".
type ListFilter struct {
	Status  notification.Status
	Channel notification.Channel
	From    time.Time // inclusive lower bound on created_at
	To      time.Time // exclusive upper bound on created_at (A-12)
	After   *Cursor   // nil for the first page
	Limit   int
}

// ListPage is one page of results. Next is nil on the last page.
type ListPage struct {
	Items []notification.Notification
	Next  *Cursor
}

// List returns one page, newest first, using keyset pagination (A-13): the next page
// starts strictly after the last row of this one, so pages stay fast at any depth and
// new inserts never shift rows between pages.
func (s *Store) List(ctx context.Context, f ListFilter) (ListPage, error) {
	if f.Limit <= 0 {
		return ListPage{}, fmt.Errorf("list: limit must be positive, got %d", f.Limit)
	}

	// Only the filters that are set become SQL conditions, so the planner sees a plain
	// query instead of "$1 IS NULL OR status = $1" for every possible filter.
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Status != "" {
		conds = append(conds, "status = "+arg(string(f.Status)))
	}
	if f.Channel != "" {
		conds = append(conds, "channel = "+arg(string(f.Channel)))
	}
	if !f.From.IsZero() {
		conds = append(conds, "created_at >= "+arg(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "created_at < "+arg(f.To))
	}
	if f.After != nil {
		// A row comparison matches the (created_at DESC, id DESC) index directly; the id
		// breaks ties between rows created in the same transaction.
		conds = append(conds, fmt.Sprintf("(created_at, id) < (%s, %s)", arg(f.After.CreatedAt), arg(f.After.ID.String())))
	}

	query := `SELECT ` + notificationColumns + ` FROM notifications`
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, " AND ")
	}
	// One extra row tells us whether another page exists without a COUNT query.
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + arg(f.Limit+1)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return ListPage{}, fmt.Errorf("list notifications: %w", err)
	}
	items, err := collectNotifications(rows)
	if err != nil {
		return ListPage{}, fmt.Errorf("list notifications: %w", err)
	}

	page := ListPage{Items: items}
	if len(items) > f.Limit {
		page.Items = items[:f.Limit]
		last := page.Items[f.Limit-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

package store

import (
	"context"
	"fmt"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

// QueueDepth is the number of open notifications for one channel, priority and status.
type QueueDepth struct {
	Channel  notification.Channel
	Priority notification.Priority
	Status   notification.Status
	Count    int64
}

// QueueDepth counts open notifications: pending (due now or retrying later) and
// processing. Both statuses have a partial index, so the count reads only open rows,
// however many finished rows the table holds.
func (s *Store) QueueDepth(ctx context.Context) ([]QueueDepth, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel, priority, status, count(*)
		FROM notifications
		WHERE status IN ('pending', 'processing')
		GROUP BY channel, priority, status`)
	if err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	defer rows.Close()

	var out []QueueDepth
	for rows.Next() {
		var (
			d               QueueDepth
			channel, status string
			rank            int16
		)
		if err := rows.Scan(&channel, &rank, &status, &d.Count); err != nil {
			return nil, fmt.Errorf("queue depth: %w", err)
		}
		if d.Priority, err = notification.PriorityFromRank(rank); err != nil {
			return nil, fmt.Errorf("queue depth: %w", err)
		}
		d.Channel, d.Status = notification.Channel(channel), notification.Status(status)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	return out, nil
}

package store

import (
	"context"
	"fmt"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

// Queue states reported by QueueDepth. They are the open statuses, except that pending
// rows still waiting for their scheduled time are counted apart, so a campaign
// scheduled for tomorrow does not look like a backlog today (ADR-0017).
const (
	StatePending    = "pending"    // due now, or a retry waiting for its backoff
	StateScheduled  = "scheduled"  // pending, scheduled for a time still in the future
	StateProcessing = "processing" // claimed by a worker
)

// QueueStates lists the states QueueDepth reports, in a fixed order.
func QueueStates() []string { return []string{StatePending, StateScheduled, StateProcessing} }

// QueueDepth is the number of open notifications for one channel, priority and state.
type QueueDepth struct {
	Channel  notification.Channel
	Priority notification.Priority
	State    string
	Count    int64
}

// QueueDepth counts open notifications per channel, priority and state. Both open
// statuses have a partial index, so the count reads only open rows, however many
// finished rows the table holds.
func (s *Store) QueueDepth(ctx context.Context) ([]QueueDepth, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel, priority,
		       CASE WHEN status = 'pending' AND scheduled_at > now() THEN 'scheduled' ELSE status END AS state,
		       count(*)
		FROM notifications
		WHERE status IN ('pending', 'processing')
		GROUP BY 1, 2, 3`)
	if err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	defer rows.Close()

	var out []QueueDepth
	for rows.Next() {
		var (
			d       QueueDepth
			channel string
			rank    int16
		)
		if err := rows.Scan(&channel, &rank, &d.State, &d.Count); err != nil {
			return nil, fmt.Errorf("queue depth: %w", err)
		}
		if d.Priority, err = notification.PriorityFromRank(rank); err != nil {
			return nil, fmt.Errorf("queue depth: %w", err)
		}
		d.Channel = notification.Channel(channel)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	return out, nil
}

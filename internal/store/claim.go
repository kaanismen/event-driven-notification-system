package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

// ClaimParams describes one claim cycle for one channel.
type ClaimParams struct {
	Channel notification.Channel
	Limit   int
	// Weights are the shares of high, normal and low priority when there is a backlog.
	Weights [3]int
	// Lease is how long the claimed rows belong to this worker (ADR-0005).
	Lease time.Duration
}

// claimSet is the update every claim applies: the row becomes processing, gets a lease,
// and its attempt counter goes up now, so a message that crashes its worker every time
// still runs out of attempts (ADR-0005).
const claimSet = `
UPDATE notifications
SET status = 'processing',
    locked_until = now() + $2::bigint * interval '1 millisecond',
    attempts = attempts + 1,
    updated_at = now()
WHERE id IN (SELECT id FROM picked)
RETURNING ` + notificationColumns

// The locking SELECT sits in a CTE because Postgres evaluates a CTE with FOR UPDATE
// exactly once; each picked row is locked once and then updated. SKIP LOCKED makes
// concurrent claimers pass over rows another worker is taking instead of waiting.
const (
	claimExpiredSQL = `
WITH picked AS (
    SELECT id FROM notifications
    WHERE channel = $1 AND status = 'processing' AND locked_until < now()
    ORDER BY locked_until
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)` + claimSet

	claimLevelSQL = `
WITH picked AS (
    SELECT id FROM notifications
    WHERE channel = $1 AND status = 'pending' AND next_attempt_at <= now() AND priority = $4
    ORDER BY next_attempt_at
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)` + claimSet

	claimAnySQL = `
WITH picked AS (
    SELECT id FROM notifications
    WHERE channel = $1 AND status = 'pending' AND next_attempt_at <= now()
    ORDER BY priority, next_attempt_at
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)` + claimSet
)

// Claim takes up to p.Limit due rows of one channel for delivery, in up to five small
// statements, each atomic on its own:
//
//  1. Rows whose lease expired (their worker died). They have waited longest, so they go
//     first, whatever their priority.
//  2. Due pending rows per priority level, up to that level's weighted share (ADR-0008).
//  3. If a level had fewer rows than its share, one more query fills the unused slots in
//     priority order, so no capacity is wasted while any work is waiting.
//
// The result is sorted by priority, then by due time.
func (s *Store) Claim(ctx context.Context, p ClaimParams) ([]notification.Notification, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("claim: limit must be positive, got %d", p.Limit)
	}
	channel := string(p.Channel)
	leaseMS := p.Lease.Milliseconds()

	claimed, err := s.claim(ctx, claimExpiredSQL, channel, leaseMS, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("claim expired leases: %w", err)
	}

	if remaining := p.Limit - len(claimed); remaining > 0 {
		quotas := splitQuota(remaining, p.Weights)
		levels := [3]notification.Priority{notification.PriorityHigh, notification.PriorityNormal, notification.PriorityLow}
		for i, prio := range levels {
			if quotas[i] == 0 {
				continue
			}
			rows, err := s.claim(ctx, claimLevelSQL, channel, leaseMS, quotas[i], prio.Rank())
			if err != nil {
				return nil, fmt.Errorf("claim %s priority: %w", prio, err)
			}
			claimed = append(claimed, rows...)
		}
	}

	if remaining := p.Limit - len(claimed); remaining > 0 {
		rows, err := s.claim(ctx, claimAnySQL, channel, leaseMS, remaining)
		if err != nil {
			return nil, fmt.Errorf("claim unused share: %w", err)
		}
		claimed = append(claimed, rows...)
	}

	sort.SliceStable(claimed, func(i, j int) bool {
		a, b := claimed[i], claimed[j]
		if a.Priority.Rank() != b.Priority.Rank() {
			return a.Priority.Rank() < b.Priority.Rank()
		}
		return a.NextAttemptAt.Before(b.NextAttemptAt)
	})
	return claimed, nil
}

func (s *Store) claim(ctx context.Context, sql, channel string, leaseMS int64, limit int, extra ...any) ([]notification.Notification, error) {
	args := append([]any{channel, leaseMS, limit}, extra...)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return collectNotifications(rows)
}

// splitQuota divides limit slots between high, normal and low. With at least three
// slots, every level gets one first, so even a tiny weight never starves; the rest is
// split by weight, rounding down, and what rounding leaves goes to high priority.
// With 20 slots and 70/20/10 the result is 14/4/2.
func splitQuota(limit int, weights [3]int) [3]int {
	var q [3]int
	if limit < len(q) {
		for i := 0; i < limit; i++ {
			q[i] = 1
		}
		return q
	}

	total := weights[0] + weights[1] + weights[2]
	rest := limit - len(q)
	used := 0
	for i, w := range weights {
		q[i] = 1 + rest*w/total
		used += q[i]
	}
	q[0] += limit - used
	return q
}

// MarkSent records a successful delivery. The update only matches while this worker
// still owns the row (status processing and the attempt it claimed), so a worker whose
// lease expired cannot overwrite the result of the worker that took the row over
// (ADR-0005). It reports whether the row was updated.
func (s *Store) MarkSent(ctx context.Context, id uuid.UUID, attempt int, providerMessageID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = 'sent', sent_at = now(), provider_message_id = NULLIF($3, ''),
		    locked_until = NULL, last_error = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		id.String(), attempt, providerMessageID)
	if err != nil {
		return false, fmt.Errorf("mark sent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkFailed records a delivery that will not be retried, with the same ownership guard
// as MarkSent. Phase 4 adds retries; until then every failure is final.
func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID, attempt int, reason string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = 'failed', last_error = $3, locked_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		id.String(), attempt, reason)
	if err != nil {
		return false, fmt.Errorf("mark failed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

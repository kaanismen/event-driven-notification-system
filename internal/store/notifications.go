package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

var (
	// ErrNotFound means no notification or batch has the requested ID.
	ErrNotFound = errors.New("not found")
	// ErrIdempotencyConflict means the Idempotency-Key was already used for a different
	// request (ADR-0004).
	ErrIdempotencyConflict = errors.New("idempotency key already used with a different request")
)

// Store reads and writes notifications in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// querier is the part of the pgx API that both the pool and a transaction provide, so
// read helpers work inside and outside a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CreateParams describes one create call: a single notification or a whole batch.
type CreateParams struct {
	// Requests must already be normalized and validated.
	Requests []notification.Request
	// Batch is true for the batch endpoint; a batch gets a batch ID even with one item.
	Batch         bool
	MaxAttempts   int
	CorrelationID string
	// IdempotencyKey is empty when the client sent no Idempotency-Key header.
	IdempotencyKey string
	// RequestHash identifies the normalized request; required with IdempotencyKey.
	RequestHash []byte
}

// CreateResult is what Create stored, or what an earlier request with the same key
// stored when Replayed is true.
type CreateResult struct {
	BatchID       *uuid.UUID
	Notifications []notification.Notification
	Replayed      bool
}

const (
	resourceNotification = "notification"
	resourceBatch        = "batch"
)

// notificationColumns is the column list every read uses, in scanNotification order.
const notificationColumns = `id, batch_id, channel, recipient, content, priority, status,
	attempts, max_attempts, next_attempt_at, last_error, provider_message_id,
	correlation_id, created_at, updated_at, sent_at, scheduled_at, template_id, version`

// insertNotifications inserts any number of rows with one fixed statement: each column
// arrives as an array and unnest turns the arrays back into rows. This avoids building
// SQL with up to 1000 VALUES tuples (8000 parameters) for a full batch. A scheduled row
// starts with next_attempt_at at its scheduled time, so the existing claim simply does not
// see it before then (ADR-0017); every other row is due at once. content is already
// final: a templated request was rendered before it got here (ADR-0019).
const insertNotifications = `
INSERT INTO notifications (id, batch_id, channel, recipient, content, priority, max_attempts, correlation_id,
                           scheduled_at, next_attempt_at, template_id)
SELECT t.id, $2::uuid, t.channel, t.recipient, t.content, t.priority, $7::int, $8::text,
       t.scheduled_at, coalesce(t.scheduled_at, now()), t.template_id
FROM unnest($1::uuid[], $3::text[], $4::text[], $5::text[], $6::smallint[], $9::timestamptz[], $10::uuid[])
    AS t(id, channel, recipient, content, priority, scheduled_at, template_id)
RETURNING ` + notificationColumns

// Create inserts the notifications and, if a key is given, the idempotency key, in one
// transaction. If the key already exists with the same request hash, nothing is
// inserted and the existing resource is returned in its current state (ADR-0004,
// ADR-0012).
func (s *Store) Create(ctx context.Context, p CreateParams) (CreateResult, error) {
	if len(p.Requests) == 0 || (!p.Batch && len(p.Requests) != 1) {
		return CreateResult{}, fmt.Errorf("create: %d requests for batch=%t", len(p.Requests), p.Batch)
	}

	// UUIDv7 IDs start with a timestamp, so IDs generated in order sort in order: batch
	// items read back by ID come out in request order, and inserts into the primary key
	// index stay at its right-hand edge instead of touching random pages.
	ids := make([]uuid.UUID, len(p.Requests))
	for i := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return CreateResult{}, fmt.Errorf("generate notification id: %w", err)
		}
		ids[i] = id
	}

	resourceType, resourceID := resourceNotification, ids[0]
	var batchID *uuid.UUID
	if p.Batch {
		id, err := uuid.NewV7()
		if err != nil {
			return CreateResult{}, fmt.Errorf("generate batch id: %w", err)
		}
		batchID = &id
		resourceType, resourceID = resourceBatch, id
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreateResult{}, fmt.Errorf("begin create: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this covers every error return.
	defer func() { _ = tx.Rollback(ctx) }()

	if p.IdempotencyKey != "" {
		// If another transaction holds the same key and has not committed yet, this INSERT
		// waits for it. Then it either inserts (the other one rolled back) or does nothing
		// (the other one committed), so exactly one request wins.
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (key, request_hash, resource_type, resource_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (key) DO NOTHING`,
			p.IdempotencyKey, p.RequestHash, resourceType, resourceID)
		if err != nil {
			return CreateResult{}, fmt.Errorf("reserve idempotency key: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return replay(ctx, tx, p, resourceType)
		}
	}

	idText := make([]string, len(p.Requests))
	channels := make([]string, len(p.Requests))
	recipients := make([]string, len(p.Requests))
	contents := make([]string, len(p.Requests))
	priorities := make([]int16, len(p.Requests))
	scheduled := make([]*time.Time, len(p.Requests)) // nil elements become SQL NULL
	templates := make([]*string, len(p.Requests))
	for i, r := range p.Requests {
		idText[i] = ids[i].String()
		channels[i] = string(r.Channel)
		recipients[i] = r.Recipient
		contents[i] = r.Content
		priorities[i] = r.Priority.Rank()
		scheduled[i] = r.ScheduledAt
		if r.TemplateID != nil {
			id := r.TemplateID.String()
			templates[i] = &id
		}
	}

	var batchArg any // nil is sent as SQL NULL
	if batchID != nil {
		batchArg = batchID.String()
	}

	rows, err := tx.Query(ctx, insertNotifications,
		idText, batchArg, channels, recipients, contents, priorities, p.MaxAttempts, p.CorrelationID, scheduled, templates)
	if err != nil {
		return CreateResult{}, fmt.Errorf("insert notifications: %w", err)
	}
	inserted, err := collectNotifications(rows)
	if err != nil {
		return CreateResult{}, fmt.Errorf("insert notifications: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, fmt.Errorf("commit create: %w", err)
	}

	// RETURNING has no guaranteed order, so put the rows back in request order.
	byID := make(map[uuid.UUID]notification.Notification, len(inserted))
	for _, n := range inserted {
		byID[n.ID] = n
	}
	ordered := make([]notification.Notification, len(ids))
	for i, id := range ids {
		n, ok := byID[id]
		if !ok {
			return CreateResult{}, fmt.Errorf("insert notifications: row %s missing from RETURNING", id)
		}
		ordered[i] = n
	}
	return CreateResult{BatchID: batchID, Notifications: ordered}, nil
}

// replay handles a request whose idempotency key already exists. Under READ COMMITTED
// each statement sees all committed data, so the key row is visible here even though it
// was committed after this transaction began.
func replay(ctx context.Context, q querier, p CreateParams, resourceType string) (CreateResult, error) {
	var (
		storedHash []byte
		storedType string
		storedID   uuid.UUID
	)
	err := q.QueryRow(ctx,
		`SELECT request_hash, resource_type, resource_id FROM idempotency_keys WHERE key = $1`,
		p.IdempotencyKey).Scan(&storedHash, &storedType, &storedID)
	if err != nil {
		return CreateResult{}, fmt.Errorf("read idempotency key: %w", err)
	}
	if storedType != resourceType || !bytes.Equal(storedHash, p.RequestHash) {
		return CreateResult{}, ErrIdempotencyConflict
	}

	if storedType == resourceNotification {
		n, err := getNotification(ctx, q, storedID)
		if err != nil {
			return CreateResult{}, fmt.Errorf("replay notification %s: %w", storedID, err)
		}
		return CreateResult{Notifications: []notification.Notification{n}, Replayed: true}, nil
	}

	items, err := getBatch(ctx, q, storedID)
	if err != nil {
		return CreateResult{}, fmt.Errorf("replay batch %s: %w", storedID, err)
	}
	return CreateResult{BatchID: &storedID, Notifications: items, Replayed: true}, nil
}

// Get returns one notification by ID.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (notification.Notification, error) {
	return getNotification(ctx, s.pool, id)
}

// GetBatch returns every notification of a batch in request order.
func (s *Store) GetBatch(ctx context.Context, batchID uuid.UUID) ([]notification.Notification, error) {
	return getBatch(ctx, s.pool, batchID)
}

func getNotification(ctx context.Context, q querier, id uuid.UUID) (notification.Notification, error) {
	n, err := scanNotification(q.QueryRow(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE id = $1`, id.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return notification.Notification{}, ErrNotFound
	}
	if err != nil {
		return notification.Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return n, nil
}

func getBatch(ctx context.Context, q querier, batchID uuid.UUID) ([]notification.Notification, error) {
	// ORDER BY id is request order because IDs are UUIDv7 generated in sequence.
	rows, err := q.Query(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE batch_id = $1 ORDER BY id`,
		batchID.String())
	if err != nil {
		return nil, fmt.Errorf("get batch: %w", err)
	}
	items, err := collectNotifications(rows)
	if err != nil {
		return nil, fmt.Errorf("get batch: %w", err)
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return items, nil
}

func collectNotifications(rows pgx.Rows) ([]notification.Notification, error) {
	defer rows.Close()
	var out []notification.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// scanNotification reads one row in notificationColumns order. Nullable columns are
// scanned into pointers (nil for NULL) and then flattened into the domain type.
func scanNotification(row pgx.Row) (notification.Notification, error) {
	var (
		n                                  notification.Notification
		batchID, templateID                uuid.NullUUID
		channel, status                    string
		priority                           int16
		lastError, providerID, correlation *string
	)
	err := row.Scan(&n.ID, &batchID, &channel, &n.Recipient, &n.Content, &priority, &status,
		&n.Attempts, &n.MaxAttempts, &n.NextAttemptAt, &lastError, &providerID,
		&correlation, &n.CreatedAt, &n.UpdatedAt, &n.SentAt, &n.ScheduledAt, &templateID, &n.Version)
	if err != nil {
		return notification.Notification{}, err
	}

	if batchID.Valid {
		n.BatchID = &batchID.UUID
	}
	if templateID.Valid {
		n.TemplateID = &templateID.UUID
	}
	n.Channel = notification.Channel(channel)
	n.Status = notification.Status(status)
	if n.Priority, err = notification.PriorityFromRank(priority); err != nil {
		return notification.Notification{}, err
	}
	n.LastError = deref(lastError)
	n.ProviderMessageID = deref(providerID)
	n.CorrelationID = deref(correlation)

	// pgx returns times in the process's local zone; the API always speaks UTC (A-12).
	n.NextAttemptAt = n.NextAttemptAt.UTC()
	n.CreatedAt = n.CreatedAt.UTC()
	n.UpdatedAt = n.UpdatedAt.UTC()
	if n.SentAt != nil {
		t := n.SentAt.UTC()
		n.SentAt = &t
	}
	if n.ScheduledAt != nil {
		t := n.ScheduledAt.UTC()
		n.ScheduledAt = &t
	}
	return n, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

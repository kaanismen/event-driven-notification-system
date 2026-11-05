package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

// ErrTemplateNameTaken means a template with this name already exists with a different
// channel or body. Templates are immutable, so the name cannot be reused (ADR-0019).
var ErrTemplateNameTaken = errors.New("template name already used for a different template")

const templateColumns = `id, name, channel, body, created_at`

// CreateTemplate stores a new template. Creating the same template again (same name,
// channel and body) returns the stored one with created false, so a client can retry a
// create safely without an Idempotency-Key: the unique name plays that role.
func (s *Store) CreateTemplate(ctx context.Context, name string, channel notification.Channel, body string) (t notification.Template, created bool, err error) {
	id, err := uuid.NewV7()
	if err != nil {
		return notification.Template{}, false, fmt.Errorf("generate template id: %w", err)
	}

	// ON CONFLICT DO NOTHING waits for a concurrent insert of the same name to finish, so
	// of two identical requests exactly one inserts and the other finds its row below.
	t, err = scanTemplate(s.pool.QueryRow(ctx, `
		INSERT INTO templates (id, name, channel, body) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO NOTHING
		RETURNING `+templateColumns,
		id.String(), name, string(channel), body))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return notification.Template{}, false, fmt.Errorf("insert template: %w", err)
	}

	existing, err := scanTemplate(s.pool.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM templates WHERE name = $1`, name))
	if err != nil {
		return notification.Template{}, false, fmt.Errorf("read template %q: %w", name, err)
	}
	if existing.Channel != channel || existing.Body != body {
		return notification.Template{}, false, ErrTemplateNameTaken
	}
	return existing, false, nil
}

// GetTemplate returns one template by ID.
func (s *Store) GetTemplate(ctx context.Context, id uuid.UUID) (notification.Template, error) {
	t, err := scanTemplate(s.pool.QueryRow(ctx,
		`SELECT `+templateColumns+` FROM templates WHERE id = $1`, id.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return notification.Template{}, ErrNotFound
	}
	if err != nil {
		return notification.Template{}, fmt.Errorf("get template: %w", err)
	}
	return t, nil
}

// GetTemplates returns the templates with the given IDs, keyed by ID, in one query; IDs
// that do not exist are simply absent. A batch of 1000 items that uses three templates
// costs one round trip, not 1000.
func (s *Store) GetTemplates(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]notification.Template, error) {
	out := make(map[uuid.UUID]notification.Template, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	text := make([]string, len(ids))
	for i, id := range ids {
		text[i] = id.String()
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+templateColumns+` FROM templates WHERE id = ANY($1::uuid[])`, text)
	if err != nil {
		return nil, fmt.Errorf("get templates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, fmt.Errorf("get templates: %w", err)
		}
		out[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get templates: %w", err)
	}
	return out, nil
}

// TemplateFilter selects templates. Zero values mean "no filter".
type TemplateFilter struct {
	Channel notification.Channel
	After   *Cursor // nil for the first page
	Limit   int
}

// TemplatePage is one page of templates. Next is nil on the last page.
type TemplatePage struct {
	Items []notification.Template
	Next  *Cursor
}

// ListTemplates returns one page, newest first, with the same keyset pagination as List.
func (s *Store) ListTemplates(ctx context.Context, f TemplateFilter) (TemplatePage, error) {
	if f.Limit <= 0 {
		return TemplatePage{}, fmt.Errorf("list templates: limit must be positive, got %d", f.Limit)
	}
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Channel != "" {
		conds = append(conds, "channel = "+arg(string(f.Channel)))
	}
	if f.After != nil {
		conds = append(conds, fmt.Sprintf("(created_at, id) < (%s, %s)", arg(f.After.CreatedAt), arg(f.After.ID.String())))
	}
	query := `SELECT ` + templateColumns + ` FROM templates`
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, " AND ")
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ` + arg(f.Limit+1)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return TemplatePage{}, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()
	var items []notification.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return TemplatePage{}, fmt.Errorf("list templates: %w", err)
		}
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return TemplatePage{}, fmt.Errorf("list templates: %w", err)
	}

	page := TemplatePage{Items: items}
	if len(items) > f.Limit {
		page.Items = items[:f.Limit]
		last := page.Items[f.Limit-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func scanTemplate(row pgx.Row) (notification.Template, error) {
	var (
		t       notification.Template
		channel string
	)
	if err := row.Scan(&t.ID, &t.Name, &channel, &t.Body, &t.CreatedAt); err != nil {
		return notification.Template{}, err
	}
	t.Channel = notification.Channel(channel)
	t.CreatedAt = t.CreatedAt.UTC()
	return t, nil
}

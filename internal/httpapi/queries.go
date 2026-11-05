package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

const (
	defaultPageSize = 50  // A-13
	maxPageSize     = 200 // A-13
)

// pathID parses the {id} path segment.
func pathID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.UUID{}, badRequest("invalid_id", "id must be a UUID")
	}
	return id, nil
}

// isListParam reports whether GET /v1/notifications accepts the query parameter.
// Anything else is rejected, for the same reason as unknown JSON fields:
// "?stauts=failed" must not silently list everything.
func isListParam(name string) bool {
	switch name {
	case "status", "channel", "scheduled", "from", "to", "limit", "cursor":
		return true
	}
	return false
}

func parseListQuery(q url.Values) (store.ListFilter, error) {
	f := store.ListFilter{Limit: defaultPageSize}
	for name, values := range q {
		if !isListParam(name) {
			return f, badRequest("invalid_query", "unknown query parameter %q", name)
		}
		if len(values) > 1 {
			return f, badRequest("invalid_query", "query parameter %q must be given once", name)
		}
	}

	if v := q.Get("status"); v != "" {
		f.Status = notification.Status(v)
		if !f.Status.Valid() {
			return f, badRequest("invalid_query", "status must be one of pending, processing, sent, failed, cancelled")
		}
	}
	if v := q.Get("channel"); v != "" {
		f.Channel = notification.Channel(v)
		if !f.Channel.Valid() {
			return f, badRequest("invalid_query", "channel must be one of sms, email, push")
		}
	}

	if v := q.Get("scheduled"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil || (v != "true" && v != "false") {
			return f, badRequest("invalid_query", "scheduled must be true or false")
		}
		f.Scheduled = &b
	}

	var err error
	if f.From, err = parseTime(q, "from"); err != nil {
		return f, err
	}
	if f.To, err = parseTime(q, "to"); err != nil {
		return f, err
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return f, badRequest("invalid_query", "from must be earlier than to")
	}

	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			return f, badRequest("invalid_query", "limit must be an integer from 1 to %d", maxPageSize)
		}
		f.Limit = n
	}

	if v := q.Get("cursor"); v != "" {
		c, err := decodeCursor(v)
		if err != nil {
			return f, err
		}
		f.After = &c
	}
	return f, nil
}

func parseTime(q url.Values, name string) (time.Time, error) {
	v := q.Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, badRequest("invalid_query", "%s must be an RFC 3339 timestamp, for example 2026-11-05T10:00:00Z", name)
	}
	return t.UTC(), nil
}

// A cursor is "<created_at>|<id>" in URL-safe base64. Clients must treat it as opaque;
// encoding it keeps them from building cursors by hand and lets the format change.
func encodeCursor(c store.Cursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (store.Cursor, error) {
	invalid := badRequest("invalid_cursor", "cursor is not valid; use the nextCursor value from a previous page")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.Cursor{}, invalid
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return store.Cursor{}, invalid
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return store.Cursor{}, invalid
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return store.Cursor{}, invalid
	}
	return store.Cursor{CreatedAt: t, ID: u}, nil
}

package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

func sampleNotification(status notification.Status) notification.Notification {
	n := pendingFrom(notification.Request{
		Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "hi", Priority: notification.PriorityHigh,
	}, nil)
	n.Status = status
	return n
}

func TestGetNotification(t *testing.T) {
	pending := sampleNotification(notification.StatusPending)
	sent := sampleNotification(notification.StatusSent)
	sentAt := fixedTime.Add(time.Second)
	sent.SentAt, sent.ProviderMessageID, sent.Attempts = &sentAt, "msg-1", 1

	s := newTestServer(t)
	s.store.getFn = func(id uuid.UUID) (notification.Notification, error) {
		switch id {
		case pending.ID:
			return pending, nil
		case sent.ID:
			return sent, nil
		}
		return notification.Notification{}, store.ErrNotFound
	}

	t.Run("pending", func(t *testing.T) {
		rec := s.do(http.MethodGet, "/v1/notifications/"+pending.ID.String(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		body := decodeBody(t, rec)
		if body["id"] != pending.ID.String() || body["status"] != "pending" || body["priority"] != "high" ||
			body["createdAt"] != "2026-11-05T10:00:00Z" || body["nextAttemptAt"] == nil {
			t.Errorf("unexpected body: %v", body)
		}
		if _, ok := body["sentAt"]; ok {
			t.Error("sentAt present on a pending notification")
		}
	})

	t.Run("sent", func(t *testing.T) {
		body := decodeBody(t, s.do(http.MethodGet, "/v1/notifications/"+sent.ID.String(), ""))
		if body["providerMessageId"] != "msg-1" || body["sentAt"] != "2026-11-05T10:00:01Z" || body["attempts"] != float64(1) {
			t.Errorf("unexpected body: %v", body)
		}
		if _, ok := body["nextAttemptAt"]; ok {
			t.Error("nextAttemptAt present on a sent notification")
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		expectError(t, s.do(http.MethodGet, "/v1/notifications/"+uuid.NewString(), ""), http.StatusNotFound, "not_found")
	})
	t.Run("malformed id", func(t *testing.T) {
		expectError(t, s.do(http.MethodGet, "/v1/notifications/not-a-uuid", ""), http.StatusBadRequest, "invalid_id")
	})
}

func TestGetBatch(t *testing.T) {
	batchID := uuid.Must(uuid.NewV7())
	items := []notification.Notification{
		sampleNotification(notification.StatusSent),
		sampleNotification(notification.StatusPending),
		sampleNotification(notification.StatusPending),
	}
	s := newTestServer(t)
	s.store.getBatchFn = func(id uuid.UUID) ([]notification.Notification, error) {
		if id != batchID {
			return nil, store.ErrNotFound
		}
		return items, nil
	}

	rec := s.do(http.MethodGet, "/v1/batches/"+batchID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	counts := body["counts"].(map[string]any)
	want := map[string]float64{"pending": 2, "sent": 1, "processing": 0, "failed": 0, "cancelled": 0}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%s] = %v, want %v", k, counts[k], v)
		}
	}
	if body["total"] != float64(3) || len(body["notifications"].([]any)) != 3 || body["batchId"] != batchID.String() {
		t.Errorf("unexpected body: %v", body)
	}

	expectError(t, s.do(http.MethodGet, "/v1/batches/"+uuid.NewString(), ""), http.StatusNotFound, "not_found")
	expectError(t, s.do(http.MethodGet, "/v1/batches/123", ""), http.StatusBadRequest, "invalid_id")
}

func TestCancelNotification(t *testing.T) {
	cancellable, sent := uuid.New(), uuid.New()
	s := newTestServer(t)
	s.store.cancelFn = func(id uuid.UUID) (notification.Notification, error) {
		switch id {
		case cancellable:
			n := sampleNotification(notification.StatusCancelled)
			n.ID = id
			return n, nil
		case sent:
			return notification.Notification{}, &store.NotCancellableError{Status: notification.StatusSent}
		}
		return notification.Notification{}, store.ErrNotFound
	}

	rec := s.do(http.MethodPost, "/v1/notifications/"+cancellable.String()+"/cancel", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["status"] != "cancelled" {
		t.Errorf("cancel pending: status %d, body %s", rec.Code, rec.Body.String())
	}

	body := expectError(t, s.do(http.MethodPost, "/v1/notifications/"+sent.String()+"/cancel", ""),
		http.StatusConflict, "not_cancellable")
	if body["currentStatus"] != "sent" {
		t.Errorf("currentStatus = %v, want sent", body["currentStatus"])
	}

	expectError(t, s.do(http.MethodPost, "/v1/notifications/"+uuid.NewString()+"/cancel", ""), http.StatusNotFound, "not_found")
	expectError(t, s.do(http.MethodPost, "/v1/notifications/x/cancel", ""), http.StatusBadRequest, "invalid_id")

	if rec := s.do(http.MethodGet, "/v1/notifications/"+cancellable.String()+"/cancel", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on cancel: status %d, want 405", rec.Code)
	}
}

func TestListNotifications(t *testing.T) {
	t.Run("defaults and empty page", func(t *testing.T) {
		s := newTestServer(t)
		rec := s.do(http.MethodGet, "/v1/notifications", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Body.String(); got != "{\"items\":[],\"nextCursor\":null}\n" {
			t.Errorf("body = %q, want empty items and null cursor", got)
		}
		if f := s.store.listCalls[0]; f.Limit != defaultPageSize || f.Status != "" || f.Channel != "" || f.After != nil {
			t.Errorf("unexpected default filter: %+v", f)
		}
	})

	t.Run("all filters are parsed", func(t *testing.T) {
		s := newTestServer(t)
		q := url.Values{
			"status": {"failed"}, "channel": {"email"}, "limit": {"10"},
			"from": {"2026-11-01T00:00:00Z"}, "to": {"2026-11-02T00:00:00+03:00"},
		}
		if rec := s.do(http.MethodGet, "/v1/notifications?"+q.Encode(), ""); rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
		}
		f := s.store.listCalls[0]
		if f.Status != notification.StatusFailed || f.Channel != notification.ChannelEmail || f.Limit != 10 {
			t.Errorf("unexpected filter: %+v", f)
		}
		if !f.From.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !f.To.Equal(time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)) {
			t.Errorf("from/to = %v / %v", f.From, f.To)
		}
	})

	t.Run("cursor round trip", func(t *testing.T) {
		s := newTestServer(t)
		next := store.Cursor{CreatedAt: time.Date(2026, 11, 5, 10, 0, 0, 123456000, time.UTC), ID: uuid.Must(uuid.NewV7())}
		s.store.listFn = func(f store.ListFilter) (store.ListPage, error) {
			if f.After == nil {
				return store.ListPage{Items: []notification.Notification{sampleNotification(notification.StatusPending)}, Next: &next}, nil
			}
			return store.ListPage{}, nil
		}

		body := decodeBody(t, s.do(http.MethodGet, "/v1/notifications?limit=1", ""))
		cursor, ok := body["nextCursor"].(string)
		if !ok || cursor == "" || len(body["items"].([]any)) != 1 {
			t.Fatalf("unexpected first page: %v", body)
		}
		s.do(http.MethodGet, "/v1/notifications?limit=1&cursor="+url.QueryEscape(cursor), "")
		got := s.store.listCalls[1].After
		if got == nil || !got.CreatedAt.Equal(next.CreatedAt) || got.ID != next.ID {
			t.Errorf("cursor decoded to %+v, want %+v", got, next)
		}
	})

	bad := map[string]string{
		"unknown parameter (typo)": "stauts=failed",
		"repeated parameter":       "status=sent&status=failed",
		"unknown status":           "status=lost",
		"unknown channel":          "channel=fax",
		"limit zero":               "limit=0",
		"limit over max":           "limit=201",
		"limit not a number":       "limit=ten",
		"bad from":                 "from=yesterday",
		"date without time":        "to=2026-11-05",
		"from not before to":       "from=2026-11-05T00:00:00Z&to=2026-11-05T00:00:00Z",
	}
	for name, query := range bad {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t)
			expectError(t, s.do(http.MethodGet, "/v1/notifications?"+query, ""), http.StatusBadRequest, "invalid_query")
			if len(s.store.listCalls) != 0 {
				t.Error("store called with an invalid query")
			}
		})
	}

	for name, cursor := range map[string]string{
		"not base64":     "%%%",
		"no separator":   encodeRaw("2026-11-05T10:00:00Z"),
		"bad timestamp":  encodeRaw("yesterday|" + uuid.NewString()),
		"bad identifier": encodeRaw("2026-11-05T10:00:00Z|nope"),
	} {
		t.Run("cursor "+name, func(t *testing.T) {
			s := newTestServer(t)
			expectError(t, s.do(http.MethodGet, "/v1/notifications?cursor="+url.QueryEscape(cursor), ""),
				http.StatusBadRequest, "invalid_cursor")
		})
	}
}

// encodeRaw builds a cursor-shaped string from arbitrary content, to test rejection.
func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

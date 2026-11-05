package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

const validSMS = `{"channel":"sms","recipient":"+905551234567","content":"Your code is 1234","priority":"high"}`

func TestCreateNotification(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(http.MethodPost, "/v1/notifications",
		`{"channel":"sms","recipient":"  +905551234567 ","content":"hi"}`, "X-Request-ID", "req-42")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["status"] != "pending" || body["priority"] != "normal" || body["recipient"] != "+905551234567" {
		t.Errorf("unexpected body: %v", body)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/notifications/"+body["id"].(string) {
		t.Errorf("Location = %q", loc)
	}
	if rec.Header().Get(replayedHeader) != "" {
		t.Error("new resource marked as replayed")
	}

	if len(s.store.createCalls) != 1 {
		t.Fatalf("store called %d times, want 1", len(s.store.createCalls))
	}
	p := s.store.createCalls[0]
	if p.Batch || p.MaxAttempts != 8 || p.CorrelationID != "req-42" || p.IdempotencyKey != "" || p.RequestHash != nil {
		t.Errorf("unexpected params: %+v", p)
	}
	if got := p.Requests[0]; got.Recipient != "+905551234567" || got.Priority != notification.PriorityNormal {
		t.Errorf("request not normalized: %+v", got)
	}
}

func TestCreateNotificationBadInput(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"empty body", "", http.StatusBadRequest, "invalid_json"},
		{"malformed json", `{"channel":`, http.StatusBadRequest, "invalid_json"},
		{"not an object", `[1,2]`, http.StatusBadRequest, "invalid_json"},
		{"wrong field type", `{"channel":"sms","recipient":"+905551234567","content":7}`, http.StatusBadRequest, "invalid_json"},
		{"unknown field (typo)", `{"channel":"sms","recipient":"+905551234567","content":"hi","priorty":"high"}`, http.StatusBadRequest, "unknown_field"},
		{"two objects", validSMS + validSMS, http.StatusBadRequest, "invalid_json"},
		{"trailing garbage", validSMS + " x", http.StatusBadRequest, "invalid_json"},
		{"too large", `{"channel":"sms","content":"` + strings.Repeat("a", maxSingleBody) + `"}`, http.StatusRequestEntityTooLarge, "request_too_large"},
		{"validation", `{"channel":"fax","recipient":"","content":" "}`, http.StatusUnprocessableEntity, "validation_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			expectError(t, s.do(http.MethodPost, "/v1/notifications", tt.body), tt.status, tt.code)
			if len(s.store.createCalls) != 0 {
				t.Error("store called for an invalid request")
			}
		})
	}
}

func TestCreateNotificationValidationDetails(t *testing.T) {
	s := newTestServer(t)
	body := expectError(t, s.do(http.MethodPost, "/v1/notifications",
		`{"channel":"sms","recipient":"12345","content":"`+strings.Repeat("a", 161)+`"}`),
		http.StatusUnprocessableEntity, "validation_failed")

	details := body["details"].([]any)
	fields := map[string]bool{}
	for _, d := range details {
		m := d.(map[string]any)
		fields[m["field"].(string)] = true
		if _, ok := m["index"]; ok {
			t.Error("single request error carries a batch index")
		}
	}
	if !fields["recipient"] || !fields["content"] || len(fields) != 2 {
		t.Errorf("details = %v, want recipient and content", details)
	}
}

func TestCreateNotificationIdempotency(t *testing.T) {
	t.Run("key and hash are passed to the store", func(t *testing.T) {
		s := newTestServer(t)
		s.do(http.MethodPost, "/v1/notifications", validSMS, idempotencyHeader, "k1")
		// Same request, different formatting and field order: must hash the same.
		s.do(http.MethodPost, "/v1/notifications",
			`{ "priority":"high", "content":"Your code is 1234", "recipient":"+905551234567", "channel":"sms" }`,
			idempotencyHeader, "k1")
		// Different content: must hash differently.
		s.do(http.MethodPost, "/v1/notifications", strings.Replace(validSMS, "1234", "9999", 1), idempotencyHeader, "k1")

		c := s.store.createCalls
		if len(c) != 3 || c[0].IdempotencyKey != "k1" || len(c[0].RequestHash) != 32 {
			t.Fatalf("unexpected calls: %+v", c)
		}
		if !bytes.Equal(c[0].RequestHash, c[1].RequestHash) {
			t.Error("reformatted request hashed differently")
		}
		if bytes.Equal(c[0].RequestHash, c[2].RequestHash) {
			t.Error("different content hashed the same")
		}
	})

	t.Run("replay returns 200 with a header", func(t *testing.T) {
		s := newTestServer(t)
		s.store.createFn = func(p store.CreateParams) (store.CreateResult, error) {
			n := pendingFrom(p.Requests[0], nil)
			n.Status = notification.StatusSent
			return store.CreateResult{Notifications: []notification.Notification{n}, Replayed: true}, nil
		}
		rec := s.do(http.MethodPost, "/v1/notifications", validSMS, idempotencyHeader, "k1")
		if rec.Code != http.StatusOK || rec.Header().Get(replayedHeader) != "true" {
			t.Fatalf("status = %d, replayed header = %q", rec.Code, rec.Header().Get(replayedHeader))
		}
		if body := decodeBody(t, rec); body["status"] != "sent" {
			t.Errorf("replay shows %v, want the current status", body["status"])
		}
	})

	t.Run("conflict returns 409", func(t *testing.T) {
		s := newTestServer(t)
		s.store.createFn = func(store.CreateParams) (store.CreateResult, error) {
			return store.CreateResult{}, store.ErrIdempotencyConflict
		}
		expectError(t, s.do(http.MethodPost, "/v1/notifications", validSMS, idempotencyHeader, "k1"),
			http.StatusConflict, "idempotency_key_conflict")
	})

	for name, key := range map[string]string{
		"too long":     strings.Repeat("k", maxIdempotencyKey+1),
		"blank":        "   ",
		"non-ascii":    "schlüssel",
		"control char": "a\tb",
	} {
		t.Run("invalid key "+name, func(t *testing.T) {
			s := newTestServer(t)
			expectError(t, s.do(http.MethodPost, "/v1/notifications", validSMS, idempotencyHeader, key),
				http.StatusBadRequest, "invalid_idempotency_key")
		})
	}
}

func TestStoreFailureIsA500WithoutDetails(t *testing.T) {
	s := newTestServer(t)
	s.store.createFn = func(store.CreateParams) (store.CreateResult, error) {
		return store.CreateResult{}, errors.New("connection refused to db:5432")
	}
	rec := s.do(http.MethodPost, "/v1/notifications", validSMS)
	expectError(t, rec, http.StatusInternalServerError, "internal_error")
	if strings.Contains(rec.Body.String(), "db:5432") {
		t.Error("internal error details leaked to the client")
	}
	if !strings.Contains(s.logs.String(), "connection refused to db:5432") {
		t.Error("internal error not logged")
	}
}

func batchBody(items ...string) string {
	return `{"notifications":[` + strings.Join(items, ",") + `]}`
}

func TestCreateBatch(t *testing.T) {
	s := newTestServer(t)
	email := `{"channel":"email","recipient":"a@example.com","content":"Hello"}`

	rec := s.do(http.MethodPost, "/v1/batches", batchBody(validSMS, email))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	items := body["notifications"].([]any)
	if body["count"] != float64(2) || len(items) != 2 || body["batchId"] == "" {
		t.Fatalf("unexpected body: %v", body)
	}
	if items[0].(map[string]any)["status"] != "pending" {
		t.Errorf("item 0 = %v", items[0])
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/batches/"+body["batchId"].(string) {
		t.Errorf("Location = %q", loc)
	}
	if p := s.store.createCalls[0]; !p.Batch || len(p.Requests) != 2 {
		t.Errorf("unexpected params: %+v", p)
	}
}

func TestCreateBatchSizeLimits(t *testing.T) {
	for _, n := range []int{0, maxBatchItems + 1} {
		t.Run(fmt.Sprintf("%d items", n), func(t *testing.T) {
			s := newTestServer(t)
			items := make([]string, n)
			for i := range items {
				items[i] = validSMS
			}
			body := expectError(t, s.do(http.MethodPost, "/v1/batches", batchBody(items...)),
				http.StatusUnprocessableEntity, "validation_failed")
			if d := body["details"].([]any)[0].(map[string]any); d["field"] != "notifications" {
				t.Errorf("detail = %v", d)
			}
		})
	}

	t.Run("1000 items accepted", func(t *testing.T) {
		s := newTestServer(t)
		items := make([]string, maxBatchItems)
		for i := range items {
			items[i] = validSMS
		}
		if rec := s.do(http.MethodPost, "/v1/batches", batchBody(items...)); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d; body: %.300s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing notifications field", func(t *testing.T) {
		s := newTestServer(t)
		expectError(t, s.do(http.MethodPost, "/v1/batches", `{}`), http.StatusUnprocessableEntity, "validation_failed")
	})
}

func TestCreateBatchIsAllOrNothing(t *testing.T) {
	s := newTestServer(t)
	bad := `{"channel":"sms","recipient":"nope","content":"hi"}`

	body := expectError(t, s.do(http.MethodPost, "/v1/batches", batchBody(validSMS, bad, validSMS, bad)),
		http.StatusUnprocessableEntity, "validation_failed")

	var indexes []float64
	for _, d := range body["details"].([]any) {
		indexes = append(indexes, d.(map[string]any)["index"].(float64))
	}
	if fmt.Sprint(indexes) != "[1 3]" {
		t.Errorf("invalid indexes = %v, want [1 3]", indexes)
	}
	if len(s.store.createCalls) != 0 {
		t.Error("store called although the batch had invalid items")
	}
}

func TestBatchAndSingleHashDiffer(t *testing.T) {
	s := newTestServer(t)
	s.do(http.MethodPost, "/v1/notifications", validSMS, idempotencyHeader, "k")
	s.do(http.MethodPost, "/v1/batches", batchBody(validSMS), idempotencyHeader, "k")
	c := s.store.createCalls
	if len(c) != 2 || bytes.Equal(c[0].RequestHash, c[1].RequestHash) {
		t.Error("a single create and a one-item batch must not share a request hash")
	}
}

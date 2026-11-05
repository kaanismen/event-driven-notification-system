package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
)

func testMessage() Message {
	return Message{
		ID: uuid.New(), Channel: notification.ChannelSMS,
		Recipient: "+905551234567", Content: "Your code is 1234", CorrelationID: "req-7",
	}
}

func TestSendSuccess(t *testing.T) {
	m := testMessage()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("method %s, content type %q", r.Method, r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Idempotency-Key") != m.ID.String() || r.Header.Get("X-Request-ID") != "req-7" {
			t.Errorf("headers: idempotency %q, request id %q", r.Header.Get("Idempotency-Key"), r.Header.Get("X-Request-ID"))
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		want := map[string]string{"to": m.Recipient, "channel": "sms", "content": m.Content}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("body[%s] = %q, want %q", k, body[k], v)
			}
		}
		if len(body) != len(want) {
			t.Errorf("body has extra fields: %v", body)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"messageId":"abc-123","status":"accepted","timestamp":"2026-11-05T10:00:00Z"}`))
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL, time.Second).Send(context.Background(), m)
	if err != nil || res.MessageID != "abc-123" {
		t.Fatalf("Send = %+v, %v; want messageId abc-123", res, err)
	}
}

func TestSendAcceptsAny2xx(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		code       int
	}{
		{"200 without JSON (webhook.site default page)", "This URL has no default content configured.", http.StatusOK},
		{"204 no content", "", http.StatusNoContent},
		{"202 with JSON but no messageId", `{"status":"accepted"}`, http.StatusAccepted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			res, err := NewClient(srv.URL, time.Second).Send(context.Background(), testMessage())
			if err != nil || res.MessageID != "" {
				t.Errorf("Send = %+v, %v; want success without an ID", res, err)
			}
		})
	}
}

func TestSendNon2xxIsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(strings.Repeat("x", 500)))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, time.Second).Send(context.Background(), testMessage())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want StatusError 503", err)
	}
	if n := len([]rune(se.Body)); n > maxErrorExcerpt+3 {
		t.Errorf("error body excerpt has %d characters, want at most %d", n, maxErrorExcerpt+3)
	}
}

func TestSendTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	start := time.Now()
	_, err := NewClient(srv.URL, 100*time.Millisecond).Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send succeeded, want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded in the chain", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
}

// TestErrorsDoNotLeakTheURL matters because a webhook.site URL is a secret, and errors are
// stored in last_error, which the API returns to clients.
func TestErrorsDoNotLeakTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	secretURL := srv.URL + "/secret-token-123"
	srv.Close() // connection refused from now on

	_, err := NewClient(secretURL, time.Second).Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send succeeded against a closed server")
	}
	if strings.Contains(err.Error(), "secret-token-123") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks the provider URL: %v", err)
	}
}

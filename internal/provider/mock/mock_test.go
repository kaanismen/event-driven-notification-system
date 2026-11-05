package mock_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/provider"
	"github.com/kaanismen/insider-assessment/internal/provider/mock"
)

// The mock must speak the same contract the real client expects.
func TestMockWithRealClient(t *testing.T) {
	m := mock.NewRecorder(mock.Options{})
	srv := httptest.NewServer(m)
	defer srv.Close()

	msg := provider.Message{
		ID: uuid.New(), Channel: notification.ChannelEmail,
		Recipient: "a@example.com", Content: "Hello", CorrelationID: "req-1",
	}
	res, err := provider.NewClient(srv.URL+"/any/path", time.Second).Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := m.Requests()
	if len(got) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(got))
	}
	r := got[0]
	if r.To != msg.Recipient || r.Channel != "email" || r.Content != "Hello" ||
		r.IdempotencyKey != msg.ID.String() || r.RequestID != "req-1" {
		t.Errorf("unexpected recorded request: %+v", r)
	}
	if res.MessageID == "" || res.MessageID != r.MessageID {
		t.Errorf("client got messageId %q, mock answered %q", res.MessageID, r.MessageID)
	}
}

func TestMockRejectsBadRequests(t *testing.T) {
	m := mock.NewRecorder(mock.Options{})
	for _, tt := range []struct {
		method, body string
		want         int
	}{
		{http.MethodGet, "", http.StatusMethodNotAllowed},
		{http.MethodPost, "not json", http.StatusBadRequest},
		{http.MethodPost, `{"content":"no recipient"}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest(tt.method, "/", strings.NewReader(tt.body)))
		if rec.Code != tt.want {
			t.Errorf("%s %q: status %d, want %d", tt.method, tt.body, rec.Code, tt.want)
		}
	}
	if n := len(m.Requests()); n != 0 {
		t.Errorf("recorded %d rejected requests", n)
	}
}

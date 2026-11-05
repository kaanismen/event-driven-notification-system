// Package mock is a stand-in for the delivery provider: it accepts every valid message
// with 202 and a unique messageId, like the real contract. Tests use it to see exactly
// what was sent and when; the cmd/mockprovider binary uses it for demos and load tests
// without webhook.site quotas (A-03). Failure modes (flaky, slow, failing) come in Phase 4.
package mock

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Request is one message the mock received.
type Request struct {
	At             time.Time
	To             string
	Channel        string
	Content        string
	IdempotencyKey string
	RequestID      string
	MessageID      string // the ID the mock answered with
}

// Server is an http.Handler. It is safe for concurrent use.
type Server struct {
	logger *slog.Logger
	record bool

	mu       sync.Mutex
	requests []Request
}

// NewRecorder returns a mock that keeps every request in memory, for tests.
func NewRecorder() *Server {
	return &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), record: true}
}

// NewLogging returns a mock that logs each request instead of keeping it, so a long load
// test does not grow memory without bound.
func NewLogging(logger *slog.Logger) *Server {
	return &Server{logger: logger}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "only POST is supported", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		To      string `json:"to"`
		Channel string `json:"channel"`
		Content string `json:"content"`
	}
	// A real provider would reject a malformed body too; answering 400 makes client bugs
	// visible in tests instead of hiding them behind a 202.
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.To == "" || body.Channel == "" {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	req := Request{
		At:             time.Now(),
		To:             body.To,
		Channel:        body.Channel,
		Content:        body.Content,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		RequestID:      r.Header.Get("X-Request-ID"),
		MessageID:      uuid.NewString(),
	}
	if s.record {
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()
	}
	s.logger.Info("message accepted",
		"channel", req.Channel, "message_id", req.MessageID,
		"idempotency_key", req.IdempotencyKey, "request_id", req.RequestID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"messageId": req.MessageID,
		"status":    "accepted",
		"timestamp": req.At.UTC().Format(time.RFC3339Nano),
	})
}

// Requests returns a copy of every recorded request, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

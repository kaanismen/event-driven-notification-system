// Package mock is a stand-in for the delivery provider: it accepts every valid message
// with 202 and a unique messageId, like the real contract. Tests use it to see exactly
// what was sent and when; the cmd/mockprovider binary uses it for demos and load tests
// without webhook.site quotas (A-03). Options inject failures (flaky, failing, slow,
// rate limited) for the reliability tests and demos (ADR-0014).
package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Options inject failures. The zero value accepts every valid message.
type Options struct {
	// FailFirst makes the first FailFirst attempts of every message fail, counted per
	// Idempotency-Key, after which the message is accepted. It is deterministic, which
	// is what tests need: with FailFirst below the retry budget every message ends up sent.
	FailFirst int
	// FailureRate is the share of requests, from 0 to 1, that fail at random. 1 means
	// the provider is down. For demos; tests prefer FailFirst.
	FailureRate float64
	// FailureStatus is the HTTP status of an injected failure; 0 means 503.
	FailureStatus int
	// RetryAfter, when positive, is sent with every injected failure as a Retry-After
	// header in whole seconds (rounded up).
	RetryAfter time.Duration
	// Latency delays every answer, to simulate a slow provider. The wait ends early
	// when the client gives up, as a real server notices a closed connection.
	Latency time.Duration
}

// Request is one message the mock received.
type Request struct {
	Status         int // the HTTP status the mock answered with (or would have, had the client waited)
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
	opts   Options

	mu       sync.Mutex
	requests []Request
	attempts map[string]int // Idempotency-Key -> requests seen, only when FailFirst > 0
}

// NewRecorder returns a mock that keeps every request in memory, for tests.
func NewRecorder(opts Options) *Server {
	return newServer(slog.New(slog.DiscardHandler), true, opts)
}

// NewLogging returns a mock that logs each request instead of keeping it, so a long load
// test does not grow memory without bound. With FailFirst set it still keeps one small
// counter per message, which is fine for demos.
func NewLogging(logger *slog.Logger, opts Options) *Server {
	return newServer(logger, false, opts)
}

func newServer(logger *slog.Logger, record bool, opts Options) *Server {
	if opts.FailureStatus == 0 {
		opts.FailureStatus = http.StatusServiceUnavailable
	}
	return &Server{logger: logger, record: record, opts: opts, attempts: map[string]int{}}
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
		Status:         http.StatusAccepted,
		At:             time.Now(),
		To:             body.To,
		Channel:        body.Channel,
		Content:        body.Content,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		RequestID:      r.Header.Get("X-Request-ID"),
	}

	s.mu.Lock()
	fail := s.opts.FailureRate > 0 && rand.Float64() < s.opts.FailureRate //nolint:gosec // G404: a test double, nothing to predict
	if s.opts.FailFirst > 0 {
		s.attempts[req.IdempotencyKey]++
		fail = fail || s.attempts[req.IdempotencyKey] <= s.opts.FailFirst
	}
	if fail {
		req.Status = s.opts.FailureStatus
	} else {
		req.MessageID = uuid.NewString()
	}
	if s.record {
		s.requests = append(s.requests, req)
	}
	s.mu.Unlock()

	if s.opts.Latency > 0 {
		t := time.NewTimer(s.opts.Latency)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			s.logger.Info("client gave up before the answer", "idempotency_key", req.IdempotencyKey)
			return
		}
	}

	if fail {
		s.logger.Info("injected failure", "status", req.Status, "channel", req.Channel,
			"idempotency_key", req.IdempotencyKey, "request_id", req.RequestID)
		if s.opts.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(s.opts.RetryAfter.Seconds()))))
		}
		http.Error(w, `{"error":"injected failure"}`, req.Status)
		return
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

// ParseOptions reads Options from MOCK_* environment variables, for the mock binary:
// MOCK_FAIL_FIRST, MOCK_FAILURE_RATE, MOCK_FAILURE_STATUS, MOCK_RETRY_AFTER (a Go
// duration) and MOCK_LATENCY (a Go duration). Unset variables keep the zero value.
func ParseOptions(getenv func(string) string) (Options, error) {
	var o Options
	var err error
	if v := getenv("MOCK_FAIL_FIRST"); v != "" {
		if o.FailFirst, err = strconv.Atoi(v); err != nil || o.FailFirst < 0 {
			return Options{}, fmt.Errorf("MOCK_FAIL_FIRST must be a whole number of at least 0, got %q", v)
		}
	}
	if v := getenv("MOCK_FAILURE_RATE"); v != "" {
		if o.FailureRate, err = strconv.ParseFloat(v, 64); err != nil || o.FailureRate < 0 || o.FailureRate > 1 {
			return Options{}, fmt.Errorf("MOCK_FAILURE_RATE must be a number from 0 to 1, got %q", v)
		}
	}
	if v := getenv("MOCK_FAILURE_STATUS"); v != "" {
		if o.FailureStatus, err = strconv.Atoi(v); err != nil || o.FailureStatus < 400 || o.FailureStatus > 599 {
			return Options{}, fmt.Errorf("MOCK_FAILURE_STATUS must be an HTTP status from 400 to 599, got %q", v)
		}
	}
	if v := getenv("MOCK_RETRY_AFTER"); v != "" {
		if o.RetryAfter, err = time.ParseDuration(v); err != nil || o.RetryAfter < 0 {
			return Options{}, fmt.Errorf("MOCK_RETRY_AFTER must be a duration such as 2s, got %q", v)
		}
	}
	if v := getenv("MOCK_LATENCY"); v != "" {
		if o.Latency, err = time.ParseDuration(v); err != nil || o.Latency < 0 {
			return Options{}, fmt.Errorf("MOCK_LATENCY must be a duration such as 500ms, got %q", v)
		}
	}
	return o, nil
}

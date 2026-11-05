package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/logging"
)

func newTestHandler(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return NewHandler(testDeps(logging.New(&logs, slog.LevelDebug), &fakeStore{})), &logs
}

func TestHealth(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Errorf("body = %q, err = %v", rec.Body.String(), err)
	}
}

func TestHealthRejectsOtherMethods(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		wantSame bool
	}{
		{"valid id is kept", "abc-123_DEF.9", true},
		{"missing id is generated", "", false},
		{"unsafe characters are replaced", "bad id\n{\"x\":1}", false},
		{"overlong id is replaced", strings.Repeat("a", maxRequestIDBytes+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, logs := newTestHandler(t)
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			if tt.incoming != "" {
				req.Header.Set(requestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(requestIDHeader)
			if tt.wantSame {
				if got != tt.incoming {
					t.Errorf("echoed id = %q, want %q", got, tt.incoming)
				}
			} else if _, err := uuid.Parse(got); err != nil {
				t.Errorf("generated id %q is not a UUID", got)
			}

			var line map[string]any
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
				t.Fatalf("access log is not one JSON line: %v\n%s", err, logs.String())
			}
			if line["request_id"] != got {
				t.Errorf("access log request_id = %v, want %q", line["request_id"], got)
			}
			if line["status"] != float64(http.StatusOK) || line["path"] != "/health" {
				t.Errorf("unexpected access log line: %v", line)
			}
		})
	}
}

func TestRecoverPanicReturns500AndLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := logging.New(&logs, slog.LevelDebug)
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := chain(boom, requestID, accessLog(logger), recoverPanic(logger))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %q", rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, `"msg":"panic in handler"`) || !strings.Contains(out, `"status":500`) {
		t.Errorf("logs missing panic or 500 access line:\n%s", out)
	}
}

// testDeps returns handler dependencies with an always-ready check and a stub metrics
// endpoint; tests override fields as needed.
func testDeps(logger *slog.Logger, st Store) Deps {
	return Deps{
		Logger:      logger,
		Store:       st,
		MaxAttempts: 8,
		Ready:       func(context.Context) error { return nil },
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("stub_metric 1\n"))
		}),
	}
}

func TestLivenessAndHealthAlias(t *testing.T) {
	h, logs := newTestHandler(t)
	for _, path := range []string{"/livez", "/health"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
			t.Errorf("GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
	// Probes are frequent, so a successful one is logged at DEBUG, not INFO.
	if !strings.Contains(logs.String(), `"level":"DEBUG","msg":"http request"`) ||
		strings.Contains(logs.String(), `"level":"INFO","msg":"http request"`) {
		t.Errorf("probe requests should be logged at DEBUG only:\n%s", logs.String())
	}
}

func TestReadiness(t *testing.T) {
	var logs bytes.Buffer
	readyErr := errors.New("shutting down")
	var current error
	d := testDeps(logging.New(&logs, slog.LevelDebug), &fakeStore{})
	d.Ready = func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("readiness check has no deadline")
		}
		return current
	}
	h := NewHandler(d)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("ready: status %d, want 200", rec.Code)
	}

	current = readyErr
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "shutting down") {
		t.Errorf("not ready: %d %s, want 503 with the reason", rec.Code, rec.Body.String())
	}
	// Liveness is unaffected by readiness.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("livez while not ready: %d, want 200", rec.Code)
	}
}

func TestMetricsRoute(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "stub_metric 1\n" {
		t.Errorf("GET /metrics = %d %q", rec.Code, rec.Body.String())
	}
}

// The access log names the matched route, so lines group by endpoint rather than by
// paths full of IDs.
func TestAccessLogRecordsTheRoute(t *testing.T) {
	h, logs := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/notifications/"+uuid.NewString(), nil))
	if !strings.Contains(logs.String(), `"route":"GET /v1/notifications/{id}"`) {
		t.Errorf("access log lacks the route pattern:\n%s", logs.String())
	}
}

package httpapi

import (
	"bytes"
	"encoding/json"
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
	return NewHandler(logging.New(&logs, slog.LevelDebug), &fakeStore{}, 8), &logs
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

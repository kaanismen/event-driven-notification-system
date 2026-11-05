package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRetryable is the classification table of ADR-0002 (DR-02).
func TestRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"bad request", &StatusError{Code: 400}, false},
		{"unauthorized", &StatusError{Code: 401}, false},
		{"not found", &StatusError{Code: 404}, false},
		{"unprocessable", &StatusError{Code: 422}, false},
		{"unexpected redirect", &StatusError{Code: 304}, false},
		{"request timeout", &StatusError{Code: 408}, true},
		{"too many requests", &StatusError{Code: 429}, true},
		{"internal error", &StatusError{Code: 500}, true},
		{"bad gateway", &StatusError{Code: 502}, true},
		{"unavailable", &StatusError{Code: 503}, true},
		{"timeout", fmt.Errorf("send to provider: %w", context.DeadlineExceeded), true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), true},
		{"wrapped status", fmt.Errorf("deliver: %w", &StatusError{Code: 400}), false},
	}
	for _, tt := range tests {
		if got := Retryable(tt.err); got != tt.want {
			t.Errorf("%s: Retryable(%v) = %t, want %t", tt.name, tt.err, got, tt.want)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"7", 7 * time.Second},
		{" 0 ", 0},
		{"-5", 0},
		{"soon", 0},
		{"99999999999999", 24 * time.Hour}, // clamped, no overflow
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0}, // a date in the past
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Retry-After", tt.header)
		}
		if got := retryAfter(h, now); got != tt.want {
			t.Errorf("Retry-After %q = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestSendReportsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, time.Second).Send(context.Background(), testMessage())
	if !Retryable(err) {
		t.Fatalf("429 should be retryable, err = %v", err)
	}
	if got := RetryAfter(err); got != 3*time.Second {
		t.Errorf("RetryAfter = %v, want 3s", got)
	}
	if got := RetryAfter(errors.New("network down")); got != 0 {
		t.Errorf("RetryAfter of a network error = %v, want 0", got)
	}
}

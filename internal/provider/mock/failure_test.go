package mock_test

import (
	"context"
	"errors"
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

func send(t *testing.T, url string, id uuid.UUID, timeout time.Duration) error {
	t.Helper()
	_, err := provider.NewClient(url, timeout).Send(context.Background(), provider.Message{
		ID: id, Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "hi",
	})
	return err
}

func TestFailFirstCountsPerMessage(t *testing.T) {
	m := mock.NewRecorder(mock.Options{FailFirst: 2})
	srv := httptest.NewServer(m)
	defer srv.Close()

	a, b := uuid.New(), uuid.New()
	for i, want := range []bool{false, false, true} { // attempts 1 and 2 fail, 3 succeeds
		if err := send(t, srv.URL, a, time.Second); (err == nil) != want {
			t.Errorf("message a, attempt %d: err = %v", i+1, err)
		}
	}
	// Another message has its own counter.
	var se *provider.StatusError
	if err := send(t, srv.URL, b, time.Second); !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Errorf("message b, attempt 1: err = %v, want the default 503", err)
	}

	got := m.Requests()
	if len(got) != 4 || got[0].Status != 503 || got[2].Status != 202 || got[2].MessageID == "" || got[0].MessageID != "" {
		t.Errorf("recorded requests: %+v", got)
	}
}

func TestProviderDownWithRetryAfter(t *testing.T) {
	m := mock.NewRecorder(mock.Options{FailureRate: 1, FailureStatus: 429, RetryAfter: 1500 * time.Millisecond})
	srv := httptest.NewServer(m)
	defer srv.Close()

	for range 5 {
		err := send(t, srv.URL, uuid.New(), time.Second)
		if !provider.Retryable(err) || provider.RetryAfter(err) != 2*time.Second {
			t.Fatalf("err = %v, Retry-After %v; want a retryable 429 with 2s (rounded up)", err, provider.RetryAfter(err))
		}
	}
}

func TestSlowProviderTimesOut(t *testing.T) {
	m := mock.NewRecorder(mock.Options{Latency: 5 * time.Second})
	srv := httptest.NewServer(m)
	defer srv.Close()

	start := time.Now()
	err := send(t, srv.URL, uuid.New(), 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !provider.Retryable(err) {
		t.Errorf("err = %v, want a retryable timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
	// Close waits for handlers; the latency wait must end when the client gives up.
	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("the mock kept waiting after the client gave up")
	}
}

func TestParseOptions(t *testing.T) {
	env := map[string]string{
		"MOCK_FAIL_FIRST": "2", "MOCK_FAILURE_RATE": "0.25", "MOCK_FAILURE_STATUS": "500",
		"MOCK_RETRY_AFTER": "3s", "MOCK_LATENCY": "200ms",
	}
	got, err := mock.ParseOptions(func(k string) string { return env[k] })
	want := mock.Options{FailFirst: 2, FailureRate: 0.25, FailureStatus: 500, RetryAfter: 3 * time.Second, Latency: 200 * time.Millisecond}
	if err != nil || got != want {
		t.Errorf("ParseOptions = %+v, %v; want %+v", got, err, want)
	}

	if got, err := mock.ParseOptions(func(string) string { return "" }); err != nil || got != (mock.Options{}) {
		t.Errorf("empty environment: %+v, %v; want zero options", got, err)
	}

	for name, value := range map[string]string{
		"MOCK_FAIL_FIRST": "-1", "MOCK_FAILURE_RATE": "1.5", "MOCK_FAILURE_STATUS": "200",
		"MOCK_RETRY_AFTER": "soon", "MOCK_LATENCY": "-1s",
	} {
		_, err := mock.ParseOptions(func(k string) string {
			if k == name {
				return value
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: err = %v, want an error naming the variable", name, value, err)
		}
	}
}

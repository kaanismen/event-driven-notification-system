// Package provider sends notifications to the external delivery provider over HTTP.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

// Message is one delivery attempt.
type Message struct {
	ID            uuid.UUID
	Channel       notification.Channel
	Recipient     string
	Content       string
	CorrelationID string
}

// Result is what the provider told us about an accepted message.
type Result struct {
	// MessageID is the provider's ID, or "" when a 2xx response carried none (A-02).
	MessageID string
}

// StatusError is a response outside 2xx. Retryable tells transient from permanent ones.
type StatusError struct {
	Code int
	Body string // first bytes of the response body, for last_error and logs
	// RetryAfter is the wait the provider asked for in a Retry-After header, or 0 (A-30).
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("provider returned HTTP %d", e.Code)
	}
	return fmt.Sprintf("provider returned HTTP %d: %s", e.Code, e.Body)
}

const (
	maxResponseBody = 64 << 10 // read at most 64 KiB of any response
	maxErrorExcerpt = 200      // characters of an error body kept in last_error
)

// Client posts messages to one provider URL. It is safe for concurrent use.
type Client struct {
	url     string
	timeout time.Duration
	http    *http.Client
}

// NewClient returns a client for providerURL with a per-request timeout.
func NewClient(providerURL string, timeout time.Duration) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The default keeps only 2 idle connections per host. Every sender talks to the same
	// host, so without this most requests would open a new TCP and TLS connection.
	transport.MaxIdleConnsPerHost = 256
	transport.MaxIdleConns = 256
	return &Client{url: providerURL, timeout: timeout, http: &http.Client{Transport: transport}}
}

type requestBody struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

type responseBody struct {
	MessageID string `json:"messageId"`
}

// Send delivers one message. It returns a *StatusError for non-2xx responses and a
// plain error for network failures and timeouts.
func (c *Client) Send(ctx context.Context, m Message) (Result, error) {
	payload, err := json.Marshal(requestBody{To: m.Recipient, Channel: string(m.Channel), Content: m.Content})
	if err != nil {
		return Result{}, fmt.Errorf("encode provider request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return Result{}, fmt.Errorf("build provider request: %w", stripURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "notifier/1.0")
	// A provider that supports idempotency keys can drop our retries of the same
	// notification (ADR-0004); webhook.site ignores the header.
	req.Header.Set("Idempotency-Key", m.ID.String())
	if m.CorrelationID != "" {
		req.Header.Set("X-Request-ID", m.CorrelationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("send to provider: %w", stripURL(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		// The status line arrived, so for a 2xx the provider has accepted the message.
		// Treating that as a failure would make the worker retry, which is a duplicate send.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return Result{}, nil
		}
		return Result{}, &StatusError{Code: resp.StatusCode, RetryAfter: retryAfter(resp.Header, time.Now())}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, &StatusError{
			Code: resp.StatusCode, Body: excerpt(body), RetryAfter: retryAfter(resp.Header, time.Now()),
		}
	}

	var parsed responseBody
	// A body that is not JSON (webhook.site's default page, for example) still means the
	// message was accepted; the caller logs the missing ID.
	_ = json.Unmarshal(body, &parsed)
	// Mapped field by field on purpose: the wire format and Result are separate contracts
	// that only happen to match today.
	return Result{MessageID: parsed.MessageID}, nil //nolint:staticcheck // S1016, see above

}

// Retryable reports whether a failed Send may succeed if tried again later (ADR-0002):
//
//   - 408, 429 and 5xx responses: the provider is slow, overloaded or briefly broken.
//   - Every error without a status code: connection failures and timeouts. A timeout is
//     ambiguous (the provider may have accepted the message), and we accept the possible
//     duplicate under at-least-once delivery (ADR-0005).
//
// Any other status, such as 400 or 404, says the request itself is wrong, so sending
// the same request again would fail the same way.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if !errors.As(err, &se) {
		return true
	}
	return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests ||
		(se.Code >= 500 && se.Code <= 599)
}

// RetryAfter returns the wait the provider asked for, or 0 when it did not ask.
func RetryAfter(err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}

// retryAfter parses a Retry-After header, which is either a number of seconds or an
// HTTP date. Invalid values and dates in the past mean "no preference".
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		// Clamped before converting, so a huge value cannot overflow the Duration.
		return time.Duration(min(max(secs, 0), maxRetryAfterSeconds)) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// maxRetryAfterSeconds bounds what we parse; the worker caps the wait at
// RETRY_MAX_DELAY anyway.
const maxRetryAfterSeconds = 24 * 60 * 60

// stripURL removes the request URL from net/http errors. A webhook.site URL is a secret
// token, and errors end up in last_error, which the API returns to clients.
func stripURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func excerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	if r := []rune(s); len(r) > maxErrorExcerpt {
		return string(r[:maxErrorExcerpt]) + "..."
	}
	return s
}

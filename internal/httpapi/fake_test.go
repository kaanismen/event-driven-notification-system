package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/logging"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
)

// fakeStore records calls and returns whatever the test configures. The SQL behaviour
// itself (idempotency races, pagination) is covered by the store's integration tests;
// these tests only check the HTTP mapping.
type fakeStore struct {
	mu          sync.Mutex
	createCalls []store.CreateParams
	listCalls   []store.ListFilter

	createFn   func(store.CreateParams) (store.CreateResult, error)
	getFn      func(uuid.UUID) (notification.Notification, error)
	getBatchFn func(uuid.UUID) ([]notification.Notification, error)
	cancelFn   func(uuid.UUID) (notification.Notification, error)
	listFn     func(store.ListFilter) (store.ListPage, error)
}

var fixedTime = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

// pendingFrom builds what the store would return for a freshly inserted request.
func pendingFrom(r notification.Request, batchID *uuid.UUID) notification.Notification {
	return notification.Notification{
		ID: uuid.Must(uuid.NewV7()), BatchID: batchID,
		Channel: r.Channel, Recipient: r.Recipient, Content: r.Content, Priority: r.Priority,
		Status: notification.StatusPending, MaxAttempts: 8, ScheduledAt: r.ScheduledAt,
		NextAttemptAt: fixedTime, CreatedAt: fixedTime, UpdatedAt: fixedTime,
	}
}

func (f *fakeStore) Create(_ context.Context, p store.CreateParams) (store.CreateResult, error) {
	f.mu.Lock()
	f.createCalls = append(f.createCalls, p)
	f.mu.Unlock()
	if f.createFn != nil {
		return f.createFn(p)
	}
	var res store.CreateResult
	if p.Batch {
		id := uuid.Must(uuid.NewV7())
		res.BatchID = &id
	}
	for _, r := range p.Requests {
		n := pendingFrom(r, res.BatchID)
		n.CorrelationID = p.CorrelationID
		res.Notifications = append(res.Notifications, n)
	}
	return res, nil
}

func (f *fakeStore) Get(_ context.Context, id uuid.UUID) (notification.Notification, error) {
	if f.getFn != nil {
		return f.getFn(id)
	}
	return notification.Notification{}, store.ErrNotFound
}

func (f *fakeStore) GetBatch(_ context.Context, id uuid.UUID) ([]notification.Notification, error) {
	if f.getBatchFn != nil {
		return f.getBatchFn(id)
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) Cancel(_ context.Context, id uuid.UUID) (notification.Notification, error) {
	if f.cancelFn != nil {
		return f.cancelFn(id)
	}
	return notification.Notification{}, store.ErrNotFound
}

func (f *fakeStore) List(_ context.Context, filter store.ListFilter) (store.ListPage, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, filter)
	f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn(filter)
	}
	return store.ListPage{}, nil
}

// testServer wires a handler to a fake store and captures the logs.
type testServer struct {
	t     *testing.T
	h     http.Handler
	store *fakeStore
	logs  *bytes.Buffer
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	fs := &fakeStore{}
	var logs bytes.Buffer
	return &testServer{t: t, h: NewHandler(testDeps(logging.New(&logs, slog.LevelDebug), fs)), store: fs, logs: &logs}
}

// do sends a request; headers are given as alternating name, value pairs.
func (s *testServer) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	s.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// decodeBody unmarshals a JSON response into a generic map for assertions.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not a JSON object: %v\n%s", err, rec.Body.String())
	}
	return m
}

// expectError checks the status and the machine-readable error code.
func expectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["error"] != code {
		t.Fatalf("error = %v, want %q; body: %s", body["error"], code, rec.Body.String())
	}
	return body
}

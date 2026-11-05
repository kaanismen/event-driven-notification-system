// Package httpapi exposes the HTTP API: routing, middleware, handlers and JSON helpers.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kaanismen/insider-assessment/internal/notification"
	"github.com/kaanismen/insider-assessment/internal/store"
)

// Store is what the handlers need from persistence. It is declared here, where it is
// used, so handler tests can pass a small fake instead of a database (a real boundary:
// HTTP mapping is tested separately from SQL).
type Store interface {
	Create(ctx context.Context, p store.CreateParams) (store.CreateResult, error)
	Get(ctx context.Context, id uuid.UUID) (notification.Notification, error)
	GetBatch(ctx context.Context, batchID uuid.UUID) ([]notification.Notification, error)
	Cancel(ctx context.Context, id uuid.UUID) (notification.Notification, error)
	List(ctx context.Context, f store.ListFilter) (store.ListPage, error)
}

// Deps are what the handler needs from the rest of the program.
type Deps struct {
	Logger *slog.Logger
	Store  Store
	// MaxAttempts is copied into each new notification (ADR-0002).
	MaxAttempts int
	// Ready reports whether this instance should receive traffic: the database answers
	// and the process is not shutting down. The error text is shown to the caller.
	Ready func(ctx context.Context) error
	// Metrics serves GET /metrics.
	Metrics http.Handler
}

// api holds the dependencies shared by all handlers.
type api struct {
	logger      *slog.Logger
	store       Store
	maxAttempts int
	ready       func(ctx context.Context) error
}

// NewHandler returns the root handler with every route and middleware attached.
func NewHandler(d Deps) http.Handler {
	a := &api{logger: d.Logger, store: d.Store, maxAttempts: d.MaxAttempts, ready: d.Ready}

	mux := http.NewServeMux()
	// Liveness and readiness are separate questions (ADR-0016). /health predates the
	// split and stays as an alias of /livez, so existing checks keep working.
	mux.HandleFunc("GET /livez", handleLive)
	mux.HandleFunc("GET /health", handleLive)
	mux.HandleFunc("GET /readyz", a.handleReady)
	mux.Handle("GET /metrics", d.Metrics)
	mux.HandleFunc("POST /v1/notifications", a.createNotification)
	mux.HandleFunc("GET /v1/notifications", a.listNotifications)
	mux.HandleFunc("GET /v1/notifications/{id}", a.getNotification)
	mux.HandleFunc("POST /v1/notifications/{id}/cancel", a.cancelNotification)
	mux.HandleFunc("POST /v1/batches", a.createBatch)
	mux.HandleFunc("GET /v1/batches/{id}", a.getBatch)

	// The first middleware listed runs first: the request ID must exist before anything
	// logs, and recovery sits inside the access log so a panic is logged as a 500.
	return chain(mux,
		requestID,
		accessLog(d.Logger),
		recoverPanic(d.Logger),
	)
}

// handleLive is the liveness check: it only proves the process can serve HTTP. It does
// not touch the database, so a database outage does not get the app restarted in a loop.
func handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyTimeout bounds the readiness check: a database that takes longer than this to
// answer a ping is as good as unavailable for a load balancer's purposes.
const readyTimeout = time.Second

// handleReady is the readiness check: 503 while the database is unreachable or the
// process is shutting down, so a load balancer stops sending traffic here. A failing
// readiness check takes the instance out of rotation; it does not get it restarted.
func (a *api) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := a.ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func chain(h http.Handler, middleware ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		h = middleware[i](h)
	}
	return h
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent, so an encoding error cannot change the response;
	// it can only mean the client went away.
	_ = json.NewEncoder(w).Encode(v)
}

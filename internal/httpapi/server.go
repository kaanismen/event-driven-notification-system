// Package httpapi exposes the HTTP API: routing, middleware, handlers and JSON helpers.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

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

// api holds the dependencies shared by all handlers.
type api struct {
	logger      *slog.Logger
	store       Store
	maxAttempts int
}

// NewHandler returns the root handler with every route and middleware attached.
// maxAttempts is copied into each new notification.
func NewHandler(logger *slog.Logger, st Store, maxAttempts int) http.Handler {
	a := &api{logger: logger, store: st, maxAttempts: maxAttempts}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
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
		accessLog(logger),
		recoverPanic(logger),
	)
}

// handleHealth is a liveness check: it only proves the process can serve HTTP. It does
// not touch the database, so a database outage does not get the app restarted in a loop.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
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

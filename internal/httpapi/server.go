// Package httpapi exposes the HTTP API: routing, middleware, handlers and JSON helpers.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	// Aliased because this package already has a type named api.
	openapi "github.com/kaanismen/event-driven-notification-system/api"
	"github.com/kaanismen/event-driven-notification-system/internal/events"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/store"
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

	CreateTemplate(ctx context.Context, name string, channel notification.Channel, body string) (notification.Template, bool, error)
	GetTemplate(ctx context.Context, id uuid.UUID) (notification.Template, error)
	GetTemplates(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]notification.Template, error)
	ListTemplates(ctx context.Context, f store.TemplateFilter) (store.TemplatePage, error)
}

// Deps are what the handler needs from the rest of the program.
type Deps struct {
	Logger *slog.Logger
	Store  Store
	// MaxAttempts is copied into each new notification (ADR-0002).
	MaxAttempts int
	// MaxScheduleAhead is how far in the future scheduledAt may be (ADR-0017).
	MaxScheduleAhead time.Duration
	// Ready reports whether this instance should receive traffic: the database answers
	// and the process is not shutting down. The error text is shown to the caller.
	Ready func(ctx context.Context) error
	// Metrics serves GET /metrics.
	Metrics http.Handler
	// Events delivers status changes to WebSocket subscribers (ADR-0020). It is the
	// in-memory hub, not an interface: tests use a real hub and publish to it.
	Events *events.Hub
}

// api holds the dependencies shared by all handlers.
type api struct {
	logger           *slog.Logger
	store            Store
	maxAttempts      int
	maxScheduleAhead time.Duration
	ready            func(ctx context.Context) error
	events           *events.Hub
}

// NewHandler returns the root handler with every route and middleware attached.
func NewHandler(d Deps) http.Handler {
	a := &api{
		logger: d.Logger, store: d.Store, maxAttempts: d.MaxAttempts,
		maxScheduleAhead: d.MaxScheduleAhead, ready: d.Ready, events: d.Events,
	}

	mux := http.NewServeMux()
	for _, rt := range a.routes(d.Metrics) {
		mux.Handle(rt.pattern, rt.handler)
	}

	// The first middleware listed runs first: the request ID must exist before anything
	// logs, and recovery sits inside the access log so a panic is logged as a 500.
	return chain(mux,
		requestID,
		accessLog(d.Logger),
		recoverPanic(d.Logger),
	)
}

// route is one endpoint: a Go 1.22 mux pattern ("METHOD /path/{param}") and its handler.
type route struct {
	pattern string
	handler http.Handler
}

// routes is the one list of everything the server serves. A test compares it with the
// OpenAPI spec in both directions, so an endpoint cannot be added, removed or renamed
// without the spec following.
func (a *api) routes(metrics http.Handler) []route {
	return []route{
		// Liveness and readiness are separate questions (ADR-0016). /health predates the
		// split and stays as an alias of /livez, so existing checks keep working.
		{"GET /livez", http.HandlerFunc(handleLive)},
		{"GET /health", http.HandlerFunc(handleLive)},
		{"GET /readyz", http.HandlerFunc(a.handleReady)},
		{"GET /metrics", metrics},
		{"GET /openapi.yaml", http.HandlerFunc(handleSpec)},
		{"POST /v1/notifications", http.HandlerFunc(a.createNotification)},
		{"GET /v1/notifications", http.HandlerFunc(a.listNotifications)},
		{"GET /v1/notifications/{id}", http.HandlerFunc(a.getNotification)},
		{"POST /v1/notifications/{id}/cancel", http.HandlerFunc(a.cancelNotification)},
		{"GET /v1/notifications/{id}/events", http.HandlerFunc(a.notificationEvents)},
		{"POST /v1/batches", http.HandlerFunc(a.createBatch)},
		{"GET /v1/batches/{id}", http.HandlerFunc(a.getBatch)},
		{"GET /v1/batches/{id}/events", http.HandlerFunc(a.batchEvents)},
		{"POST /v1/templates", http.HandlerFunc(a.createTemplate)},
		{"GET /v1/templates", http.HandlerFunc(a.listTemplates)},
		{"GET /v1/templates/{id}", http.HandlerFunc(a.getTemplate)},
	}
}

// handleSpec serves the OpenAPI specification embedded at build time (ADR-0018).
func handleSpec(w http.ResponseWriter, _ *http.Request) {
	// application/yaml is the registered media type for YAML (RFC 9512).
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openapi.Spec)
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

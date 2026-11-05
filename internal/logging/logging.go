// Package logging builds the JSON logger and carries the correlation (request) ID through
// context.Context, so every log line written for one request or one notification can be
// found with a single search.
package logging

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// requestIDKey is an unexported type, so no other package can read or overwrite the value
// by accident with a plain string key.
type requestIDKey struct{}

// WithRequestID returns a copy of ctx that carries id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the ID stored in ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// New returns a JSON logger. Lines logged with the *Context methods (InfoContext,
// ErrorContext, ...) get a request_id field when the context carries one, and trace_id
// and span_id when it carries a recorded trace span (ADR-0021), so a log line leads
// straight to its trace.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(contextHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
}

// contextHandler decorates another handler instead of re-implementing JSON output.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must be overridden: the embedded versions would return the bare
// JSON handler and silently drop the request_id decoration.
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

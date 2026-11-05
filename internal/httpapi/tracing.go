package httpapi

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/kaanismen/event-driven-notification-system/internal/tracing"
)

// untracedPaths are probes and scrapes. They arrive every few seconds and say nothing
// about notifications, so tracing them would bury the traces that matter.
var untracedPaths = map[string]bool{"/livez": true, "/health": true, "/readyz": true, "/metrics": true}

// traceRequests starts a server span for each request (ADR-0021). A caller that sends a
// W3C traceparent header continues its own trace; otherwise a new one starts. The span is
// in the request context, so log lines get its trace_id and creates store it on the rows.
func traceRequests(tracer trace.Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if untracedPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			ctx := tracing.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := tracer.Start(ctx, r.Method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(semconv.HTTPRequestMethodKey.String(r.Method), semconv.URLPath(r.URL.Path)),
			)
			defer span.End()

			rec := &statusRecorder{ResponseWriter: w}
			r = r.WithContext(ctx)
			next.ServeHTTP(rec, r)

			// The mux records the matched pattern on the request, so the route is known
			// only now. The span name uses it ("GET /v1/notifications/{id}"), never the raw
			// path, which contains IDs and would make every span name unique.
			if method, route, ok := strings.Cut(r.Pattern, " "); ok {
				span.SetName(method + " " + route)
				span.SetAttributes(semconv.HTTPRoute(route))
			}
			status := rec.status()
			span.SetAttributes(semconv.HTTPResponseStatusCode(status))
			// Per the HTTP conventions, 4xx is the client's problem, not a server error.
			if status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
		})
	}
}

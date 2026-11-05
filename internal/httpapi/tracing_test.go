package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/kaanismen/event-driven-notification-system/internal/tracing"
)

// tracedServer is a handler with a real SDK tracer whose spans are kept in memory.
func tracedServer(t *testing.T) (http.Handler, *fakeStore, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	fs := &fakeStore{}
	deps := testDeps(slog.New(slog.DiscardHandler), fs)
	deps.Tracer = tp.Tracer("test")
	return NewHandler(deps), fs, rec
}

func attr(span sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	for _, kv := range span.Attributes() {
		if kv.Key == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func TestRequestSpans(t *testing.T) {
	h, fs, rec := tracedServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/notifications",
		strings.NewReader(`{"channel":"sms","recipient":"+905551234567","content":"hi"}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "POST /v1/notifications" || span.SpanKind() != trace.SpanKindServer {
		t.Errorf("span %q kind %v, want POST /v1/notifications as a server span", span.Name(), span.SpanKind())
	}
	if got := attr(span, "http.response.status_code").AsInt64(); got != http.StatusAccepted {
		t.Errorf("status attribute = %d, want 202", got)
	}
	if got := attr(span, "http.route").AsString(); got != "/v1/notifications" {
		t.Errorf("route attribute = %q", got)
	}

	// The rows remember this span, for the delivery to link to.
	stored := tracing.SpanContextOf(fs.createCalls[0].TraceParent)
	if stored.TraceID() != span.SpanContext().TraceID() || stored.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("stored traceparent %q does not point at the request span", fs.createCalls[0].TraceParent)
	}
}

func TestRequestSpanContinuesTheCallersTrace(t *testing.T) {
	h, _, rec := tracedServer(t)
	const callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/019a0000-0000-7000-8000-000000000000", nil)
	req.Header.Set("traceparent", "00-"+callerTrace+"-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	span := rec.Ended()[0]
	if span.SpanContext().TraceID().String() != callerTrace || span.Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("span trace %s parent %s, want the caller's trace and span", span.SpanContext().TraceID(), span.Parent().SpanID())
	}
	// The raw path holds an ID; the span name must use the route pattern instead.
	if span.Name() != "GET /v1/notifications/{id}" {
		t.Errorf("span name = %q", span.Name())
	}
}

func TestProbesAreNotTraced(t *testing.T) {
	h, _, rec := tracedServer(t)
	for _, path := range []string{"/livez", "/health", "/readyz", "/metrics"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if n := len(rec.Ended()); n != 0 {
		t.Errorf("probes produced %d spans, want none", n)
	}
}

func TestUntracedRequestsStoreNoTraceParent(t *testing.T) {
	s := newTestServer(t) // no tracer: tracing off
	s.do(http.MethodPost, "/v1/notifications", `{"channel":"sms","recipient":"+905551234567","content":"hi"}`)
	if tp := s.store.createCalls[0].TraceParent; tp != "" {
		t.Errorf("traceparent = %q with tracing off, want empty", tp)
	}
}

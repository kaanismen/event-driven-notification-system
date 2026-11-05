package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestSetupIsOffWithoutAnEndpoint(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"no endpoint":       {},
		"explicit disabled": {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1", "OTEL_SDK_DISABLED": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			tp, shutdown, err := Setup(context.Background(), env(vars), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := tp.(*sdktrace.TracerProvider); ok {
				t.Error("got a real SDK provider, want the no-op one")
			}
			_, span := tp.Tracer("t").Start(context.Background(), "x")
			if span.SpanContext().IsValid() {
				t.Error("the no-op provider produced a real span")
			}
			if err := shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestSetupWithAnEndpoint(t *testing.T) {
	// Nothing listens there; creating the exporter does not connect, and shutdown with
	// no spans has nothing to send.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	tp, shutdown, err := Setup(context.Background(), env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1"}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tp.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("provider is %T, want the SDK provider", tp)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestTraceParentRoundTrip(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "request")
	defer span.End()

	stored := TraceParent(ctx)
	got := SpanContextOf(stored)
	if !got.IsValid() || got.TraceID() != span.SpanContext().TraceID() || got.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("round trip of %q gave %v, want the span %v", stored, got, span.SpanContext())
	}

	if TraceParent(context.Background()) != "" {
		t.Error("a context without a span produced a traceparent")
	}
	for _, bad := range []string{"", "garbage", "00-zz-zz-01"} {
		if SpanContextOf(bad).IsValid() {
			t.Errorf("SpanContextOf(%q) is valid", bad)
		}
	}
}

func TestHeadersCarryTheTrace(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "call")
	defer span.End()

	h := http.Header{}
	Inject(ctx, propagation.HeaderCarrier(h))
	if h.Get("traceparent") == "" {
		t.Fatal("no traceparent header written")
	}
	got := trace.SpanContextFromContext(Extract(context.Background(), propagation.HeaderCarrier(h)))
	if got.TraceID() != span.SpanContext().TraceID() {
		t.Errorf("extracted trace %s, want %s", got.TraceID(), span.SpanContext().TraceID())
	}
}

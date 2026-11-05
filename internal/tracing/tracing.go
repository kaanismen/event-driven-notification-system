// Package tracing sets up OpenTelemetry tracing (ADR-0021) and carries trace context
// through the places a context.Context cannot go: HTTP headers and the queue row.
//
// The tracer provider is returned to main and passed explicitly to the components that
// create spans, like the logger and the metrics, instead of being installed as the
// library's global provider.
package tracing

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceName is the default service.name of the spans; OTEL_SERVICE_NAME overrides it.
const ServiceName = "notifier"

// propagator reads and writes the W3C traceparent header. It is stateless.
var propagator = propagation.TraceContext{}

// Setup returns the tracer provider described by the standard OpenTelemetry environment
// variables, and a function that flushes and stops it. Without an OTLP endpoint
// (OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT), or with
// OTEL_SDK_DISABLED=true, tracing is off: the provider is a no-op that costs nothing.
// The exporter and the sampler read their own OTEL_* settings, as the specification
// defines them.
func Setup(ctx context.Context, getenv func(string) string, logger *slog.Logger) (trace.TracerProvider, func(context.Context) error, error) {
	endpoint := getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		endpoint = getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" || getenv("OTEL_SDK_DISABLED") == "true" {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(ServiceName)),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES win
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing resource: %w", err)
	}
	// The exporter reports failures (for example, the collector is down) through the
	// library's error handler, which otherwise prints unstructured lines to stderr. It is
	// the library's one global setting, and it only redirects its own error output.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("tracing export failed", "error", err)
	}))

	// The batcher sends spans in the background, so tracing never adds a network round
	// trip to a request or a delivery. The default sampler follows the caller's decision
	// and samples everything else; OTEL_TRACES_SAMPLER changes it.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	logger.Info("tracing enabled", "endpoint", endpoint)
	return tp, tp.Shutdown, nil
}

// Extract returns ctx with the trace context of an incoming request's headers.
func Extract(ctx context.Context, h propagation.HeaderCarrier) context.Context {
	return propagator.Extract(ctx, h)
}

// Inject writes ctx's trace context into outgoing request headers as traceparent.
func Inject(ctx context.Context, h propagation.HeaderCarrier) {
	propagator.Inject(ctx, h)
}

// TraceParent encodes ctx's span as a W3C traceparent value, or "" when ctx has no
// sampled span. It is stored on the queue row so the delivery can link back to the
// request that created it.
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// SpanContextOf decodes a stored traceparent. The result is invalid for "" or garbage,
// which callers treat as "nothing to link to".
func SpanContextOf(traceParent string) trace.SpanContext {
	if traceParent == "" {
		return trace.SpanContext{}
	}
	ctx := propagator.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceParent})
	return trace.SpanContextFromContext(ctx)
}

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not valid JSON: %v\n%s", err, buf.String())
	}
	return line
}

func TestLoggerWritesJSONWithRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo).With("component", "test")

	ctx := WithRequestID(context.Background(), "req-123")
	logger.InfoContext(ctx, "hello", "answer", 42)

	line := decode(t, &buf)
	want := map[string]any{
		"level":      "INFO",
		"msg":        "hello",
		"component":  "test",
		"answer":     float64(42),
		"request_id": "req-123",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("field %q = %v, want %v", k, line[k], v)
		}
	}
	if _, ok := line["time"]; !ok {
		t.Error("missing time field")
	}
}

func TestLoggerOmitsRequestIDWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo).InfoContext(context.Background(), "no id")

	if _, ok := decode(t, &buf)["request_id"]; ok {
		t.Error("request_id present without one in the context")
	}
}

func TestLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelWarn).Info("dropped")
	if buf.Len() != 0 {
		t.Errorf("info line written at warn level: %s", buf.String())
	}
}

func TestLoggerAddsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo)
	ctx, span := sdktrace.NewTracerProvider().Tracer("t").Start(context.Background(), "op")
	defer span.End()

	logger.InfoContext(ctx, "inside a span")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != span.SpanContext().TraceID().String() || line["span_id"] != span.SpanContext().SpanID().String() {
		t.Errorf("log line = %v, want the span's trace_id and span_id", line)
	}

	buf.Reset()
	logger.InfoContext(context.Background(), "no span")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("trace_id logged without a span: %s", buf.String())
	}
}

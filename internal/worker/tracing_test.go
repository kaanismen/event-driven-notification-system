package worker

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/kaanismen/event-driven-notification-system/internal/metrics"
	"github.com/kaanismen/event-driven-notification-system/internal/notification"
	"github.com/kaanismen/event-driven-notification-system/internal/provider"
	"github.com/kaanismen/event-driven-notification-system/internal/provider/mock"
	"github.com/kaanismen/event-driven-notification-system/internal/tracing"
)

// noTracer is what the engine gets when tracing is off.
var noTracer = noop.NewTracerProvider().Tracer("")

func spanAttr(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// Each delivery attempt is its own trace with three steps, linked to the request that
// created the notification, and the provider receives that trace's traceparent
// (ADR-0021).
func TestDeliverySpans(t *testing.T) {
	const requestTrace, requestSpan = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	rows := rowsFor(notification.ChannelSMS, 2)
	rows[0].TraceParent = "00-" + requestTrace + "-" + requestSpan + "-01"
	rows[1].Content = "reject" // answered with a permanent 400 below

	m := mock.NewRecorder(mock.Options{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"content":"reject"`) {
			http.Error(w, `{"error":"bad recipient"}`, http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		m.ServeHTTP(w, r)
	}))
	defer srv.Close()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	f := newFakeStore(rows)
	e := New(f, provider.NewClient(srv.URL, 5*time.Second), slog.New(slog.DiscardHandler), metrics.New(), testConfig(), tp.Tracer("test"))
	runUntilDone(t, e, f, 5*time.Second)

	spans := recorder.Ended()
	root := func(id string) sdktrace.ReadOnlySpan {
		for _, s := range spans {
			if s.Name() == "deliver sms" && spanAttr(s, "notification.id") == id {
				return s
			}
		}
		t.Fatalf("no delivery span for %s", id)
		return nil
	}
	children := func(parent sdktrace.ReadOnlySpan) []string {
		var names []string
		for _, s := range spans {
			if s.Parent().SpanID() == parent.SpanContext().SpanID() && s.SpanContext().TraceID() == parent.SpanContext().TraceID() {
				names = append(names, s.Name())
			}
		}
		slices.Sort(names)
		return names
	}

	sent := root(rows[0].ID.String())
	if sent.SpanKind() != trace.SpanKindConsumer || sent.Parent().IsValid() {
		t.Errorf("delivery span: kind %v, parent %v; want a consumer span at the root of its own trace", sent.SpanKind(), sent.Parent())
	}
	if sent.SpanContext().TraceID().String() == requestTrace {
		t.Error("delivery joined the request's trace; it must start its own and link to it")
	}
	if links := sent.Links(); len(links) != 1 ||
		links[0].SpanContext.TraceID().String() != requestTrace || links[0].SpanContext.SpanID().String() != requestSpan {
		t.Errorf("links = %+v, want one link to the request span", links)
	}
	if got := spanAttr(sent, "notification.outcome"); got != "sent" {
		t.Errorf("outcome = %q, want sent", got)
	}
	if got, want := children(sent), []string{"provider call", "rate limit wait", "record result"}; !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}

	// The provider got the delivery trace's context, not the request's.
	reqs := m.Requests()
	if len(reqs) != 1 {
		t.Fatalf("mock got %d requests, want 1", len(reqs))
	}
	if got := tracing.SpanContextOf(reqs[0].TraceParent); got.TraceID() != sent.SpanContext().TraceID() {
		t.Errorf("provider traceparent %q is not in the delivery trace %s", reqs[0].TraceParent, sent.SpanContext().TraceID())
	}

	failed := root(rows[1].ID.String())
	if failed.Status().Code != codes.Error || spanAttr(failed, "notification.outcome") != "failed" {
		t.Errorf("failed delivery: status %v outcome %q, want error and failed", failed.Status(), spanAttr(failed, "notification.outcome"))
	}
	if len(failed.Links()) != 0 {
		t.Errorf("untraced notification has links %+v", failed.Links())
	}
}

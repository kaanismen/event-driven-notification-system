# ADR-0021: Distributed tracing with OpenTelemetry

- Status: Accepted
- Date: 2026-11-06

## Context

Distributed tracing is an optional bonus in the brief, approved by the maintainer as Phase 6d.
Until now, correlation IDs tied an API request to its worker log lines and the provider call
(ADR-0016). Tracing adds timing: where the time of a request or a delivery went, step by step,
shown as a timeline in a trace viewer. The queue between the API and the workers is the
interesting part, because a delivery can happen seconds or hours after the request.

## Options and decision

### 1. How much is traced automatically

- A. OpenTelemetry SDK and OTLP exporter, with spans written by us.
- B. A, plus the contrib instrumentation libraries `otelhttp` and `otelpgx`.
- C. No SDK; only propagate the `traceparent` header.

**Decision: A** (chosen by the maintainer). Every span has a purpose we chose, and the dependency
cost stays at the SDK and one exporter. B adds two libraries and a span per SQL query, which is
mostly noise for this service. C produces nothing to look at.

### 2. How delivery spans relate to the API request

- A. Each delivery attempt is its own trace, linked to the request.
- B. Delivery spans are children of the request span.
- C. No relation.

**Decision: A** (chosen by the maintainer). This is OpenTelemetry's guidance for messages taken
from a queue. With B, a batch request would hold 1000 delivery subtrees, and a retry five minutes
later would stretch the request's trace over five minutes. With A, traces stay small and finish
quickly, and the viewer follows the link from a delivery to its request.

### 3. How the trace viewer runs

- A. Jaeger always in Docker Compose.
- B. Jaeger in an optional Compose profile.
- C. No viewer; print spans to the console.

**Decision: A** (chosen by the maintainer). `docker compose up` starts it, so tracing works with
no extra step, at the cost of one more container.

## How it works

- **Setup** (`internal/tracing`): the standard `OTEL_*` variables configure everything. With no
  `OTEL_EXPORTER_OTLP_ENDPOINT`, or `OTEL_SDK_DISABLED=true`, the tracer is a no-op and costs
  nothing; tests and `go run` without the variable are untraced. Spans are exported in batches in
  the background over OTLP/HTTP, so tracing never adds a network round trip to a request.
- **No global provider.** `main` passes one tracer to the API and the engine, like the logger and
  the metrics (CLAUDE.md: no global mutable state). The only global set is the library's error
  handler, so export failures appear in the JSON log instead of as plain lines on stderr.
- **API**: a middleware starts a server span per request, named after the route
  (`GET /v1/notifications/{id}`), and continues the caller's trace when a `traceparent` header
  arrives. Probes and metric scrapes are not traced. A create stores the span's `traceparent` on
  every row it inserts (migration `000006`, column `trace_parent`).
- **Worker**: each attempt starts a new trace, `deliver <channel>`, linked to the stored
  `traceparent`, with three child spans: `rate limit wait`, `provider call` and `record result`.
  The root span carries the notification ID, channel, priority, attempt, the time the row waited
  in the queue after it was due, and the outcome (`sent`, `retry`, `failed`, `released`). A dead
  letter marks it as an error.
- **Provider**: the client sends the delivery span's `traceparent`, so a provider that traces
  could continue the trace. The mock provider records and logs it.
- **Logs**: lines written inside a span carry `trace_id` and `span_id`, so a log line leads to its
  trace and back.

```
trace 1f7dd64d  POST /v1/notifications                 5.67 ms
trace a016ee94  deliver sms                            6.10 ms   (link -> 1f7dd64d)
                  rate limit wait                      0.00 ms
                  provider call                        2.47 ms
                  record result                        3.57 ms
```

That is a real pair of traces from the Compose stack, as Jaeger returned them.

## Measured cost

- **Dependencies**: four direct modules (`otel`, `otel/trace`, `otel/sdk` and the OTLP/HTTP
  exporter) and about 16 indirect ones; `go.sum` grew by 32 lines. Even the HTTP exporter compiles
  in about 66 gRPC packages, because the official OTLP protobuf package contains its gRPC service
  code next to the message types.
- **Binary size**: the release binary (`-trimpath -ldflags="-s -w"`, as in the Dockerfile) grew
  from 14.6 MiB to 20.5 MiB, about 40%.
- **Throughput**: a 3000-message load at the full rate produced 12003 spans in Jaeger with none
  dropped, and drained in the same 11 seconds as without tracing.

## Consequences

- One request can be followed from the API, through the queue, to the provider, with timings,
  in the Jaeger UI at `http://localhost:16686`.
- Correlation IDs stay. They work without a trace viewer and are what a client quotes in a
  support request; traces add the timeline.
- The dependency and size cost above is the largest of any feature. It is accepted because
  OpenTelemetry is the industry standard and the alternative is writing an exporter ourselves.
- Compose samples every trace (`parentbased_always_on`). At production volume a ratio sampler
  (`OTEL_TRACES_SAMPLER=parentbased_traceidratio`) would keep a share; it is a setting, not code.
- Jaeger in Compose keeps traces in memory only. It is a demo viewer, not storage.
- A WebSocket connection is one span for the whole connection, which can last minutes.

# ADR-0016: Observability (metrics, health checks, logs)

- Status: Accepted
- Date: 2026-11-05

## Context

The brief asks for a metrics endpoint (queue depth, success and failure rates, latency),
structured logs with correlation IDs, and a health check. Logs and correlation IDs exist since
Phases 1 and 3. Three questions were open for Phase 5.

## Options and decision

### 1. How metrics are produced

- A. `github.com/prometheus/client_golang`, the standard Prometheus library.
- B. The Prometheus text format written by hand on the standard library.
- C. The standard library's `expvar` (JSON at `/debug/vars`).

**Decision: A** (chosen by the maintainer). Prometheus is the format most monitoring systems
collect, and the library gets counters, histograms, concurrency and the exposition format right.
It also adds Go runtime and process metrics. The cost is a dependency with a few modules under it
(ADR-0009). B would be ours to maintain and explain; C has no histograms and no standard format.

All metrics live in one registry created in `main`, not in the library's global default registry,
so tests get fresh counters and there is no global mutable state.

### 2. Health endpoints

- A. `/livez` (the process serves HTTP) and `/readyz` (the database answers, and the process is
  not shutting down). `/health` stays as an alias of `/livez`.
- B. One `/health` that also checks the database.
- C. A, with readiness also failing while a circuit breaker is open.

**Decision: A** (chosen by the maintainer). The two answer different questions. A failing liveness
check gets a process restarted, which only helps if the process itself is stuck; a failing
readiness check takes the instance out of a load balancer, which is right for a database outage.
B would restart a healthy app whenever the database is slow. C contradicts A-33: the API still
works while a provider is down, and the queue absorbs the backlog.

On SIGTERM readiness fails at once, and the HTTP listener stays open for `SHUTDOWN_DRAIN_DELAY`
(default 2 s) before draining, so a load balancer polling `/readyz` stops sending new requests here
before connections are refused. The delivery engine stops claiming immediately, as before.

### 3. The per-message "notification sent" log line

- A. Keep it at INFO.
- B. Move successes to DEBUG and log a periodic summary per channel.
- C. Sample success lines.

**Decision: A** (chosen by the maintainer). It is the per-message audit trail, searchable by
correlation ID. The rate limit bounds it at 300 lines per second (about 30 MB per hour); metrics
now carry the totals, and `LOG_LEVEL=warn` silences it if needed. Successful probe and metrics
requests, on the other hand, are logged at DEBUG: they arrive every few seconds and carry no
information.

## Metrics

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `notifier_queue_depth` | gauge | channel, priority, status | Pending (including retries scheduled later, A-37) and processing rows. Read from the database on each scrape (A-22) with a 2 s timeout; every combination is reported, zeros included. |
| `notifier_deliveries_total` | counter | channel, outcome | Attempts by recorded outcome: `sent`, `retry`, `failed`. Success rate = sent / (sent + failed); retry rate = retry / all. |
| `notifier_provider_request_duration_seconds` | histogram | channel, result | Provider call duration by `success`, `transient_error`, `permanent_error`. |
| `notifier_delivery_latency_seconds` | histogram | channel | Creation to acceptance, including queueing and retries. Both ends are database timestamps, so app and database clocks cannot skew it. |
| `notifier_circuit_breaker_state` | gauge | channel | 0 closed, 1 open, 2 half-open (ADR-0015). |
| `notifier_released_total` | counter | channel, reason | Claimed rows handed back unsent: `shutdown` or `breaker_open`. |
| `go_*`, `process_*` | various | | Runtime and process metrics from the client library. |

A metric is recorded only after the database update it describes succeeded, so the counters agree
with the stored statuses. If the queue depth query fails, the other metrics are still served.

## Consequences

- Measured with `make load` against the mock provider: 10,000 notifications accepted in about 1 s
  and delivered in 38 s (about 100 per second per channel); mean end-to-end latency about 17 s,
  which is queueing behind the backlog (A-20); mean provider call 0.5 ms.
- Counters are per process and reset on restart, which Prometheus' `rate()` handles. With several
  instances, each exposes its own counters; queue depth is global because it comes from the database.
- `/metrics` has no authentication (A-36); in production it would be served on an internal port.
- `NewHandler` now takes a `Deps` struct, since it has five dependencies.

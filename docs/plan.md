# Delivery Plan

Each phase ends with verification, a report and an approval gate. Estimates include build time
and the maintainer's review (30-45 minutes per phase).

| Phase | Scope | Gate | Estimate |
|-------|-------|------|----------|
| 0 | Intake and design: requirements matrix, assumptions, ADR proposals, architecture, plan | Decisions chosen, docs approved | 1.5 h |
| 1 | Foundation: module layout, config, Docker Compose (app + Postgres), first migration, `/health`, JSON logging with request-ID middleware, Makefile, `.gitattributes` | `docker compose up` clean, `curl /health` returns 200 | 1.5 h |
| 2 | Notification API: domain model, validation, create, batch, get by ID and batch ID, cancel, list with filters and cursor pagination, idempotency key | Every endpoint exercised with curl; handler and integration tests green | 2.5 h |
| 3 | Processing engine: dispatchers, claim query, priority policy, per-channel rate limiter, provider client, status transitions, mock provider | A real notification reaches webhook.site and its status becomes `sent` | 2.5 h |
| 4 | Reliability: backoff with jitter, error classification, max attempts and dead-letter, lease recovery, fenced updates, graceful drain | Failure-injection tests (flaky, slow, always failing) green | 2.5 h |
| 5 | Observability: metrics endpoint (queue depth, success/failure counters, latency), log polish, liveness vs readiness | Load script runs and the metrics move | 1.5 h |
| 6 | Delivery: OpenAPI spec served at `/openapi.yaml`, README with Mermaid diagram, decisions and limitations, GitHub Actions (test + lint), clean-clone check | Fresh clone: `docker compose up` and `make test` green | 2.0 h |
| **Total** | | | **14.0 h** |

## Decisions deferred to later phases

These are smaller and are raised in the PLAN step of their phase:

- Phase 1: how `make test` provides Postgres (Compose service vs a Testcontainers dependency).
- Phase 5: metrics format (Prometheus text format via `client_golang` vs a stdlib JSON endpoint).
- Phase 6: which bonus features, if any, to add (circuit breaker, scheduled notifications).

## Cut list if behind schedule

In this order: drop bonus features; replace weighted priority with strict priority; reduce metrics to
counters and queue depth; shorten the README to the required sections.

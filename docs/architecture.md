# Architecture

Status: **Accepted** (2026-11-05). It follows the options chosen in ADR-0001 to ADR-0008.

## Overview

One Go binary runs two parts: an HTTP API and a worker engine. PostgreSQL is both the source of
truth and the queue: each notification is one row, and its status columns double as the queue state.
Workers claim rows with `SELECT ... FOR UPDATE SKIP LOCKED`, send them through a per-channel rate
limiter to the provider, and write the result back to the same row.

```mermaid
flowchart LR
    Client[API client] -->|JSON over HTTP| API

    subgraph App["notifier (one Go binary)"]
        API["HTTP API<br/>net/http + middleware<br/>(request ID, logging, recovery)"]
        Obs["/metrics and /health"]
        subgraph Engine["Worker engine"]
            DS["SMS dispatcher"] --> LS["token bucket<br/>100/s"]
            DE["Email dispatcher"] --> LE["token bucket<br/>100/s"]
            DP["Push dispatcher"] --> LP["token bucket<br/>100/s"]
            LS --> PC
            LE --> PC
            LP --> PC
            PC["Provider client<br/>timeout + error classification"]
        end
    end

    API -->|insert, query, cancel| DB[("PostgreSQL<br/>notifications table<br/>= store + queue")]
    DB -->|claim batch<br/>FOR UPDATE SKIP LOCKED| DS
    DB -->|claim batch| DE
    DB -->|claim batch| DP
    PC -->|POST JSON| Prov["webhook.site<br/>(or mock provider in tests)"]
    PC -->|sent / retry later / failed| DB
    Obs -->|queue depth| DB
```

## Notification lifecycle

```mermaid
stateDiagram-v2
    [*] --> pending: created via API
    pending --> processing: claimed by a worker (lease starts)
    pending --> cancelled: cancel request
    processing --> sent: provider returned 2xx
    processing --> pending: transient error, next_attempt_at = now + backoff
    processing --> pending: lease expired (worker crashed), attempts left
    processing --> pending: released at shutdown, attempt given back
    processing --> failed: permanent error or max attempts (dead-letter)
    processing --> failed: lease expired on the final attempt
    sent --> [*]
    failed --> [*]
    cancelled --> [*]
```

## Request flow (create)

1. The middleware reads `X-Request-ID` or generates one, puts it in the request `context`, adds it
   to every log line and echoes it in the response.
2. The handler decodes JSON with a body size limit and validates the content (required fields,
   recipient format, per-channel length limit). Invalid input returns 422 with field errors.
3. If an `Idempotency-Key` header is present and already known, the handler returns the original
   resource (or 409 if the body differs). See ADR-0004.
4. One transaction inserts the row(s) with `status = pending` and `next_attempt_at = now()`, plus the
   correlation ID so the worker can log under the same ID.
5. The API returns `202 Accepted` with the notification (or batch) ID. Delivery happens later.

## Worker flow (deliver)

1. For each channel, one dispatcher goroutine first dead-letters rows whose lease expired on their
   final attempt, then claims a small batch (ADR-0013): first rows whose lease expired and that have
   attempts left, then due `pending` rows per priority level up to its weighted share, then any due
   rows for unused share (ADR-0008). Each claim is one `FOR UPDATE SKIP LOCKED` statement that sets
   `status = processing`, `locked_until = now() + lease` and `attempts = attempts + 1`. Rows go to a
   pool of sender goroutines over an unbuffered Go channel, so nothing is claimed far ahead.
2. For each claimed row, a sender waits for the channel's token bucket and its one-second window cap
   (ADR-0003 and its amendment), then POSTs `{to, channel, content}` to the provider with a timeout. The notification ID goes in an
   `Idempotency-Key` header and the correlation ID in `X-Request-ID`.
3. The result is written back, guarded by `WHERE id = $1 AND status = 'processing' AND attempts = $2`.
   If the lease expired and another worker took the row, this update matches nothing and is logged
   instead of overwriting newer state (ADR-0005).
   - 2xx: `sent`, store `provider_message_id`.
   - Transient error and attempts left: back to `pending` with `next_attempt_at = now() + backoff with jitter`,
     never sooner than the provider's `Retry-After` (ADR-0002, A-30). The retry comes back through
     the normal claim, so it passes the same rate limiter as a first attempt.
   - Permanent error or no attempts left: `failed` with `last_error` (the dead-letter state).
4. When the queue is empty, the dispatcher sleeps for a short poll interval. When a claim returns a
   full batch, it claims again immediately.
5. On shutdown (SIGTERM), dispatchers stop claiming, in-flight sends finish within a deadline, and
   claimed rows that were not sent are released back to `pending` with their attempt given back
   (ADR-0014).

## Data model (indicative, finalized in Phases 1-2)

`notifications`: `id` (UUID), `batch_id` (nullable), `channel`, `recipient`, `content`, `priority`,
`status`, `attempts`, `max_attempts`, `next_attempt_at`, `locked_until`, `last_error`,
`provider_message_id`, `correlation_id`, `created_at`, `updated_at`, `sent_at`.

`idempotency_keys`: `key` (primary key), `request_hash`, `resource_type`, `resource_id`, `created_at`.

Main index: a partial index on `(channel, priority, next_attempt_at) WHERE status = 'pending'`, so
the claim query only touches due rows and stays fast however many finished rows exist.
A second partial index on `(channel, locked_until) WHERE status = 'processing'` serves the
lease-recovery half of the claim (rows whose worker crashed). Migration `000001` is the source of truth.

## Package layout

```
cmd/notifier/          main: wiring, config, signal handling, graceful shutdown
internal/config/       environment variables to a typed Config struct
internal/logging/      JSON logger; request ID carried through context.Context
internal/notification/ domain types, validation, status transitions (no I/O)
internal/store/        PostgreSQL access: CRUD, claim, complete, queue depth
internal/httpapi/      handlers, middleware, JSON helpers
internal/worker/       dispatchers, rate limiting, retry policy
internal/provider/     HTTP client for the provider + mock provider server
migrations/            versioned SQL migrations (golang-migrate), embedded in the binary
internal/testdb/       test helper: one fresh database per integration test
```

Interfaces exist only at real boundaries: the store (so handlers can be tested with a fake), the
provider client (so workers can be tested with a fake) and the clock (so backoff and leases are
testable).

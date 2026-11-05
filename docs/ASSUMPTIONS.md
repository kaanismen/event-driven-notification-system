# Assumptions

The brief leaves several points open. Each entry states the gap, the assumption we work with, and
why. Status is `Proposed` until the maintainer confirms it, then `Accepted`. Entries that depend on
an open design decision point to the ADR.

## Provider and delivery

| ID | Gap in the brief | Assumption | Why | Status |
|----|------------------|------------|-----|--------|
| A-01 | What "sent" or "delivered" means | `sent` means the provider accepted the message (2xx). We cannot observe delivery to the handset or inbox; no delivery receipts or callbacks. | The provider contract only returns an acceptance response. | Accepted |
| A-02 | Whether only HTTP 202 counts as success | Any 2xx is success. We store `messageId` if present. A 2xx with a missing or unreadable body is still success and is logged as a warning. | Retrying after a 2xx would cause a duplicate send, which is worse than a missing provider ID. | Accepted |
| A-03 | Limits of webhook.site | webhook.site is used for the functional demo only. Load and failure tests use the in-repo mock provider. A static configured response returns the same `messageId` every time, so we never rely on it being unique. | The free tier has request quotas, and a fixed response body cannot generate unique IDs. | Accepted |
| A-04 | Provider authentication | None. The provider URL comes from the `PROVIDER_URL` environment variable. | The brief describes no credentials. | Accepted |
| A-05 | How failures should be classified | Transient: network error, timeout, 408, 429, 5xx. Permanent: any other 4xx and unexpected non-2xx codes. Details in ADR-0002. | Standard HTTP semantics. | Accepted |
| A-30 | How to treat a provider's `Retry-After` | Accepted as seconds or an HTTP date, on any retryable failure. It is the minimum wait before the next attempt, still capped at `RETRY_MAX_DELAY`. Invalid values are ignored. | Retrying sooner would be refused again; the cap stops a provider from parking messages for hours. | Accepted |
| A-31 | What is kept about failed attempts | Only the last error (`last_error`: status code plus the first 200 characters of the body, never the provider URL) and the attempt count. No per-attempt history table. | Enough to debug a dead letter; a history table would add a write per attempt. Logs hold the full history per correlation ID. | Accepted |
| A-32 | A send succeeds but recording the result fails | The row stays `processing`, its lease expires, and it is sent again: a possible duplicate, accepted under at-least-once delivery (ADR-0005). | The alternative, marking it sent before the call, can lose messages silently. | Accepted |
| A-33 | Whether an open circuit breaker makes the service unhealthy | No. `/health` stays healthy while a channel's breaker is open; the API keeps accepting and the queue absorbs the backlog. Queue depth (Phase 5) shows it. | The service itself works; failing health checks would only get it restarted, which does not fix the provider. | Accepted |
| A-34 | Cost of probing a provider that is down | Each probe uses one real attempt of one message. Over a very long outage, probed messages can still be dead-lettered, one per channel per cool-down. | A probe has to be a real send to prove the provider works; the cost is bounded and small. | Accepted |
| A-35 | Whether messages expire | They do not. After a long outage, old messages (for example a stale OTP) are still delivered. A per-message "expires at" field would solve this and is out of scope. | Not in the brief; documented so the behavior is a known choice. | Accepted |

## Content and validation

| ID | Gap in the brief | Assumption | Why | Status |
|----|------------------|------------|-----|--------|
| A-06 | Recipient format per channel | SMS: E.164 phone number (`+` and 8-15 digits). Email: a single address accepted by Go's `net/mail` parser. Push: an opaque device token, 1-512 characters. | Matches the example in the brief and common provider rules. | Accepted |
| A-07 | Character limits | SMS: 160 characters (one segment, counted as Unicode code points; no multipart). Push: 256 characters. Email: 10,000 characters. Content must not be empty or only whitespace. | Simple, explainable limits; real segmentation rules (GSM-7 vs UCS-2) are out of scope. | Accepted |
| A-08 | Email subject | No subject field. The provider body only carries `to`, `channel` and `content`. | Follow the given provider contract. | Accepted |
| A-09 | Default priority | `normal` when omitted. Accepted values: `high`, `normal`, `low`. | Safe default. | Accepted |

## API behavior

| ID | Gap in the brief | Assumption | Why | Status |
|----|------------------|------------|-----|--------|
| A-10 | Batch composition | A batch has 1-1000 items, may mix channels, and gets a server-generated batch ID. All-or-nothing vs partial success: see ADR-0006. | Mixed channels cost nothing extra. | Accepted |
| A-11 | Batch status response | `GET /v1/batches/{id}` returns counts per status plus the items (at most 1000, so no pagination). | Small enough to return in one response. | Accepted |
| A-12 | Date range filter | Filters on `created_at`. `from` is inclusive and `to` is exclusive, both RFC 3339. All timestamps are UTC. | Half-open ranges avoid double-counting at boundaries. | Accepted |
| A-13 | Pagination style | Cursor (keyset) pagination ordered by `created_at DESC, id DESC`. Default page size 50, maximum 200. The cursor is opaque to clients. | Offset pagination gets slow and skips or repeats rows on a table that grows by millions per day. | Accepted |
| A-14 | Which states count as "pending" for cancellation | `pending` includes messages waiting for a retry. In-flight and finished messages: see ADR-0007. Cancel works per notification only (no batch cancel). | Narrow scope; batch cancel can be added later. | Accepted |
| A-15 | Authentication and multi-tenancy | Out of scope. Idempotency keys are therefore global, not per client. | Not mentioned in the brief; keeps the scope inside the time budget. | Accepted |
| A-16 | How long idempotency keys live | Kept as long as the row exists. Production would expire them (for example after 24 hours). | Simplest correct behavior for the assessment. | Accepted |
| A-17 | "Real-time status tracking" | Clients poll the status endpoints. WebSocket push is an optional bonus and not planned. Amended in Phase 6c: the WebSocket bonus was approved and built (ADR-0020); polling still works. | Polling meets the requirement with no extra moving parts. | Accepted |
| A-26 | Request body size limits | 64 KiB for a single notification, 12 MiB for a batch. 1000 maximum-length emails in plain ASCII fit; a batch of long multi-byte content can exceed the limit, gets `413`, and must be split. | Bounds memory per request without rejecting realistic batches. | Accepted |
| A-27 | Format of notification and batch IDs | UUIDv7: random, but starting with a millisecond timestamp. Batch items therefore read back in request order, and primary-key inserts stay local in the index. The creation time is visible in the ID. | Ordered IDs without a separate sequence column; creation time is not secret here. | Accepted |
| A-28 | Pagination consistency | Rows are ordered by `created_at` (the inserting transaction's start time) and ID. A row whose transaction commits after a reader has passed its position is not shown on that walk. Our insert transactions take milliseconds, so the window is tiny. | Keyset pagination without a global sequence; the trade-off is documented rather than hidden. | Accepted |
| A-42 | Scope and lifetime of templates | Template names are global (no tenants, A-15) and unique. Templates cannot be changed or deleted, and a notification names its template by ID only, not by name. | Immutability keeps every stored notification traceable to its text (ADR-0019); an ID is fixed and short, while a name is a label for people. | Accepted |
| A-43 | What a template value may contain | Any string, inserted literally: no HTML or other escaping (content is plain text, A-08), no rendering of placeholders inside values, empty values allowed. A template cannot contain a literal `{{`. | Escaping rules depend on a format we do not have; strictness about `{{` catches broken placeholders. | Accepted |
| A-44 | How errors in templated requests are reported | Missing or unused values are reported on `variables`; problems with the rendered text, such as exceeding the SMS limit, on `content`, the field the text ends up in. | One field name for "the message text", whichever way it was supplied. | Accepted |
| A-45 | Delivery guarantee of real-time events | Best effort: every connection starts from a snapshot, then gets changes in order, but events are not stored or replayed. After a disconnect the client reconnects for a fresh snapshot, so it can miss intermediate steps but never ends with a wrong state. | Replay needs an events table and retention; the snapshot gives correctness without it (ADR-0020). | Accepted |
| A-46 | Which changes are real-time events | Changes of `status` or `attempts` only. Creation is not an event (a client cannot know the ID in advance), and other column changes are not announced. | These are the changes a client tracks; fewer events keep NOTIFY cheap. | Accepted |
| A-47 | Who may watch a notification | Anyone who knows its ID, like the rest of the API (A-15). Browsers may only connect from the same origin, which stops other websites from opening sockets in a visitor's browser. | Consistent with the assessment's scope; IDs are random UUIDv7. | Accepted |
| A-48 | Whether the event feed affects readiness | No. While the listener reconnects, the event endpoints answer `503` and everything else keeps working, like an open circuit breaker (A-33). | Real-time status is an extra; failing readiness would take a working API out of rotation. | Accepted |
| A-49 | How many traces are kept | Compose samples every trace. A caller's sampling decision in `traceparent` is respected; at production volume `OTEL_TRACES_SAMPLER` would keep a share. | Full sampling suits a demo; sampling is configuration, not code. | Accepted |
| A-50 | Whether to trust an incoming `traceparent` | Accepted from any caller, like the rest of the API (A-15). It only lets the caller's trace include our spans; it changes no behavior. | Continuing callers' traces is the point of propagation. | Accepted |
| A-51 | What is not traced | Health probes and metric scrapes. A WebSocket connection is one span for its whole life. | Probes every few seconds would bury real traces. | Accepted |
| A-52 | Where traces are stored | In Jaeger's memory inside Compose; they are lost when it restarts. | A demo viewer; production would export to a managed backend through the same OTLP setting. | Accepted |
| A-29 | Allowed `Idempotency-Key` values | 1-255 printable ASCII characters, not only spaces. Other values return `400`. | Keys end up in logs and indexes; UUIDs and typical client keys fit. | Accepted |

## Processing and scale

| ID | Gap in the brief | Assumption | Why | Status |
|----|------------------|------------|-----|--------|
| A-18 | What the rate limit applies to | Outgoing provider calls, including retries. API intake is not rate limited, because the queue absorbs bursts. Scope across instances: see ADR-0003. | The limit protects the provider, not our API. | Accepted |
| A-19 | Meaning of "millions per day" | We design for about 5 million per day (about 60/s on average) with bursts. Delivery is capped by the rate limit at 300/s (3 channels x 100/s), about 26 million per day in theory. | Gives concrete numbers to size the queue choice (ADR-0001). | Accepted |
| A-20 | Business effect of the rate limit | A burst of 1 million SMS takes about 2.8 hours to drain at 100/s. This is expected behavior, visible through the queue depth metric, not a bug. | Worth stating so reviewers see we understand the consequence. | Accepted |
| A-21 | Deployment size | Docker Compose runs one app instance (API and workers in the same binary) and one Postgres. Horizontal scaling is described but not demonstrated. | One-command setup and limited time. | Accepted |
| A-22 | "Real-time" metrics | Counters and latency are updated in process; queue depth is read from the database when the metrics endpoint is requested. | Always current; cheap with a partial index. | Accepted |
| A-36 | Access to `/metrics` | No authentication, like the rest of the API (A-15). In production it would be served only on an internal port or network. | Consistent with the assessment's scope; the exposure is stated, not hidden. | Accepted |
| A-37 | What queue depth counts | All `pending` rows, including retries scheduled for later, plus `processing` rows as a separate status. "Due now" is not reported separately; the retry counter shows retry activity. Amended in Phase 5b: pending rows waiting for a future `scheduledAt` are reported as `scheduled` instead (ADR-0017). | One cheap grouped count; the difference is small in practice. | Accepted |
| A-38 | What `scheduledAt` promises | The earliest send time, not an exact one. Delivery usually starts within the poll interval (250 ms) after it; under a backlog the message waits behind other due messages, and the rate limit still applies. The time is compared on the database clock. | The queue is polled and rate limited; an exact-time promise would be false. | Accepted |
| A-39 | Priority of scheduled messages | Once due, a scheduled message is an ordinary pending one: priority applies, and it does not jump ahead of other due messages. | One ordering rule for the whole queue. | Accepted |
| A-40 | Scheduling and batch semantics | Batches stay all-or-nothing (ADR-0006): one invalid item, an invalid `scheduledAt` included, rejects the whole batch. Items may mix immediate and scheduled sends. | Scheduling does not change batch semantics. | Accepted |
| A-41 | Idempotent retry after the scheduled time | Validation uses the current time, so retrying a request with the same Idempotency-Key more than 1 minute after its `scheduledAt` returns 422 instead of a replay. Clients normally retry within seconds; checking the key before validating would make the API's error order depend on stored state. | Keeps validation pure and before any database access; the case is rare and visible. | Accepted |

## Tooling

| ID | Gap in the brief | Assumption | Why | Status |
|----|------------------|------------|-----|--------|
| A-23 | Exact Go version ("1.2x") | The latest stable Go 1.2x release, at least 1.22 (needed for method and path patterns in `net/http` routing). | Stdlib routing avoids a router dependency. | Accepted |
| A-24 | `docker-compose up` vs `docker compose up` | Compose v2 (`docker compose up`). The file also works with the legacy `docker-compose` binary. | v1 is deprecated. | Accepted |
| A-25 | "Test suite runnable with a single command" | `make test` runs unit and integration tests. Integration tests need Docker for Postgres. | Integration tests on a real database catch the queue bugs that matter most. | Accepted |

## Questions we could send to the company

The brief invites questions. If time allows, these are the useful ones (the assumptions above stand
until we get an answer):

1. Is the 100 messages per second limit per instance or global across all instances?
2. Should a batch with some invalid items be rejected as a whole or partially accepted?
3. Are the character limits meant per SMS segment, or is multipart SMS expected?

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
| A-17 | "Real-time status tracking" | Clients poll the status endpoints. WebSocket push is an optional bonus and not planned. | Polling meets the requirement with no extra moving parts. | Accepted |
| A-26 | Request body size limits | 64 KiB for a single notification, 12 MiB for a batch. 1000 maximum-length emails in plain ASCII fit; a batch of long multi-byte content can exceed the limit, gets `413`, and must be split. | Bounds memory per request without rejecting realistic batches. | Accepted |
| A-27 | Format of notification and batch IDs | UUIDv7: random, but starting with a millisecond timestamp. Batch items therefore read back in request order, and primary-key inserts stay local in the index. The creation time is visible in the ID. | Ordered IDs without a separate sequence column; creation time is not secret here. | Accepted |
| A-28 | Pagination consistency | Rows are ordered by `created_at` (the inserting transaction's start time) and ID. A row whose transaction commits after a reader has passed its position is not shown on that walk. Our insert transactions take milliseconds, so the window is tiny. | Keyset pagination without a global sequence; the trade-off is documented rather than hidden. | Accepted |
| A-29 | Allowed `Idempotency-Key` values | 1-255 printable ASCII characters, not only spaces. Other values return `400`. | Keys end up in logs and indexes; UUIDs and typical client keys fit. | Accepted |

## Processing and scale

| ID | Gap in the brief | Assumption | Why | Status |
|----|------------------|------------|-----|--------|
| A-18 | What the rate limit applies to | Outgoing provider calls, including retries. API intake is not rate limited, because the queue absorbs bursts. Scope across instances: see ADR-0003. | The limit protects the provider, not our API. | Accepted |
| A-19 | Meaning of "millions per day" | We design for about 5 million per day (about 60/s on average) with bursts. Delivery is capped by the rate limit at 300/s (3 channels x 100/s), about 26 million per day in theory. | Gives concrete numbers to size the queue choice (ADR-0001). | Accepted |
| A-20 | Business effect of the rate limit | A burst of 1 million SMS takes about 2.8 hours to drain at 100/s. This is expected behavior, visible through the queue depth metric, not a bug. | Worth stating so reviewers see we understand the consequence. | Accepted |
| A-21 | Deployment size | Docker Compose runs one app instance (API and workers in the same binary) and one Postgres. Horizontal scaling is described but not demonstrated. | One-command setup and limited time. | Accepted |
| A-22 | "Real-time" metrics | Counters and latency are updated in process; queue depth is read from the database when the metrics endpoint is requested. | Always current; cheap with a partial index. | Accepted |

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

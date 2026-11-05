# ADR-0017: Scheduled notifications

- Status: Accepted
- Date: 2026-11-05

## Context

Scheduling ("send this at 09:00 tomorrow") is an optional bonus in the brief, approved by the
maintainer as Phase 5b. The queue already supports it: a row is claimed only when
`next_attempt_at <= now()` (ADR-0001). What was open was the API contract, the accepted times and
how scheduled rows show in the metrics.

## Options and decision

### 1. How a scheduled notification appears in the API

- A. Status stays `pending`, plus a `scheduledAt` field.
- B. A new status `scheduled` that turns into `pending` when due.
- C. A, plus a computed `"scheduled": true` flag.

**Decision: A** (chosen by the maintainer). No new state: the state machine, cancel (A-14), claim
and every status rule stay as they are. B needs something to flip the status when the time comes
(a job, or the claim) and touches transitions, cancel, metrics and tests; C repeats `scheduledAt`.

### 2. Which times are accepted

- A. More than 1 minute in the past is rejected; 0-60 s in the past counts as now; at most
  `MAX_SCHEDULE_AHEAD` (default 30 days) ahead.
- B. Any past time means "send now", with the same upper limit.
- C. No limits.

**Decision: A** (chosen by the maintainer). A clearly past time is almost always a client bug,
such as a wrong time zone, and is better rejected (422) than sent silently. The minute of tolerance
absorbs ordinary clock differences between client and server. The upper limit catches typos; the
setting itself is capped at one year.

### 3. Scheduled notifications in the queue-depth metric

- A. A third state, `status="scheduled"`, for pending rows whose time is still in the future.
- B. Count them as `pending` (A-37 unchanged).
- C. A separate gauge.

**Decision: A** (chosen by the maintainer). `pending` keeps meaning "should be sent now or is
waiting for a retry", so a large campaign scheduled for tomorrow does not look like a backlog
today. The metric now has 27 series (3 channels x 3 priorities x 3 states).

## How it works

- `scheduledAt` is optional on a single create and on each batch item, so one batch can mix
  immediate and scheduled items. It must be RFC 3339 with a time zone; it is stored in UTC with
  microsecond precision (what PostgreSQL keeps).
- Migration `000003` adds a nullable `scheduled_at` column. The insert sets
  `next_attempt_at = coalesce(scheduled_at, now())`, so the claim simply does not see the row
  before its time. No new index and no worker change: the due index already covers the claim.
  `scheduled_at` keeps the requested time, because `next_attempt_at` moves on every retry.
- The time is part of the idempotency hash: the same key with a different time is a 409. The same
  instant written in another time zone is the same request. Unscheduled requests hash exactly as
  before (the field is omitted when empty), so replays of keys stored before this change still
  match.
- `GET /v1/notifications?scheduled=true|false` filters on whether a time was requested.

## Consequences

- `scheduledAt` is the earliest send time, not an exact one (A-38). Measured: 10-60 ms after the
  time with the default 250 ms poll interval. Under a backlog the message waits behind other due
  messages of its priority (A-39), and the rate limit still applies.
- The time is compared on the **database clock**, which is the authority for every due time in
  the system (leases and retries too). Another machine's clock can differ slightly; the integration
  test measured the mock (host clock) receiving a message 0.24 ms "before" its time while the
  database timestamps showed it was sent after it. The test therefore checks the guarantee on the
  database's own `sent_at`, and allows the host clock a small tolerance.
- Validation uses the current time, so retrying a request with the same Idempotency-Key long after
  its `scheduledAt` has passed returns 422 instead of a replay (A-41).
- Scheduled messages do not expire (A-35): a message whose time passes during an outage is sent
  when delivery resumes.

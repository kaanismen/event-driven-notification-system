# ADR-0014: Reliability implementation (failure injection, shutdown release, circuit breaker)

- Status: Accepted
- Date: 2026-11-05

## Context

ADR-0002 (retry policy) and ADR-0005 (leases and at-least-once delivery) fix what must happen when
a delivery fails or a worker stops. Building them in Phase 4 left three questions open:

1. How the mock provider's failure modes are switched on, for tests and for demos.
2. What exactly happens to rows that are claimed but not yet sent when the process shuts down.
3. Whether to add a circuit breaker (an allowed bonus) now.

## Options and decision

### 1. Turning on the mock's failure modes

- A. Fixed at startup: tests pass `mock.Options`; the Compose service reads `MOCK_*` environment
  variables. Switching modes means restarting the mock container (about a second).
- B. A, plus a runtime control endpoint on the mock to switch modes without a restart.
- C. Tests only; the Compose mock always succeeds.

**Decision: A** (chosen by the maintainer). The modes are: fail the first N attempts of each
message (deterministic, so tests know every message ends up sent), fail a share of requests at
random (for demos; 1 means the provider is down), the status of an injected failure, a
`Retry-After` header, and a delay before every answer (a slow provider). B adds an admin endpoint
to explain and protect for little gain; C would leave no live failure demo for Phase 5 metrics.

### 2. Claimed but unsent rows at shutdown

When shutdown starts, a dispatcher may hold claimed rows it has not handed to a sender, and senders
may be waiting for a rate slot. Neither has called the provider.

- A. Release them at once: back to `pending`, `attempts - 1`, original due time, fenced on the
  claimed attempt like every completion update.
- B. Release them at once but keep the attempt counted.
- C. Leave them `processing` until the lease expires (the Phase 3 behavior).

**Decision: A** (chosen by the maintainer). A restart or deploy must not use up a message's retry
budget, and must not delay it by a lease period. Giving the attempt back is safe because the
message was never sent. A real crash cannot release anything, so a message that crashes every
worker still runs out of attempts (ADR-0005). The release has its own short deadline, because the
shutdown signal has already cancelled the engine's context; if it fails, the lease expiry is the
fallback.

### 3. Circuit breaker

- A. Do not build it; document the limitation.
- B. Build a per-channel breaker inside Phase 4.
- C. Build it as a separate small phase (4b) after Phase 4 is approved.

**Decision: C** (chosen by the maintainer). Phase 4 stays small enough to review in one sitting.
Built in Phase 4b, see ADR-0015.
Until 4b, a provider outage longer than the retry window (about 4 minutes in the worst case with
the defaults) dead-letters the messages that run out of attempts during it.

## Related details settled in the build

- A lease that expires on the **final** attempt is not reclaimed (that would exceed
  `max_attempts`); each claim cycle moves such rows to `failed` with an explicit `last_error`.
- `Retry-After` is honored on any retryable failure that carries it, not only 429 (503 uses it too),
  as a minimum wait capped at `RETRY_MAX_DELAY` (ASSUMPTIONS A-30).
- A retry is a row back in the queue with a later `next_attempt_at`, on the database clock like the
  lease. It comes back through the normal claim, so it passes the same rate limiter as a first
  attempt (DR-06), with no extra code.

## Consequences

- After a graceful shutdown, no row is left in `processing` (tested).
- The duplicate window of ADR-0005 is unchanged: a crash after the provider accepted a message, a
  timeout, or a failed result write (A-32) can still cause a second send.
- With `MOCK_FAIL_FIRST`, the mock keeps one small counter per message key in memory; fine for
  demos, not for an unbounded load run.

# ADR-0002: Retry policy

- Status: Accepted
- Date: 2026-11-04 (proposed), 2026-11-05 (accepted by the maintainer)

## Context

The brief leaves delivery and retry design to the candidate. Provider calls can fail for a moment
(network, overload, rate limiting) or for good (a bad request). Notifications are time-sensitive:
an OTP or flash-sale message that arrives an hour late is often worse than one that never arrives.
So the retry window should be minutes, not hours.

## Options

### Where the retry waits

1. **In memory**: the worker sleeps and tries again. Simple, but it ties up a worker, and the
   retry is lost if the process restarts.
2. **Persisted (recommended)**: the row goes back to `pending` with
   `next_attempt_at = now() + delay`. The worker is free right away, retries survive restarts, and
   the API can show when the next attempt is due.

### Backoff curve

1. **Fixed delay** (for example 10 s): easy to reason about, but it keeps hitting a provider that is
   down, at full strength.
2. **Exponential without jitter**: backs off, but every message that failed in the same outage
   retries at the same moment (a thundering herd).
3. **Exponential with full jitter (recommended)**: `delay = random(0, min(cap, base * 2^(attempt-1)))`.
   Spreads retries out and is the well-known pattern from the AWS architecture guidance.

## Decision

- Persisted retries with exponential backoff and full jitter.
- Defaults (environment variables): `base = 2s`, `cap = 5m`, `max_attempts = 8` in total. The upper
  bounds of the waits are 2, 4, 8, 16, 32, 64 and 128 seconds, so the retry window is about
  4 minutes at worst and about 2 minutes on average. That covers short provider outages without
  delivering stale messages.
- On 429, if the provider sends `Retry-After`, wait at least that long (still capped).
- Classification:
  - Success: any 2xx (ASSUMPTIONS A-02).
  - Transient (retry): network errors, timeouts, 408, 429, 5xx.
  - Permanent (no retry): any other 4xx and unexpected status codes.
- Dead letter: when attempts run out or the error is permanent, the row becomes `failed` with
  `last_error` and `attempts` kept. Dead-lettered rows are found with
  `GET /v1/notifications?status=failed`. We use a state in the same table, not a separate
  dead-letter table, because then the status API covers dead letters for free.

## Consequences

- A timeout is ambiguous: the provider may have received the request. Retrying it can produce a
  duplicate, which we accept under at-least-once delivery (ADR-0005).
- Retries consume rate limit tokens like first attempts do (ADR-0003).
- A manual "requeue from dead letter" endpoint is not planned. It is a possible extension.
- A per-channel circuit breaker (bonus) would pause claiming during a provider outage instead of
  using up attempts. Only built after approval.

## Amendment (2026-11-05, Phase 4)

Built as decided, with two details settled during the build (ADR-0014):

- `Retry-After` is honored on any retryable failure that carries it (503 often does too), not only
  on 429. It is a minimum wait and is still capped at `RETRY_MAX_DELAY` (ASSUMPTIONS A-30).
- Settings: `RETRY_BASE_DELAY` (default 2s) and `RETRY_MAX_DELAY` (default 5m, at most 1h).
  `MAX_ATTEMPTS` already existed and is copied into each row at creation.

# ADR-0005: Crash safety and delivery semantics

- Status: Accepted
- Date: 2026-11-04 (proposed), 2026-11-05 (accepted by the maintainer)

## Context

A worker can crash, or be killed, at any point between claiming a row and recording the result. We
must choose what happens to that message. Exactly-once delivery is not possible over a network
unless the provider dedupes, and webhook.site does not.

## Options

### A. At-most-once

Mark the row `sent` before calling the provider.

- Pros: never a duplicate.
- Cons: a crash at the wrong moment loses the message silently. Unacceptable for OTPs and alerts.

### B. At-least-once with a lease (visibility timeout)

The claim sets `status = processing` and `locked_until = now() + lease`. If the worker dies, the
lease expires and another worker claims the row again.

- Pros: no message is lost. No long database transactions. The lease is visible in the row, so it
  is easy to debug.
- Cons: a duplicate is possible if the worker crashes after the provider accepted the message but
  before the database update.

### C. At-least-once with a transaction held during the send

Lock the row in a transaction, call the provider, commit. If the process crashes, the database
rolls back on its own.

- Pros: no lease logic.
- Cons: one database connection is held for each in-flight HTTP call (about 30 per channel, see
  ADR-0003). Long transactions also block vacuum. The connection pool becomes the bottleneck.

## Decision

**Option B.**

- Lease default: 30 seconds. It must exceed the provider timeout (10 seconds) plus the longest rate
  limiter wait for a claimed batch.
- `attempts` goes up at claim time, so a message that crashes the worker every time still reaches
  the dead-letter state instead of looping forever.
- Fencing: the completion update is
  `WHERE id = $1 AND status = 'processing' AND attempts = $2`. A slow worker whose lease expired
  (and whose row another worker re-claimed) updates nothing and logs a warning. It cannot overwrite
  the newer result.
- How the duplicate window is kept small:
  - The time between the provider's response and our commit is a single short UPDATE.
  - Graceful shutdown finishes in-flight sends and releases unsent claimed rows straight away.
  - The notification ID is sent as an `Idempotency-Key` header for providers that support it.

## Consequences

- The README states the semantics plainly: at-least-once, with a narrow duplicate window on a crash
  or an ambiguous timeout.
- A crash delays the affected messages by up to one lease period.

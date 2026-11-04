# ADR-0007: Cancellation semantics

- Status: Proposed
- Date: 2026-11-04

## Context

The brief requires that notifications which are still waiting can be cancelled. A notification may be in flight when the cancel
arrives: claimed by a worker, waiting for a rate limit token, or with an HTTP call in progress. We
must decide what a cancel means in each state.

## Options

### A. Cancel only what has not been claimed

`POST /v1/notifications/{id}/cancel` runs one atomic statement:
`UPDATE ... SET status = 'cancelled' WHERE id = $1 AND status = 'pending'`.

- `pending` (including waiting for a retry): becomes `cancelled`, `200 OK`.
- `processing`, `sent`, `failed`: `409 Conflict` with the current status.
- Already `cancelled`: `200 OK` (cancelling twice is harmless).
- Pros: always truthful. The row lock settles the race with a worker's claim: exactly one side wins,
  with no extra code.
- Cons: a message in the short `processing` window cannot be stopped.

### B. Best-effort cancel of in-flight messages

Set a `cancel_requested` flag that the worker checks just before the provider call.

- Pros: catches messages that are claimed but still waiting for a token.
- Cons: still cannot stop a call that has already started, so the answer has to be "maybe
  cancelled". Adds a state and more race cases to test.

### C. Always mark as cancelled

- Rejected: it would report `cancelled` for messages that were actually sent.

## Recommendation

**Option A.** It is simple, honest and race-free. The `processing` window is short (a claimed batch
is about 200 ms of tokens plus one HTTP call). If that attempt fails with a transient error, the row
goes back to `pending` and can be cancelled then.

## Consequences

- Use `POST .../cancel` rather than `DELETE`, because the record stays and only its state changes.
- The integration test runs cancel and claim concurrently and checks that a cancelled row is never
  sent.

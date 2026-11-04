# ADR-0004: Idempotency

- Status: Proposed
- Date: 2026-11-04

## Context

"Prevent duplicate sends" has two separate parts:

1. **Intake**: a client retries a create request after a timeout and must not create a second
   notification.
2. **Delivery**: our own workers must not send the same notification twice. That is covered by
   leases and fenced updates in ADR-0005. This ADR covers intake.

## Options

### A. Request-level `Idempotency-Key` header (on single and batch create)

The client sends a unique key with the request. We store the key with a SHA-256 hash of the request
body and the ID of the created resource, in the same transaction as the insert.

- Pros: the common industry convention (Stripe; an IETF draft standardizes the header). It works the
  same for both endpoints. Combined with all-or-nothing batches (ADR-0006), a retried batch either
  replays the whole original batch or is created fresh; it is never half applied.
- Cons: the client cannot dedupe individual items inside different batches.

### B. Per-notification key in the request body (including each batch item)

- Pros: fine-grained; the same notification is never created twice, even across batches.
- Cons: retrying a batch gives a mix of new and existing items, which needs a more complex response
  shape and batch ID handling.

### C. Both A and B

- Pros: covers every case.
- Cons: two mechanisms to build, test and explain. Not justified for this scope.

## Recommendation

**Option A.**

- The key is optional, at most 255 characters, and global in scope (no tenants, ASSUMPTIONS A-15).
- Same key + same body hash: return the original resource with `200 OK` instead of `202`.
- Same key + different body: `409 Conflict`.
- Two requests with the same key at the same moment: a unique index on the key decides. The second
  insert waits for the first transaction, sees the conflict, and returns the original.
- Keys are kept as long as the rows exist (ASSUMPTIONS A-16).

## Consequences

- One small table: `idempotency_keys(key, request_hash, resource_type, resource_id, created_at)`.
- The notification ID is also sent to the provider as an `Idempotency-Key` header, so a real provider
  could dedupe our retries. webhook.site ignores it, which is a documented limitation.

# ADR-0006: Batch semantics

- Status: Proposed
- Date: 2026-11-04

## Context

A batch request carries up to 1000 notifications. Some items may be invalid. We must decide whether
the valid items are stored anyway, and what the response looks like.

## Options

### A. All-or-nothing

Validate every item first. If any item is invalid, return `422` with an error per invalid item
(`index`, `field`, `message`) and store nothing. Otherwise insert all items in one transaction and
return `202`.

- Pros: one simple rule. A batch is one unit for idempotency (ADR-0004), so a retry is always safe.
  The client never has to work out which items made it in.
- Cons: one bad phone number blocks 999 good ones until the client fixes it and resends.

### B. Partial success

Store the valid items and report the invalid ones. Respond with a per-item result list.

- Pros: good items go out without delay.
- Cons: the client must parse per-item results, and retries are harder: resending the whole batch
  would duplicate the items that were already accepted, unless items carry their own keys.

### C. Accept now, validate asynchronously

- Pros: the fastest response.
- Cons: the client only learns about errors by polling. More states and more code.

## Recommendation

**Option A.**

Response on success (`202 Accepted`):

```json
{
  "batchId": "6f1c...",
  "count": 2,
  "notifications": [
    { "id": "a1...", "status": "pending" },
    { "id": "b2...", "status": "pending" }
  ]
}
```

Response on validation failure (`422 Unprocessable Entity`):

```json
{
  "error": "validation_failed",
  "details": [
    { "index": 3, "field": "recipient", "message": "must be an E.164 phone number" }
  ]
}
```

Limits: 1-1000 items. Anything outside that range returns 422. The request body is capped (a few
MB) to protect memory.

## Consequences

- One transaction with a multi-row insert, which is fast for 1000 rows.
- Clients are expected to validate before sending. The 422 response tells them exactly which items
  to fix.

# ADR-0012: API response semantics (replays, list totals, unknown input)

- Status: Accepted
- Date: 2026-11-05

## Context

ADR-0004, ADR-0006 and ADR-0007 fix the behavior of idempotency, batches and cancellation. Three
questions about the response contract were still open when the API was built:

1. What a repeated request with a known `Idempotency-Key` returns.
2. Whether the list endpoint reports a total count.
3. What happens to JSON fields (and query parameters) the API does not know.

## Options and decision

### 1. Idempotent replay

- A. The original resource in its **current** state, `200 OK`. We store only key, hash and ID.
- B. A byte-exact replay of the first response, stored as a snapshot (Stripe's approach). A batch
  key would store up to 1000 items, and the replayed status is already stale.
- C. `409` "already processed" with the ID only. The client needs a second call, which breaks the
  promise in ADR-0004.

**Decision: A** (chosen by the maintainer). A replay also carries `Idempotent-Replayed: true`, so a
client can tell a replay from a fresh create (`202`) without comparing bodies.

### 2. Total count on `GET /v1/notifications`

- A. No total. The response is `{items, nextCursor}`, `nextCursor: null` on the last page.
- B. Exact `COUNT(*)` with the same filters: every page request scans every matching row, which grows
  with the table (millions of rows per day).
- C. A planner estimate: cheap but can be far off.

**Decision: A** (chosen by the maintainer). The store fetches `limit + 1` rows; the extra row only
tells it whether another page exists. Operational totals belong in metrics (Phase 5).

### 3. Unknown input

- A. Reject unknown JSON fields with `400 unknown_field`.
- B. Ignore them (more forgiving for clients that send extra metadata).

**Decision: A** (chosen by the maintainer). A typo such as `"priorty": "high"` would otherwise be
stored silently as `normal`. The same rule applies to the list endpoint's query string: unknown or
repeated parameters return `400 invalid_query`, because `?stauts=failed` would otherwise list
everything.

## Consequences

- Adding an optional request field later is a breaking change for old servers, not for old
  clients: a newer client talking to an older server gets a 400 instead of silent loss.
- Error responses share one shape: `{error, message}`, plus `details[]` for `422` and
  `currentStatus` for `409 not_cancellable`.
- Status codes: `202` new resource (with `Location`), `200` replay or read, `400` malformed input,
  `404` unknown ID, `409` idempotency conflict or not cancellable, `413` body too large, `422`
  validation failed, `500` unexpected (logged, no internal details in the body).

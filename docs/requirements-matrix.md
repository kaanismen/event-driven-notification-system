# Requirements Matrix

Every requirement and deliverable from the assessment brief, paraphrased, mapped to the phase that
delivers it and the kind of test that proves it. The "Evidence" column names the concrete check we
will write. Status is updated at the end of each phase.

## Test type legend

| Code | Meaning |
|------|---------|
| U | Unit test (pure Go, no I/O) |
| H | HTTP handler test (`httptest`, fake store behind the store interface) |
| I | Integration test against a real PostgreSQL (started by `make test`) |
| F | Failure-injection test with the in-repo mock provider (flaky, slow, always failing) |
| E | End-to-end check against the running Docker Compose stack (curl / load script / webhook.site) |
| D | Document or repository review (README, OpenAPI, git history, CI run) |

## 1. Notification management API

| ID | Requirement (paraphrased) | Phase | Test | Evidence | Status |
|----|---------------------------|-------|------|----------|--------|
| API-01 | Accept a single notification with recipient, channel, content and priority | 2 | H, I | Valid request returns 202 with an ID; row is stored as `pending` | Done |
| API-02 | Accept a batch of up to 1000 notifications in one request | 2 | H, I | 1000 items accepted; 0 and 1001 items rejected; invalid item handling per ADR-0006 | Done |
| API-03 | Return the status of a notification by its ID | 2 | H, I | Known ID returns current state; unknown ID 404; malformed ID 400 | Done |
| API-04 | Return the status of all notifications in a batch by batch ID | 2 | H, I | Summary counts per status plus items; unknown batch 404 | Done |
| API-05 | Cancel a notification that has not been sent yet | 2, 3 | H, I | `pending` becomes `cancelled`; other states return 409; a worker never sends a cancelled row (race test) | Done |
| API-06 | List notifications filtered by status, channel and creation date range | 2 | I | Each filter alone and combined returns exactly the matching rows | Done |
| API-07 | Paginate the list endpoint | 2 | I | Walking all pages returns every row once, in a stable order, even with concurrent inserts | Done |
| API-08 | Let clients retry create requests safely (idempotency key at the API) | 2 | H, I | Same key + same body returns the original; same key + different body 409; concurrent duplicates create one row | Done |

## 2. Processing engine

| ID | Requirement (paraphrased) | Phase | Test | Evidence | Status |
|----|---------------------------|-------|------|----------|--------|
| PE-01 | Deliver notifications asynchronously through queue workers, decoupled from the API | 3 | I, E | API returns before delivery; worker moves the row to `sent` | Done |
| PE-02 | Never send more than 100 messages per second on any single channel | 3, 4 | U, F | Mock provider records timestamps; no 1-second window exceeds the limit per channel, retries included | Phase 3 part done |
| PE-03 | Support three priorities (high, normal, low) | 3 | I | With a backlog, claim order follows the policy in ADR-0008 | Done |
| PE-04 | Validate content before accepting it (required fields, per-channel length limits) | 2 | U | Table-driven tests per channel: missing fields, too long, bad recipient format | Done |
| PE-05 | Prevent the same notification from being sent twice | 2, 3, 4 | I, F | Only the lease holder may complete a send; stale worker update is rejected; see ADR-0004 and ADR-0005 | Phase 3 part done |
| PE-06 | Track status transitions so clients see progress | 3 | U, I | Allowed transitions enforced; illegal ones rejected | Done |

## 3. Delivery and retry logic (design left to the candidate)

| ID | Requirement (our design) | Phase | Test | Evidence | Status |
|----|--------------------------|-------|------|----------|--------|
| DR-01 | Call the external provider (webhook.site URL from config) with the agreed JSON body; treat 202 + `messageId` as accepted | 3 | U, E | Client test against an `httptest` server; real notification visible on webhook.site | Done |
| DR-02 | Classify failures as transient (network, timeout, 5xx, 429) or permanent (other 4xx) | 4 | U | Table-driven classification test | Planned |
| DR-03 | Retry transient failures with capped exponential backoff and jitter | 4 | U | Delay stays within bounds for each attempt (seeded random source) | Planned |
| DR-04 | Stop after a maximum number of attempts and park the message in a dead-letter state | 4 | F | Always-failing provider ends in `failed` with the last error recorded | Planned |
| DR-05 | Recover messages held by a crashed worker (lease / visibility timeout) | 4 | I | Claimed row with an expired lease is claimed again and delivered | Planned |
| DR-06 | Retries go through the same rate limiter as first attempts | 4 | F | Flaky provider under load still respects PE-02 | Planned |
| DR-07 | Bound every provider call with a timeout | 3, 4 | F | Slow provider causes a timeout and a retry, not a stuck worker | Phase 3 part done |
| DR-08 | Shut down gracefully: stop claiming, finish or release in-flight work | 1, 4 | I | After shutdown no row stays in `processing` with a live lease | Phase 3 part done |

## 4. Observability

| ID | Requirement (paraphrased) | Phase | Test | Evidence | Status |
|----|---------------------------|-------|------|----------|--------|
| OBS-01 | Metrics endpoint exposing queue depth | 5 | I, E | Depth per channel/priority matches the database during a load run | Planned |
| OBS-02 | Metrics for delivery success and failure rates | 5 | F, E | Counters move as the mock provider succeeds and fails | Planned |
| OBS-03 | Metrics for latency (provider call and end-to-end) | 5 | E | Histogram/summary values change under load | Planned |
| OBS-04 | Structured (JSON) logs | 1 | U | Log line is valid JSON with level, message and context fields | Done |
| OBS-05 | Correlation ID on every request, carried into worker logs for the same notification | 1, 3 | H, I | Incoming `X-Request-ID` is echoed and logged; worker log for that notification carries it | Done |
| OBS-06 | Health check endpoint (liveness; readiness checks the database) | 1, 5 | H, E | `/health` returns 200; readiness returns 503 when the database is down | Phase 1 part done |

## 5. Non-functional expectations (from the overview and business context)

| ID | Expectation (paraphrased) | Phase | Test | Evidence | Status |
|----|---------------------------|-------|------|----------|--------|
| NFR-01 | Handle millions of notifications per day | 0, 5 | D, E | Capacity reasoning in ADR-0001; load script numbers in the README | Planned |
| NFR-02 | Absorb traffic bursts without dropping requests | 3, 5 | E | Large batch is accepted quickly; queue drains at the rate limit | Phase 3 part done |
| NFR-03 | Reliable delivery (no silent loss) | 4 | I, F | At-least-once semantics per ADR-0005 | Planned |
| NFR-04 | Status visible to API consumers and internal teams | 2, 5 | H, E | Status endpoints plus metrics and logs | Phase 2 part done |
| NFR-05 | Implemented in Go 1.2x | 1 | D | `go.mod` Go version | Done |

## 6. Deliverables

| ID | Deliverable (paraphrased) | Phase | Test | Evidence | Status |
|----|---------------------------|-------|------|----------|--------|
| DEL-01 | Public Git repository with a clean, readable commit history | all | D | Conventional Commits, one logical change per commit | In progress |
| DEL-02 | README with setup steps, architecture overview and API examples | 6 | D | README review from a fresh clone | Planned |
| DEL-03 | One-command local setup with Docker Compose | 1, 6 | E | `docker compose up` on a clean clone starts app + Postgres | Phase 1 part done |
| DEL-04 | API documentation as an OpenAPI (Swagger) specification | 6 | D | `/openapi.yaml` served and matches the handlers | Planned |
| DEL-05 | Versioned database migrations | 1+ | I | Migrations apply up and down cleanly in the test run | Phase 1 part done |
| DEL-06 | Test suite runnable with a single command | 1, 6 | E, D | `make test` green locally and in CI | Phase 1 part done |

## 7. Optional bonus features

| ID | Bonus (paraphrased) | Plan | Phase | Test |
|----|---------------------|------|-------|------|
| BON-01 | Failure handling | Dead-letter state is part of the core design (DR-04). Circuit breaker per channel only after approval. | 4 | F |
| BON-02 | Scheduled notifications (send at a future time) | Only if ahead of schedule. Schema already supports it through `next_attempt_at`. | 4+ | I |
| BON-03 | Message templates with variables | Out of scope unless requested | - | - |
| BON-04 | Real-time status via WebSocket | Out of scope unless requested | - | - |
| BON-05 | Distributed tracing | Out of scope; correlation IDs are the lightweight substitute | - | - |
| BON-06 | CI pipeline (tests + lint) on GitHub Actions | After approval | 6 | D |

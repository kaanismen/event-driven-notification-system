# Project: Event-Driven Notification System (Go)

An assessment implementation of a scalable multi-channel notification system (SMS, Email, Push).

## Roles and language
- The maintainer is the architect and decision-maker. You are the implementer and reviewer. You propose, the maintainer decides, you document.
- Everything is in English: chat, code, comments, commit messages, ADRs, docs, README. Use plain, professional English.
- Source of truth for requirements: the assessment PDF whose path the maintainer gives you. Never commit it, and never copy its text verbatim into committed files. Paraphrase requirements.

## How we work: phase gates (strict)
We build in phases. After every phase you STOP and wait for the maintainer's explicit "approved" before starting the next one. Never start a phase early, never fold two phases together.

Every phase follows this loop:
1. PLAN: list what you will build, which files change, and which decisions the maintainer must make (max 3). For each decision give 2-3 options, trade-offs, and your recommendation. Wait for the choice before coding.
2. BUILD: small logical commits, Conventional Commits style (feat:, fix:, test:, docs:, chore:, refactor:). One logical change per commit. Never squash or rewrite history.
3. VERIFY: run gofmt, go vet, the linter if available, and the tests. Report real command output. Never claim something passes without running it. If you cannot run something, say so.
4. REPORT (concise, one screen):
   - What was built and how it maps to the requirements matrix.
   - Exact commands to run and the output to expect.
   - A reading guide: the 3-5 files to read, in order, and what to look for.
   - Open risks and assumptions.
5. Wait for approval.

## Decision log and honesty
- Every significant decision gets an ADR in `docs/adr/NNNN-title.md` (context, options, decision, consequences).
- Anything the requirements leave ambiguous goes into `docs/ASSUMPTIONS.md` instead of being decided silently.
- The README has a short "How this was built" section: AI-assisted workflow with human design decisions and approval gates per phase. Do not hide or overstate AI use.
- Never write personal notes, interview material or private instructions into committed files.

## Engineering rules
- Keep it simple and idiomatic. No speculative abstractions. Interfaces only at real boundaries (queue/store, provider client, clock).
- Small packages, explicit error handling with wrapped errors, `context.Context` propagated everywhere, graceful shutdown, no global mutable state.
- Comments explain why, not what.
- Minimal dependencies, each justified in an ADR. Defaults: stdlib `net/http` (Go 1.22 pattern routing), `log/slog`, `pgx`, `golang-migrate`, `golang.org/x/time/rate`, `github.com/google/uuid`. Tests: stdlib `testing` + `httptest` + a fake provider.
- Configuration through environment variables, with a `.env.example`. No secrets in the repo. The provider URL (webhook.site) comes from config.
- No scope creep. Bonus features only when the maintainer explicitly approves them.

## Target scope
Required: notification management API (create, batch up to 1000, get by ID and batch ID, cancel pending, list with filters + pagination), async processing engine (queue workers, 100 msg/s per channel rate limit, priority high/normal/low, content validation, idempotency), delivery and retry logic (design it with explicit rationale), observability (metrics endpoint with queue depth, success/failure rates and latency; structured logs with correlation IDs; health check).
Deliverables: source with clean commit history, README (setup, architecture overview, API examples), Docker Compose (one command), OpenAPI spec, versioned DB migrations, test suite runnable with a single command (`make test`).
Cheap bonuses allowed after approval: failure handling (dead-letter state, circuit breaker), GitHub Actions CI. Optional only if ahead of schedule: scheduled notifications. Out of scope unless asked: templates, WebSocket, distributed tracing.

## Phases
- Phase 0: Intake and design (NO application code). Produce `docs/requirements-matrix.md` (every requirement mapped to a phase and a test), `docs/ASSUMPTIONS.md`, the architecture proposal and the decisions below, and a plan with a time estimate per phase. Each phase must be small enough to review in about 30-45 minutes.
- Phase 1: Foundation. Repo layout, config, Docker Compose (app + Postgres), migrations, health endpoint, structured logging with correlation-ID middleware, Makefile (`make up`, `make test`, `make lint`). Gate: clean `docker compose up`, `curl /health` works.
- Phase 2: Notification API. Domain model, validation, create, batch (max 1000), get by ID and batch ID, cancel pending, list with filters and pagination, API-level idempotency key. Tests. Gate: every endpoint exercised with curl.
- Phase 3: Processing engine. Workers, priority handling, per-channel rate limit (100/s), provider client for webhook.site (202 + messageId), status transitions, a mock provider for tests and demos. Gate: a real notification reaches the webhook.site URL and the status updates.
- Phase 4: Reliability. Retry with exponential backoff and jitter, transient (5xx, timeout, 429) vs permanent (4xx) errors, max attempts, dead-letter state, worker crash recovery (lease/visibility timeout), idempotency across retries, retries respect the rate limit. Gate: failure-injection tests with the mock provider (flaky, slow, always failing).
- Phase 5: Observability. Metrics endpoint (queue depth, success/failure rates, latency), log polish, readiness vs liveness. Gate: generate load and watch the metrics move.
- Phase 6: Delivery. OpenAPI spec (served at `/openapi.yaml`), README with architecture diagram (Mermaid), decisions and limitations, GitHub Actions (test + lint), final verification from a clean clone. Gate: fresh clone, `docker compose up`, `make test`, all green.

## Decisions to put in front of the maintainer in Phase 0 (recommend, do not assume)
1. Queue and storage. Default recommendation: PostgreSQL as source of truth AND queue via `SELECT ... FOR UPDATE SKIP LOCKED` (one dependency, no dual-write problem, retry and scheduling via `next_attempt_at`). Compare with Redis Streams / RabbitMQ and state the scale assumption (millions per day, bursts at 100/s per channel).
2. Retry policy: backoff curve, jitter, max attempts, error classification, dead-letter behavior.
3. Rate limiter scope: per-channel token bucket, in-process vs shared across instances; document the limit honestly.
4. Idempotency semantics: key scope, behavior on duplicate (return the original), interaction with batches and retries.
5. Crash safety: at-least-once delivery, lease timeout, duplicate-send window and how it is minimized.
6. Batch semantics: all-or-nothing vs partial success; response shape.
7. Cancellation semantics for notifications already in flight.
8. Priority starvation: strict priority vs weighted.

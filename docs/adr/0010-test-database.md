# ADR-0010: Test database for integration tests

- Status: Accepted
- Date: 2026-11-05

## Context

The most important behaviour in this system lives in SQL: claiming with `SKIP LOCKED`, leases,
fenced updates, idempotency conflicts. Those can only be tested against a real PostgreSQL. The
test suite must still run with one command (`make test`), and Go runs test packages in parallel.

## Options

### A. A `postgres-test` service in Docker Compose (profile `test`)

- Pros: no new Go dependency. Starts once and is reused across runs. Runs on tmpfs with
  durability switched off, so it is fast.
- Cons: tests depend on `make test` (or on setting `TEST_DATABASE_URL` by hand).

### B. testcontainers-go

- Pros: each test package starts its own container; plain `go test ./...` is enough.
- Cons: a large dependency tree (Docker client SDK), slower start per package, and known friction
  with Docker Desktop on Windows.

### C. Run tests against the development database

- Pros: nothing to set up.
- Cons: tests destroy the demo data, and parallel packages interfere with each other.

## Decision

**Option A** (chosen by the maintainer).

- `make test` runs `docker compose --profile test up -d --wait postgres-test`, then `go test ./...`
  with `TEST_DATABASE_URL` pointing at it (port 55432 by default).
- Isolation: `internal/testdb.New(t)` creates a database with a random name for each test and drops
  it with `WITH (FORCE)` when the test ends. Parallel test packages therefore never share a schema.
- Without `TEST_DATABASE_URL` the integration tests skip with a message, so `make test-unit` (or
  plain `go test ./...`) runs every unit test on a machine without Docker.

## Consequences

- A skipped integration test is not a passing one. CI (Phase 6) runs `make test`, so all of them
  run there.
- The test server stays up between runs for speed; `make test-db-down` removes it.

# ADR-0011: Running migrations at application startup

- Status: Accepted
- Date: 2026-11-05

## Context

The schema is versioned in `migrations/` (golang-migrate format). Something must apply it before
the app uses the database, and setup must stay one command.

## Options

### A. The app applies pending migrations at startup

The SQL files are embedded in the binary (`embed.FS`), and golang-migrate applies them before the
HTTP server starts.

- Pros: one command, no extra container. The binary always runs against the schema it was built
  with. Tests use the same code path. golang-migrate holds a Postgres advisory lock during the run,
  so several instances starting together apply each migration once.
- Cons: the app's database user needs DDL rights. A slow migration delays startup.

### B. A one-shot `migrate` container in Compose, run before the app

- Pros: closer to production practice; the app needs no DDL rights.
- Cons: one more service, and tests still need their own way to migrate.

### C. Manual `make migrate`

- Pros: full control.
- Cons: breaks the one-command setup and is easy to forget.

## Decision

**Option A** (chosen by the maintainer).

Startup order: load config, connect (with a 10 second timeout), migrate, then serve. If any step
fails, the process exits with a non-zero code and a clear error.

## Consequences

- Each migration file runs inside one transaction (`BEGIN ... COMMIT`), so a failed migration
  leaves no half-applied schema behind.
- Migrations must be backward compatible with the previous app version during a rolling deploy.
  This only matters once more than one version runs at a time.
- In production we would move migrations into a separate deploy step (Option B) and give the
  runtime user no DDL rights. Documented, not built.

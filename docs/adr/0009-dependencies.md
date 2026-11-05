# ADR-0009: Third-party dependencies

- Status: Accepted
- Date: 2026-11-05

## Context

The project rule is minimal dependencies, each one justified. The standard library covers HTTP
routing (method and path patterns since Go 1.22), structured logging (`log/slog`), JSON, contexts
and testing. The needs below are not covered well by the standard library.

## Options

For each need, the alternative was to write the code ourselves on top of the standard library.

| Need | Chosen | Alternative considered | Why the dependency wins |
|------|--------|------------------------|-------------------------|
| PostgreSQL driver and connection pool | `github.com/jackc/pgx/v5` | `database/sql` with `lib/pq` | `lib/pq` is in maintenance mode. pgx is the maintained driver, has a native pool (`pgxpool`) and rich Postgres types, and reports errors with SQLSTATE codes, which the store needs to tell a unique-key conflict apart from other failures. |
| Versioned migrations | `github.com/golang-migrate/migrate/v4` | Hand-written runner over a `schema_migrations` table | Up and down files, a version table with a "dirty" flag, and an advisory lock against concurrent runs. A home-made runner would need all three and tests for them. |
| UUIDs | `github.com/google/uuid` | 16 random bytes formatted by hand | Small, widely used, no transitive dependencies; also parses and validates IDs from requests (Phase 2). |
| Token bucket rate limiter (Phase 3) | `golang.org/x/time/rate` | A hand-written token bucket | Maintained by the Go team, safe for concurrent use, and `Wait(ctx)` honours cancellation. It paces sends; the exact per-second ceiling on top of it is our own small window cap (ADR-0003 amendment). |
| Metrics in the Prometheus format (Phase 5) | `github.com/prometheus/client_golang` | Writing the Prometheus text format by hand on the standard library | The format most monitoring systems collect. It handles counters, histograms, concurrency and the exposition format correctly, and adds Go runtime and process metrics. It brings a few modules with it (`prometheus/common`, `client_model`, `procfs`, `protobuf`); accepted because metrics are a solved problem and less of our own code means less to get wrong (ADR-0016). |

## Decision

Use the five libraries above. Only the packages we import are compiled into the binary. For
golang-migrate that is the core package, the `iofs` source and the `pgx/v5` database driver, not
its other database drivers.

## Consequences

- `go.sum` lists more modules than we import directly, because it also records the test-only
  dependencies of our dependencies. They are not compiled into the binary.
- Any new dependency needs a row in this ADR.

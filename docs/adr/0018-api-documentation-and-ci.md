# ADR-0018: API documentation, README layout and continuous integration

- Status: Accepted
- Date: 2026-11-05

## Context

Phase 6 turns the working service into a deliverable. The brief asks for an OpenAPI (Swagger)
specification, a README with setup, architecture and API examples, and a test suite runnable with
one command. A CI pipeline is an optional bonus the maintainer approved. Three choices were open:
how the spec is produced and kept true, how the documentation is laid out, and who creates the
GitHub repository.

## Options and decision

### 1. How the OpenAPI spec is produced

- A. Written by hand (OpenAPI 3.1, YAML), embedded in the binary, with a Go test that compares
  the served routes with the spec, and Redocly's linter in CI.
- B. Generated from annotations in the handlers (swaggo).
- C. Written by hand and validated in Go tests with `kin-openapi`.

**Decision: A** (chosen by the maintainer). The spec is the API's contract and reads best when it
is written as documentation, with descriptions of the delivery semantics and examples. No Go
dependency is added. B adds a dependency and comment annotations to every handler, and swaggo
targets OpenAPI 2.0. C adds a test-only dependency for a check that the linter already does.

The risk of a hand-written spec is drift. It is limited in three ways:

- All routes live in one table in `internal/httpapi/server.go`. `TestSpecCoversEveryRoute` fails
  if a route is served but not documented, or documented but not served.
- `TestSpecEnumsMatchDomain` checks the channel, priority and status lists against the domain
  package.
- `make lint-spec` (Redocly, recommended rules, version pinned) runs in CI and checks that the
  document is valid OpenAPI and that every reference resolves.

The route test reads the YAML by indentation instead of with a YAML library: the layout of the
hand-written file is fixed, and the linter validates the YAML itself.

What is not checked automatically: that each documented schema field and status code matches the
handler exactly. Handler tests cover the behavior; the spec was written against the handlers and
reviewed. Fields added later need a matching spec edit.

### 2. How the README is organized

- A. One README of moderate length that covers what a reviewer needs (run it, try it, understand
  it, the trade-offs), linking to `docs/` and the ADRs for depth.
- B. A short README plus separate pages for the API and operations.
- C. Everything in the README.

**Decision: A** (chosen by the maintainer). A reviewer gets everything in one place, and the ADRs
stay the single place where each decision is argued, so nothing is written twice.

### 3. CI and the GitHub repository

- A. The workflow file is added here; the maintainer creates the repository and pushes.
- B. The repository is created and pushed from this session with the GitHub CLI.
- C. No CI.

**Decision: A** (chosen by the maintainer). Publishing is the maintainer's action on their own
account.

## The pipeline

`.github/workflows/ci.yml` runs four independent jobs on each push to `main` and each pull
request, so one failure does not hide another:

| Job | What it proves |
|-----|----------------|
| `lint` | `golangci-lint` v2.15.0 with the repository's `.golangci.yml`, the same as `make lint` |
| `test` | `make test TESTFLAGS=-race`: every unit and integration test on a real Postgres, with the race detector |
| `spec` | `make lint-spec`: the OpenAPI document is valid |
| `e2e` | `docker compose up --build --wait` followed by `make smoke`: the one-command setup works from a clean checkout and every endpoint answers as expected |

- The race detector needs cgo, which GitHub's Linux runners have. Before adding it, the full suite
  was run with `-race` in a Linux container: no races, and the rate-limit peaks stayed at 100.
- Actions are pinned to commit SHAs, with the release named in a comment, because a tag can be
  moved to different code. The token is read-only. The cost is that updates are manual; Dependabot
  could automate them and is not set up.
- The `e2e` job runs without a `.env` file, so delivery goes to the bundled mock provider and the
  pipeline needs no webhook.site URL or other secret.

## Consequences

- The spec cannot silently lose or gain an endpoint, and the pipeline checks it on every push.
- CI runs the same commands a reviewer runs locally (`make lint`, `make test`, `make smoke`), so
  a green pipeline means the documented commands work.
- Field-level drift between handlers and spec is still possible and is caught only in review.
- A full CI run takes a few minutes; the `test` job is the slowest because of the race detector.

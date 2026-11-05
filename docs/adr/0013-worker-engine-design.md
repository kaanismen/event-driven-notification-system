# ADR-0013: Worker engine design (handoff, wake-up, mock provider)

- Status: Accepted
- Date: 2026-11-05

## Context

ADR-0001 (queue in Postgres), ADR-0003 (rate limit), ADR-0005 (leases) and ADR-0008 (weighted
priority) fix what the engine must do. Three questions about how it does it were open in Phase 3:

1. How claimed rows reach the goroutines that call the provider.
2. How workers notice new work.
3. What form the mock provider takes.

## Options and decision

### 1. Handoff from the database to the HTTP calls

- A. Per channel, one dispatcher goroutine claims small batches and hands rows to a pool of sender
  goroutines over a Go channel. Senders wait for a rate slot, send, and record the result.
- B. Every sender claims one row at a time: no handoff, but about 300 claim queries per second at
  full speed, and a 70/20/10 split cannot be applied to single-row claims.
- C. One dispatcher for all channels: a slow channel would delay the others, against ADR-0003.

**Decision: A** (chosen by the maintainer). The Go channel is **unbuffered**, so the dispatcher can
hand over a row only when a sender is free. It therefore never claims far ahead of what it can send:
a claimed row waits at most for the rows ahead of it (claim batch plus busy senders) to get a rate
slot, under a second with the defaults, against a 30 second lease. Config validation rejects a lease
shorter than the provider timeout plus that worst-case wait.

### 2. Noticing new work

- A. Polling: claim again at once after a full batch, otherwise sleep `POLL_INTERVAL` (250 ms).
- B. `LISTEN/NOTIFY` with polling as a fallback: near-zero latency, but a dedicated connection,
  reconnect handling and more failure cases.
- C. Adaptive polling (50 ms growing to 1 s): less idle load, harder to explain.

**Decision: A** (chosen by the maintainer). Latency is at most the poll interval (measured: a new
notification reached the provider 168 ms after creation). The idle cost is three cheap index-only
claims per channel per second, which is nothing next to the delivery load.

### 3. Mock provider

- A. A Go package used in tests plus a `cmd/mockprovider` binary run as a Compose service.
- B. Tests only (`httptest`); demos go to webhook.site and hit its quotas.
- C. An off-the-shelf mock server (WireMock): another dependency and config format.

**Decision: A** (chosen by the maintainer). The mock answers 202 with a unique `messageId`, like the
real contract, and rejects malformed bodies with 400. In tests it records every request; as a
service it only logs, so load runs do not grow its memory. It is the default `PROVIDER_URL` in
Compose, so the stack works without a webhook.site account; `PROVIDER_URL` in `.env` switches to
webhook.site. Phase 4 adds failure modes to it.

## Consequences

- Shutdown: dispatchers stop claiming at once. A send that has started finishes and records its
  result, because abandoning a request the provider may have accepted creates a duplicate later.
  Rows claimed but not yet handed to a sender stay `processing` until their lease expires and any
  worker reclaims them; Phase 4 releases them immediately instead.
- The claim also picks up rows whose lease expired, ahead of the priority shares, since they have
  waited longest. Completion updates are already fenced on the claimed attempt (ADR-0005).
- Until Phase 4, every delivery failure is final (`failed` with `last_error`).

# ADR-0003: Rate limiter scope

- Status: Accepted
- Date: 2026-11-04 (proposed), 2026-11-05 (accepted by the maintainer)

## Context

At most 100 messages per second may go to the provider per channel. The limit protects the provider,
so it applies to outgoing calls, including retries, not to API intake (ASSUMPTIONS A-18). The
channels are independent: an SMS backlog must not slow down email.

## Options

### A. In-process token bucket per channel (`golang.org/x/time/rate`)

Each process keeps one limiter per channel. A sender calls `limiter.Wait(ctx)` before every
provider call.

- Pros: tiny, well tested, no extra infrastructure, easy to unit test.
- Cons: the limit applies per process. Two worker instances would send up to 200/s per channel.

### B. Option A with the limit split across instances

Same code, but the operator sets `RATE_LIMIT_PER_CHANNEL = 100 / number_of_worker_instances`.

- Pros: correct globally with no new infrastructure.
- Cons: static. If one instance dies, total throughput drops until the setting changes.

### C. Shared limiter (Redis GCRA, or a counter row in Postgres)

- Pros: an exact global limit with any number of instances.
- Cons: a network round trip per token (or batched token leases), an extra dependency (Redis), or
  extra write load on a hot Postgres row.

## Decision

**Option A, deployed as one worker instance, with Option B documented as the scaling rule.** The
Compose setup runs one instance. The README states clearly that the limit is per process and that
scaling out needs B now or C later.

Settings: rate 100/s, burst 1. The burst size decides how many tokens can pile up while the channel
is idle. With burst 1, no one-second window carries more than about 100 sends. A larger burst would
allow up to `burst + 100` in the first second after an idle period, which breaks a strict reading
of the requirement.

## Consequences

- Senders need enough concurrency to use the rate: at 100/s and roughly 300 ms provider latency,
  about 30 calls are in flight per channel. The number of senders per channel is configurable.
- Claim batches stay small (for example 20 rows, about 200 ms of tokens) so claimed rows do not wait
  so long for tokens that their lease expires.
- Tests check the limit with the mock provider recording request timestamps.

## Amendment (2026-11-05, Phase 3): exact one-second ceiling

Measured with the mock provider, the token bucket alone let **101** sends into some one-second
windows. With burst 1 it schedules sends exactly 10 ms apart, so send 1 and send 101 are exactly one
second apart, and the smallest timer delay on send 1 squeezes them into one window. The average rate
was correct; a strict sliding-window count was not.

Options put to the maintainer: (a) keep the bucket and add a cap on actual send times, (b) lower the
rate to 99/s, (c) keep it and document the 101. **Chosen: (a).**

- The token bucket still paces sends evenly. A window cap behind it keeps the start times of the
  last 100 sends in a ring buffer, and a send may start only when the 100th-previous one is at least
  one window old. That makes "at most 100 in any window" exact for our send times
  (`internal/worker/ratelimit.go`).
- What happens after a send starts (goroutine scheduling, the network) can still move requests
  closer together at the provider. The window is therefore one send interval wider than a second,
  1.01 s. Measured at the mock provider: a 2 ms margin let 101 through when the CPU was saturated by
  the full test suite; 5 ms and 10 ms held in every run. The cost is about 1% of throughput (100
  sends per 1.01 s, roughly 99 per second sustained).
- No client-side limiter can fully control arrival times at a remote provider. The honest claim is:
  exact at our boundary, with headroom for jitter.

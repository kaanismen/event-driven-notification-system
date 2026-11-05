# ADR-0015: Circuit breaker per channel

- Status: Accepted
- Date: 2026-11-05

## Context

Retries (ADR-0002) handle short failures, but every retry costs an attempt. With the defaults, a
provider outage longer than about 4 minutes dead-letters messages that would have been delivered
minutes later, and meanwhile we keep calling a provider that is down. The maintainer approved a
circuit breaker as a bonus, built as its own phase (ADR-0014). Three questions were open.

## Options and decision

### 1. When the breaker opens

- A. After N transient failures in a row (default 10).
- B. When the failure rate over a sliding window passes a threshold, with a minimum call count.
- C. One breaker for the whole provider instead of one per channel.

**Decision: A** (chosen by the maintainer). It is simple to explain and test, and a full outage
trips it within a fraction of a second at 100 sends per second. Any healthy answer resets the run,
so a provider that fails only some of the time never trips it; partial failures are what retries
are for. Per channel, because channels normally have separate providers and are already isolated
from each other (ADR-0003).

Only transient failures (ADR-0002) count. A permanent error such as 400 proves the provider is up,
so for the breaker it counts as healthy.

### 2. How it recovers

- A. A fixed cool-down (default 30 s), then one probe message.
- B. A cool-down that doubles after each failed probe, up to a cap.
- C. A gradual ramp-up of the send rate after recovery.

**Decision: A** (chosen by the maintainer). Predictable: during a long outage each channel sends
one probe per cool-down, and recovery is noticed within one cool-down. B notices recovery later; C
adds much code, and the rate limiter already caps the load when sending resumes.

### 3. Build or use a library

- A. Our own small state machine with an injectable clock.
- B. `github.com/sony/gobreaker`.

**Decision: A** (chosen by the maintainer). About 80 lines, no new dependency (ADR-0009 unchanged),
and the fake clock makes the state machine testable without sleeping.

## How it works

- **Closed:** normal claiming and sending. Transient failures in a row are counted.
- **Open:** the dispatcher claims nothing until the cool-down is over. A sender that already holds a
  row and finds the breaker open (it opened while the sender waited for a rate slot) releases the
  row with its attempt given back, as at shutdown (ADR-0014).
- **Half-open:** the dispatcher claims one row and sends it as the probe; no other sends happen.
  Success closes the breaker; failure opens it for another full cool-down.
- Results of sends that started before the breaker opened arrive late; they are ignored, so they
  can neither close an open breaker nor count towards the next opening.
- Every state change is logged at WARN. Phase 5 adds the state to the metrics.

## Consequences

- Measured in the integration test: a 1.2 s outage with threshold 5 and a 200 ms cool-down made 9
  provider calls instead of about 120, no message used more than 2 attempts, and all messages were
  sent once after recovery.
- Each probe uses a real attempt of one message (A-34); over a very long outage, probed messages
  can still be dead-lettered, one per channel per cool-down.
- An open breaker does not fail `/health` (A-33); the API keeps accepting and the queue absorbs the
  backlog.
- The breaker is per process. With several app instances, each opens on its own failures, which is
  acceptable: each probes at most once per cool-down.

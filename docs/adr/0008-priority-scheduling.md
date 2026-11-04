# ADR-0008: Priority scheduling and starvation

- Status: Proposed
- Date: 2026-11-04

## Context

Notifications have three priorities. With no backlog, priority does not matter: everything goes out
right away. It matters only when a channel's queue grows faster than 100/s can drain it, for example
during a flash sale. Then we must decide who waits.

## Options

### A. Strict priority

Claim with `ORDER BY priority, next_attempt_at`.

- Pros: the simplest option and it uses the index directly. High priority (OTPs, alerts) always goes
  first.
- Cons: under sustained overload, low-priority messages can wait forever (starvation).

### B. Weighted share per claim cycle (work-conserving)

Each claim cycle splits the batch by weight, for example 70% high, 20% normal and 10% low. If a level
has fewer due rows than its share, the unused slots go to the other levels in priority order, so no
capacity is wasted.

- Pros: no starvation. Under overload each level gets a guaranteed share (70/20/10 msg/s on a full
  channel). Each per-level query still uses the index.
- Cons: up to 4 small queries per cycle instead of 1. High priority is capped at 70% while lower
  levels have a backlog, so high-priority messages can wait a little when they exceed 70/s.

### C. Strict priority with aging

A message's priority rises the longer it waits.

- Pros: no starvation, and high priority still dominates.
- Cons: the sort key depends on `now()`, so a plain index cannot serve it. The behavior is harder to
  predict and to explain.

## Recommendation

**Option B with weights 70/20/10.** It keeps high priority dominant, guarantees progress for low
priority, and stays index-friendly. If time runs short, Option A is the fallback (it is in the plan's
cut list), and its starvation risk gets documented.

## Consequences

- The weights are configuration, not code.
- The test fills a backlog of all three levels and checks the share of each level in claimed
  batches. A second test checks that unused share is redistributed.

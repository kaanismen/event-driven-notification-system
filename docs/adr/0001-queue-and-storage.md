# ADR-0001: Queue and storage

- Status: Proposed
- Date: 2026-11-04

## Context

We need durable storage for notification state, which the API reads, and a work queue for the
workers. The queue must support three priorities, delayed retries, and recovery when a worker
crashes.

Scale assumption (ASSUMPTIONS A-19): about 5 million notifications per day, roughly 60/s on
average, with bursts that arrive as 1000-item batches. Delivery is capped by the rate limit at
3 channels x 100/s = 300 sends/s. Each notification costs about 3 writes (insert, claim, complete),
so peak write load is around 1,000 row writes per second.

## Options

### A. PostgreSQL as both store and queue (`FOR UPDATE SKIP LOCKED`)

Each notification row carries its queue state (`status`, `next_attempt_at`, `locked_until`). Workers
claim due rows with `SKIP LOCKED`, so concurrent workers never block on or take the same row.

- Pros: one dependency. State and queue live in the same row and change in the same transaction, so
  there is no dual write and no outbox. Priorities are an `ORDER BY`. Delayed retries are a
  timestamp column. The API reads the same row the worker updates. Easy to inspect with plain SQL.
- Cons: workers poll, which adds latency up to the poll interval (`LISTEN/NOTIFY` can remove it
  later). Frequent updates create dead tuples, so autovacuum matters at high volume. One primary
  limits throughput, but its ceiling (thousands of updates per second) is far above our 300/s
  delivery cap.

### B. PostgreSQL + Redis Streams

- Pros: very high throughput. Consumer groups and `XAUTOCLAIM` handle worker crashes.
- Cons: a second system to run. Writing to the database and the stream is a dual write, so it needs
  a transactional outbox to stay consistent. No native priorities (one stream per priority) and no
  native delays (an extra sorted set). Durability depends on Redis persistence settings.

### C. PostgreSQL + RabbitMQ

- Pros: native priority queues, acknowledgements and dead-letter exchanges. Mature.
- Cons: a second system and the same dual-write problem. Delayed retries need TTL plus dead-letter
  chains or a plugin. Queue state lives in the broker and status lives in the database, so there
  are two sources of truth to reconcile.

Kafka was considered and rejected: it has no per-message priority and no per-message delay, and it
is heavy for this scale.

## Recommendation

**Option A.** It meets the scale with a wide margin, removes a whole class of consistency bugs, and
keeps the setup to one command and two containers.

## Consequences

- A partial index on due `pending` rows keeps the claim query fast however many finished rows exist.
- Finished rows grow without bound. Production would need retention or time-based partitioning.
  Documented, not built.
- When to revisit: if fan-out to several independent consumers is needed, or sustained throughput
  approaches several thousand messages per second, move to a broker fed by a transactional outbox.

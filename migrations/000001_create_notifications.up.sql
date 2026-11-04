BEGIN;

-- One row per notification. The row is both the record the API reads and the queue entry
-- the workers claim (ADR-0001), so status and queue columns change together in one UPDATE.
CREATE TABLE notifications (
    id                  uuid        PRIMARY KEY,
    batch_id            uuid,
    channel             text        NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    recipient           text        NOT NULL,
    content             text        NOT NULL,
    -- 1 = high, 2 = normal, 3 = low: a number so that "more urgent" sorts first.
    priority            smallint    NOT NULL DEFAULT 2 CHECK (priority BETWEEN 1 AND 3),
    status              text        NOT NULL DEFAULT 'pending'
                                    CHECK (status IN ('pending', 'processing', 'sent', 'failed', 'cancelled')),
    -- Incremented at claim time, so a message that crashes its worker every time still
    -- reaches max_attempts and the dead-letter state (ADR-0005).
    attempts            integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- Copied from configuration at insert time, so changing the setting later does not
    -- change the retry budget of messages already queued.
    max_attempts        integer     NOT NULL CHECK (max_attempts > 0),
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    locked_until        timestamptz,
    last_error          text,
    provider_message_id text,
    correlation_id      text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    sent_at             timestamptz,

    -- A claimed row without a lease could never be recovered after a worker crash.
    CONSTRAINT processing_has_lease CHECK (status <> 'processing' OR locked_until IS NOT NULL)
);

-- Claim path: due pending rows per channel in priority order. Partial, so finished rows
-- (the vast majority over time) never enter the index.
CREATE INDEX notifications_due_idx
    ON notifications (channel, priority, next_attempt_at)
    WHERE status = 'pending';

-- Crash recovery path: claimed rows whose lease has expired (ADR-0005).
CREATE INDEX notifications_lease_idx
    ON notifications (channel, locked_until)
    WHERE status = 'processing';

-- GET /v1/batches/{id}.
CREATE INDEX notifications_batch_idx
    ON notifications (batch_id)
    WHERE batch_id IS NOT NULL;

-- List endpoint: keyset pagination ordered newest first (ASSUMPTIONS A-13).
CREATE INDEX notifications_created_idx
    ON notifications (created_at DESC, id DESC);

COMMIT;

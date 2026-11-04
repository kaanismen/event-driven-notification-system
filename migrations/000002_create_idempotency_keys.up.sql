BEGIN;

-- Request-level idempotency (ADR-0004). The key row is inserted in the same transaction
-- as the notifications it created, so a key never exists without its resource.
CREATE TABLE idempotency_keys (
    -- The primary key is what serialises two concurrent requests with the same key: the
    -- second INSERT waits for the first transaction, then sees the conflict.
    key           text        PRIMARY KEY CHECK (char_length(key) BETWEEN 1 AND 255),
    -- SHA-256 of the normalized request; a different body under the same key is a 409.
    request_hash  bytea       NOT NULL,
    resource_type text        NOT NULL CHECK (resource_type IN ('notification', 'batch')),
    -- A notification ID or a batch ID, depending on resource_type. No foreign key:
    -- batches are not a table, only a shared batch_id on notification rows.
    resource_id   uuid        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

COMMIT;

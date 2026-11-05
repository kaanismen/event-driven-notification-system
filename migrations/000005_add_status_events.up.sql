BEGIN;

-- version counts the updates of a row. Updates of one row are serialised by its row lock,
-- so a higher version is always a later state. Real-time subscribers use it to drop
-- events that are not newer than what they already have (ADR-0020).
ALTER TABLE notifications ADD COLUMN version bigint NOT NULL DEFAULT 0;

-- One trigger for every UPDATE path (API cancel, worker claim and results, lease recovery),
-- so no code path can forget to bump the version or to announce a change.
CREATE FUNCTION notifications_on_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.version := OLD.version + 1;
    -- Only status and attempt changes are announced; a new attempt can keep the status
    -- (a crashed worker's row claimed again stays processing).
    IF NEW.status IS DISTINCT FROM OLD.status OR NEW.attempts IS DISTINCT FROM OLD.attempts THEN
        -- pg_notify is transactional: listeners receive the message only when this
        -- transaction commits, and in commit order. The payload limit is 8000 bytes, so
        -- the error text is shortened.
        PERFORM pg_notify('notification_status', json_build_object(
            'id', NEW.id,
            'batchId', NEW.batch_id,
            'status', NEW.status,
            'attempts', NEW.attempts,
            'version', NEW.version,
            'lastError', left(NEW.last_error, 1000),
            'updatedAt', NEW.updated_at
        )::text);
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER notifications_on_update
    BEFORE UPDATE ON notifications
    FOR EACH ROW EXECUTE FUNCTION notifications_on_update();

COMMIT;

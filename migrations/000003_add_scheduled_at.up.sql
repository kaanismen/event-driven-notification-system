-- Scheduled notifications (ADR-0017). The claim already waits for next_attempt_at, so a
-- scheduled row is inserted with next_attempt_at = scheduled_at and needs no new index.
-- scheduled_at keeps the time the client asked for, because next_attempt_at moves on
-- every retry. NULL means "send as soon as possible".
ALTER TABLE notifications ADD COLUMN scheduled_at timestamptz;

BEGIN;

DROP TRIGGER IF EXISTS notifications_on_update ON notifications;
DROP FUNCTION IF EXISTS notifications_on_update();
ALTER TABLE notifications DROP COLUMN IF EXISTS version;

COMMIT;

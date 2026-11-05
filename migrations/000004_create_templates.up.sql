BEGIN;

-- Message templates (ADR-0019). Immutable: there is no UPDATE or DELETE path, so a
-- template ID always means the same text.
CREATE TABLE templates (
    id         uuid        PRIMARY KEY,
    -- Unique, so creating the same template twice can be recognised and answered with
    -- the existing one instead of a duplicate.
    name       text        NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 100),
    channel    text        NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    body       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- GET /v1/templates: keyset pagination, newest first, like the notification list.
CREATE INDEX templates_created_idx ON templates (created_at DESC, id DESC);

-- Which template a notification's content was rendered from. Nullable: most
-- notifications carry plain content. Templates are never deleted, so the reference
-- cannot dangle; no index, because nothing looks notifications up by template yet.
ALTER TABLE notifications ADD COLUMN template_id uuid REFERENCES templates (id);

COMMIT;

-- In-app notifications (the bell). Modules create them; each person sees their own.
CREATE TABLE notifications (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users(id),
    kind        text        NOT NULL,          -- e.g. followup_due, followup_overdue, followup_escalated
    title       text        NOT NULL,
    body        text,
    link        text,                          -- app path to open, e.g. /deals/<id>
    entity_type text,
    entity_id   uuid,
    dedupe_key  text UNIQUE,                   -- stops the same reminder being created twice
    created_at  timestamptz NOT NULL DEFAULT now(),
    read_at     timestamptz
);
CREATE INDEX notifications_user_idx   ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;

-- One summary email per person per day, whichever server instance sends it.
CREATE TABLE notification_digests (
    user_id  uuid NOT NULL REFERENCES users(id),
    day      date NOT NULL,
    kind     text NOT NULL,
    sent_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day, kind)
);

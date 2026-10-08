-- Core platform tables shared by every module.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL,
    full_name     text        NOT NULL,
    position      text        NOT NULL,          -- job title shown as "responsible person / position"
    department    text        NOT NULL,
    role          text        NOT NULL,
    password_hash text        NOT NULL,
    active        boolean     NOT NULL DEFAULT true,
    last_login_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_role_chk CHECK (role IN
        ('ceo','sales_manager','sales_executive','operations','technical','finance','admin'))
);
CREATE UNIQUE INDEX users_email_uq ON users (lower(email));

-- Append-only activity log. Never UPDATE or DELETE rows here.
CREATE TABLE audit_log (
    id          bigserial   PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor_id    uuid        REFERENCES users(id),
    actor_name  text        NOT NULL,
    actor_role  text        NOT NULL,
    entity_type text        NOT NULL,
    entity_id   uuid        NOT NULL,
    action      text        NOT NULL,
    summary     text        NOT NULL,
    changes     jsonb,
    reason      text,
    request_id  text
);
CREATE INDEX audit_log_entity_idx ON audit_log (entity_type, entity_id, occurred_at DESC);
CREATE INDEX audit_log_actor_idx  ON audit_log (actor_id, occurred_at DESC);

CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();

-- Server-side counters for document numbers. period = year, or 0 when the
-- sequence never resets.
CREATE TABLE number_sequences (
    doc_type   text        NOT NULL,
    period     int         NOT NULL,
    last_value bigint      NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_type, period)
);

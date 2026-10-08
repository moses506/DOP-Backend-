-- Account set-up links (invitations and password resets) and the email send log.

-- Only a SHA-256 hash of the link token is stored, so a database leak can't
-- be used to take over accounts. Cleared once the link is used.
ALTER TABLE users ADD COLUMN setup_token_hash bytea;
ALTER TABLE users ADD COLUMN setup_expires_at timestamptz;
CREATE UNIQUE INDEX users_setup_token_uq ON users (setup_token_hash) WHERE setup_token_hash IS NOT NULL;

-- One row per email attempt: what was sent to whom, and whether it went.
CREATE TABLE email_log (
    id         bigserial   PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    recipient  text        NOT NULL,
    kind       text        NOT NULL,       -- e.g. user_invite, password_reset
    subject    text        NOT NULL,
    status     text        NOT NULL CHECK (status IN ('sent', 'failed', 'logged')),
    error      text
);
CREATE INDEX email_log_recipient_idx ON email_log (lower(recipient), created_at DESC);

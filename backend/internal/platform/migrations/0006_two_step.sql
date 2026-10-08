-- Two-step verification with an authenticator app (TOTP, RFC 6238).
ALTER TABLE users
    ADD COLUMN mfa_secret          bytea,        -- encrypted (AES-256-GCM); never stored in plain text
    ADD COLUMN mfa_enabled_at      timestamptz,
    ADD COLUMN mfa_last_step       bigint,       -- last accepted 30-second step, so a code can't be used twice
    ADD COLUMN mfa_pending_secret  bytea,        -- during set-up, until the first code is confirmed
    ADD COLUMN mfa_pending_until   timestamptz;

-- One-time recovery codes for a lost phone, stored as SHA-256 hashes.
CREATE TABLE mfa_recovery_codes (
    id         bigserial PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users(id),
    code_hash  bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at    timestamptz
);
CREATE INDEX mfa_recovery_user_idx ON mfa_recovery_codes (user_id) WHERE used_at IS NULL;

-- A session opened with only a password, for someone who must set up
-- two-step verification, can do nothing else until they have.
ALTER TABLE sessions ADD COLUMN needs_mfa_setup boolean NOT NULL DEFAULT false;

ALTER TABLE login_attempts DROP CONSTRAINT login_attempts_outcome_check;
ALTER TABLE login_attempts ADD CONSTRAINT login_attempts_outcome_check CHECK (outcome IN
    ('success','bad_password','unknown_email','inactive','locked','rate_limited','unlocked','code_required','bad_code'));

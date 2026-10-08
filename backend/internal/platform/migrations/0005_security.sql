-- Server-side sessions: every sign-in is a row, so signing out (or an
-- administrator) can end it at once, and idle sessions expire.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),  -- last request made while the person was active
    expires_at   timestamptz NOT NULL,                -- absolute limit, however active
    revoked_at   timestamptz,
    revoked_why  text,                                -- signed_out, signed_out_everywhere, idle, ...
    ip           text,
    user_agent   text
);
CREATE INDEX sessions_user_open_idx ON sessions (user_id) WHERE revoked_at IS NULL;

-- Every sign-in attempt, successful or not: the record used to stop password
-- guessing, and a security log for administrators.
CREATE TABLE login_attempts (
    id         bigserial PRIMARY KEY,
    email_key  text        NOT NULL,        -- lower-cased email as typed, whether or not an account exists
    user_id    uuid REFERENCES users(id),
    ip         text        NOT NULL,
    user_agent text,
    outcome    text        NOT NULL CHECK (outcome IN
               ('success','bad_password','unknown_email','inactive','locked','rate_limited','unlocked')),
    at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX login_attempts_email_idx ON login_attempts (email_key, at DESC);
CREATE INDEX login_attempts_ip_idx    ON login_attempts (ip, at DESC);

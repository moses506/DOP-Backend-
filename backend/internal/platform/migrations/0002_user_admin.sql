-- Staff administration: temporary passwords and session revocation.

-- Set when an administrator creates an account or resets a password; the user
-- must choose their own password at next sign-in.
ALTER TABLE users ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN password_changed_at  timestamptz;

-- Tokens issued before this moment are rejected. Bumped on deactivation,
-- password reset, password change and role change, so a lost tablet or a
-- leaver is signed out everywhere immediately.
ALTER TABLE users ADD COLUMN sessions_valid_after timestamptz;  -- NULL: no revocation yet

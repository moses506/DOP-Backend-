-- Who each person reports to, so managers see and are chased about their own team.
ALTER TABLE users ADD COLUMN manager_id uuid REFERENCES users(id);
CREATE INDEX users_manager_idx ON users (manager_id) WHERE manager_id IS NOT NULL;

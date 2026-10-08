-- Where a visit or meeting was logged from, when the person checked in on site
-- with their phone's location. Kept as recorded; never edited.
ALTER TABLE client_interactions ADD COLUMN checkin_lat        numeric(9,6);
ALTER TABLE client_interactions ADD COLUMN checkin_lng        numeric(9,6);
ALTER TABLE client_interactions ADD COLUMN checkin_accuracy_m int;
ALTER TABLE client_interactions ADD COLUMN checked_in_at      timestamptz;
ALTER TABLE client_interactions ADD CONSTRAINT client_interactions_checkin_ck
    CHECK ((checkin_lat IS NULL) = (checkin_lng IS NULL) AND (checkin_lat IS NULL) = (checked_in_at IS NULL));
CREATE INDEX client_interactions_author_idx ON client_interactions (created_by, occurred_at DESC);

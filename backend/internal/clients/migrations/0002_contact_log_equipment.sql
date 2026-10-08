-- Clients module: the contact log (calls, visits, meetings) with follow-ups,
-- when each client was last contacted, and the equipment installed at clients.

-- Set from the contact log, so lists can show and filter on it cheaply.
ALTER TABLE clients ADD COLUMN last_contact_at timestamptz;
CREATE INDEX clients_last_contact_idx ON clients (last_contact_at) WHERE status = 'active';

-- One row per call, visit, meeting, email or message with the client.
-- Entries are a record of what happened: they are not edited or deleted.
-- Only the follow-up on an entry changes (done, or moved to another date).
CREATE TABLE client_interactions (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id           uuid        NOT NULL REFERENCES clients(id) ON DELETE RESTRICT,
    contact_id          uuid        REFERENCES client_contacts(id),
    kind                text        NOT NULL CHECK (kind IN ('call','visit','meeting','email','message','note')),
    occurred_at         timestamptz NOT NULL,
    summary             text        NOT NULL,
    outcome             text,
    -- The next step this contact led to, if any.
    follow_up_on        date,
    follow_up_note      text,
    follow_up_owner_id  uuid        REFERENCES users(id),
    follow_up_done_at   timestamptz,
    follow_up_done_by   uuid        REFERENCES users(id),
    follow_up_done_note text,
    created_by          uuid        NOT NULL REFERENCES users(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK ((follow_up_on IS NULL) = (follow_up_owner_id IS NULL)),
    CHECK (follow_up_done_at IS NULL OR follow_up_on IS NOT NULL)
);
CREATE INDEX client_interactions_client_idx ON client_interactions (client_id, occurred_at DESC);
CREATE INDEX client_interactions_open_idx ON client_interactions (follow_up_on, follow_up_owner_id)
    WHERE follow_up_on IS NOT NULL AND follow_up_done_at IS NULL;

-- Machines and systems DOS has supplied or looks after at a client.
-- Never deleted: equipment taken out of use is marked removed with a reason.
CREATE TABLE client_equipment (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id       uuid        NOT NULL REFERENCES clients(id) ON DELETE RESTRICT,
    site_id         uuid        REFERENCES client_sites(id),
    category        text        NOT NULL CHECK (category IN
                        ('copier','printer','scanner','computer','server','network','projector','phone_system','software','other')),
    make            text        NOT NULL,
    model           text        NOT NULL,
    serial_no       text,
    serial_key      text,       -- serial_no upper-cased without spaces or dashes, for matching
    asset_tag       text,
    installed_on    date,
    warranty_until  date,
    contract        text        NOT NULL DEFAULT 'none'
                        CHECK (contract IN ('none','service','rental','lease')),
    contract_until  date,
    notes           text,
    status          text        NOT NULL DEFAULT 'in_use' CHECK (status IN ('in_use','removed')),
    removed_at      timestamptz,
    removed_reason  text,
    created_by      uuid        NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    version         int         NOT NULL DEFAULT 1,
    CHECK ((status = 'removed') = (removed_at IS NOT NULL))
);
CREATE INDEX client_equipment_client_idx ON client_equipment (client_id);
-- A serial number can be in use at one client at a time.
CREATE UNIQUE INDEX client_equipment_serial_uq ON client_equipment (serial_key)
    WHERE serial_key IS NOT NULL AND status = 'in_use';
CREATE INDEX client_equipment_expiry_idx ON client_equipment (warranty_until, contract_until) WHERE status = 'in_use';

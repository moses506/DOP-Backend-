-- Clients module: client register, contacts and delivery sites (brief 4.1).
-- Clients are never deleted; they are deactivated (status = 'inactive').

CREATE TABLE clients (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reference           text        NOT NULL UNIQUE,      -- server-allocated, e.g. DOS-CL-00001
    legal_name          text        NOT NULL,
    trading_name        text,
    name_key            text        NOT NULL,             -- normalised name for duplicate detection
    registration_no     text,                             -- PACRA registration number
    tpin                text,                             -- ZRA taxpayer identification number
    sector              text,
    billing_address     text,
    default_currency    char(3)     NOT NULL DEFAULT 'ZMW',
    credit_terms_days   int         NOT NULL DEFAULT 30 CHECK (credit_terms_days BETWEEN 0 AND 365),
    credit_limit        numeric(14,2) CHECK (credit_limit IS NULL OR credit_limit >= 0),
    status              text        NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    notes               text,
    owner_id            uuid        NOT NULL REFERENCES users(id),   -- current responsible person
    created_by          uuid        NOT NULL REFERENCES users(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    deactivated_at      timestamptz,
    deactivation_reason text,
    duplicate_override_reason text,                       -- set when registered despite a possible duplicate
    version             int         NOT NULL DEFAULT 1    -- optimistic locking
);

CREATE INDEX clients_name_key_idx  ON clients (name_key);
CREATE INDEX clients_tpin_idx      ON clients (tpin) WHERE tpin IS NOT NULL;
CREATE INDEX clients_regno_idx     ON clients (registration_no) WHERE registration_no IS NOT NULL;
CREATE INDEX clients_owner_idx     ON clients (owner_id);
CREATE INDEX clients_search_trgm   ON clients
    USING gin ((legal_name || ' ' || coalesce(trading_name,'') || ' ' || reference) gin_trgm_ops);

CREATE TABLE client_contacts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id   uuid        NOT NULL REFERENCES clients(id) ON DELETE RESTRICT,
    name        text        NOT NULL,
    position    text,
    email       text,
    phone       text,
    is_primary  boolean     NOT NULL DEFAULT false,
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX client_contacts_client_idx ON client_contacts (client_id);
CREATE UNIQUE INDEX client_contacts_one_primary ON client_contacts (client_id) WHERE is_primary AND active;

CREATE TABLE client_sites (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id     uuid        NOT NULL REFERENCES clients(id) ON DELETE RESTRICT,
    name          text        NOT NULL,          -- e.g. "Kansanshi Mine – Stores"
    address       text        NOT NULL,
    contact_name  text,
    contact_phone text,
    notes         text,
    active        boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX client_sites_client_idx ON client_sites (client_id);

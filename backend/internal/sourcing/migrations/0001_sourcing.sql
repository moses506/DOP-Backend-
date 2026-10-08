-- Sourcing: the suppliers DOS buys from, and price requests (RFQs) sent to
-- them before quoting a client. Suppliers are never deleted, only made
-- inactive; requests are cancelled, never deleted. Every change is audited.

CREATE TABLE suppliers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reference   text        NOT NULL UNIQUE,            -- DOS-SUP-0001
    name        text        NOT NULL,
    name_key    text        NOT NULL,                   -- normalised for duplicate checks
    website     text,
    brands      text,                                   -- what they sell, e.g. "HP, Lenovo, Cisco"
    notes       text,
    status      text        NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    inactive_reason text,
    created_by  uuid        NOT NULL REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    version     int         NOT NULL DEFAULT 1,
    CHECK ((status = 'inactive') = (inactive_reason IS NOT NULL))
);
CREATE UNIQUE INDEX suppliers_name_key_uq ON suppliers (name_key);
CREATE INDEX suppliers_search_trgm ON suppliers USING gin ((name || ' ' || coalesce(brands, '') || ' ' || reference) gin_trgm_ops);

-- DOS's account with the supplier, one per region it buys through
-- (a distributor often has a South Africa account and a separate Zambia one).
CREATE TABLE supplier_accounts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    supplier_id    uuid          NOT NULL REFERENCES suppliers(id),
    region         text          NOT NULL CHECK (region IN ('ZA','ZM','GB','OTHER')),
    account_number text,
    terms          text          CHECK (terms IN ('credit','cod')),
    credit_limit   numeric(14,2) CHECK (credit_limit IS NULL OR credit_limit >= 0),
    currency       char(3)       NOT NULL,
    contact_name   text,
    contact_email  text          NOT NULL,
    other_emails   text[]        NOT NULL DEFAULT '{}',
    note           text,
    active         boolean       NOT NULL DEFAULT true,
    created_at     timestamptz   NOT NULL DEFAULT now(),
    updated_at     timestamptz   NOT NULL DEFAULT now(),
    CHECK (terms = 'credit' OR credit_limit IS NULL)
);
CREATE UNIQUE INDEX supplier_accounts_region_uq ON supplier_accounts (supplier_id, region) WHERE active;
CREATE INDEX supplier_accounts_supplier_idx ON supplier_accounts (supplier_id);

-- Exchange rates to ZMW, for comparing prices quoted in different currencies.
CREATE TABLE fx_rates (
    currency     char(3)       PRIMARY KEY CHECK (currency <> 'ZMW'),
    rate_to_zmw  numeric(14,6) NOT NULL CHECK (rate_to_zmw > 0),  -- 1 unit of currency = this many ZMW
    updated_by   uuid          NOT NULL REFERENCES users(id),
    updated_at   timestamptz   NOT NULL DEFAULT now()
);

CREATE TABLE price_requests (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reference     text        NOT NULL UNIQUE,           -- DOS-RFQ-2026-0001
    title         text        NOT NULL,
    needed_by     date,
    client_id     uuid        REFERENCES clients(id),    -- who it's for, when known
    deal_id       uuid        REFERENCES deals(id),
    notes         text,                                  -- shown to suppliers
    internal_note text,
    status        text        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','sent','decided','cancelled')),
    owner_id      uuid        NOT NULL REFERENCES users(id),
    created_by    uuid        NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    sent_at       timestamptz,
    decided_at    timestamptz,
    cancelled_at  timestamptz,
    closed_reason text,
    version       int         NOT NULL DEFAULT 1,
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);
CREATE INDEX price_requests_status_idx ON price_requests (status, created_at DESC);
CREATE INDEX price_requests_owner_idx  ON price_requests (owner_id);
CREATE INDEX price_requests_client_idx ON price_requests (client_id) WHERE client_id IS NOT NULL;

CREATE TABLE price_request_items (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id  uuid          NOT NULL REFERENCES price_requests(id) ON DELETE CASCADE,
    position    int           NOT NULL,
    description text          NOT NULL,
    part_no     text,
    brand       text,
    quantity    numeric(12,2) NOT NULL CHECK (quantity > 0),
    unit        text          NOT NULL DEFAULT 'each',
    chosen_invite_id uuid,                               -- the supplier picked for this item
    UNIQUE (request_id, position)
);

-- Each supplier account asked for prices on a request, and what they replied.
CREATE TABLE price_request_invites (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id    uuid        NOT NULL REFERENCES price_requests(id) ON DELETE CASCADE,
    supplier_id   uuid        NOT NULL REFERENCES suppliers(id),
    account_id    uuid        NOT NULL REFERENCES supplier_accounts(id),
    sent_to       text        NOT NULL,                  -- email address, or how it was asked (phone, WhatsApp)
    emailed       boolean     NOT NULL,
    sent_at       timestamptz NOT NULL DEFAULT now(),
    sent_by       uuid        NOT NULL REFERENCES users(id),
    status        text        NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting','replied','declined')),
    replied_at    timestamptz,
    currency      char(3),
    lead_time     text,                                  -- e.g. "In stock", "2–3 weeks"
    valid_until   date,
    reply_note    text,
    UNIQUE (request_id, account_id)
);
ALTER TABLE price_request_items ADD CONSTRAINT price_request_items_chosen_fk
    FOREIGN KEY (chosen_invite_id) REFERENCES price_request_invites(id);

CREATE TABLE price_request_prices (
    invite_id   uuid          NOT NULL REFERENCES price_request_invites(id) ON DELETE CASCADE,
    item_id     uuid          NOT NULL REFERENCES price_request_items(id) ON DELETE CASCADE,
    unit_cost   numeric(14,2) CHECK (unit_cost IS NULL OR unit_cost >= 0),  -- NULL: can't supply this item
    availability text,
    PRIMARY KEY (invite_id, item_id)
);

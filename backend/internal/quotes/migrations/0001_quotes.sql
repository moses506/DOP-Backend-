-- Quotations module: the product and price list, quotations and their lines.
-- Quotations are never deleted: they are cancelled, declined, expired or
-- superseded by a revision, and every change is in the audit log.

CREATE TABLE products (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sku         text,                                   -- DOS item code, optional
    name        text          NOT NULL,
    description text,
    category    text          NOT NULL DEFAULT 'other' CHECK (category IN
                    ('copier','printer','scanner','computer','server','network','projector','phone_system',
                     'software','consumable','service','other')),
    unit        text          NOT NULL DEFAULT 'each',  -- each, box, hour, month…
    unit_price  numeric(14,2) NOT NULL CHECK (unit_price >= 0),
    currency    char(3)       NOT NULL DEFAULT 'ZMW',
    vat_rate    numeric(5,2)  NOT NULL DEFAULT 16 CHECK (vat_rate BETWEEN 0 AND 100),
    active      boolean       NOT NULL DEFAULT true,
    created_by  uuid          NOT NULL REFERENCES users(id),
    created_at  timestamptz   NOT NULL DEFAULT now(),
    updated_at  timestamptz   NOT NULL DEFAULT now(),
    version     int           NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX products_sku_uq ON products (upper(sku)) WHERE sku IS NOT NULL;
CREATE INDEX products_search_trgm ON products USING gin ((name || ' ' || coalesce(sku, '')) gin_trgm_ops);

CREATE TABLE quotes (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reference        text          NOT NULL,           -- DOS-Q-2026-0001, shared by all revisions
    revision         int           NOT NULL DEFAULT 1,
    client_id        uuid          NOT NULL REFERENCES clients(id),
    deal_id          uuid          REFERENCES deals(id),
    contact_id       uuid          REFERENCES client_contacts(id),
    title            text          NOT NULL,
    currency         char(3)       NOT NULL,
    quote_date       date          NOT NULL,
    valid_until      date          NOT NULL,
    status           text          NOT NULL DEFAULT 'draft' CHECK (status IN
                         ('draft','pending_approval','ready','sent','accepted','declined','expired','superseded','cancelled')),
    notes            text,                              -- shown to the client
    terms            text,                              -- shown to the client
    internal_note    text,                              -- staff only
    subtotal         numeric(14,2) NOT NULL DEFAULT 0,  -- before discounts, excluding VAT
    discount_total   numeric(14,2) NOT NULL DEFAULT 0,
    net_total        numeric(14,2) NOT NULL DEFAULT 0,
    vat_total        numeric(14,2) NOT NULL DEFAULT 0,
    total            numeric(14,2) NOT NULL DEFAULT 0,
    discount_pct     numeric(5,2)  NOT NULL DEFAULT 0,  -- overall discount, for approval
    max_line_discount numeric(5,2) NOT NULL DEFAULT 0,
    client_po        text,                              -- the client's order number, on acceptance
    owner_id         uuid          NOT NULL REFERENCES users(id),
    submitted_at     timestamptz,
    approved_by      uuid          REFERENCES users(id),
    approved_at      timestamptz,
    approval_note    text,
    sent_at          timestamptz,
    sent_to          text,
    decided_at       timestamptz,
    decision_note    text,
    closed_reason    text,                              -- why it was cancelled
    created_by       uuid          NOT NULL REFERENCES users(id),
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now(),
    version          int           NOT NULL DEFAULT 1,
    UNIQUE (reference, revision),
    CHECK (valid_until >= quote_date)
);
CREATE INDEX quotes_client_idx ON quotes (client_id, created_at DESC);
CREATE INDEX quotes_deal_idx   ON quotes (deal_id) WHERE deal_id IS NOT NULL;
CREATE INDEX quotes_status_idx ON quotes (status, valid_until);
CREATE INDEX quotes_owner_idx  ON quotes (owner_id);

CREATE TABLE quote_lines (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    quote_id     uuid          NOT NULL REFERENCES quotes(id) ON DELETE CASCADE,
    position     int           NOT NULL,
    product_id   uuid          REFERENCES products(id),
    sku          text,
    description  text          NOT NULL,
    quantity     numeric(12,2) NOT NULL CHECK (quantity > 0),
    unit         text          NOT NULL DEFAULT 'each',
    unit_price   numeric(14,2) NOT NULL CHECK (unit_price >= 0),
    discount_pct numeric(5,2)  NOT NULL DEFAULT 0 CHECK (discount_pct BETWEEN 0 AND 100),
    vat_rate     numeric(5,2)  NOT NULL DEFAULT 16 CHECK (vat_rate BETWEEN 0 AND 100),
    line_net     numeric(14,2) NOT NULL,
    line_vat     numeric(14,2) NOT NULL,
    line_total   numeric(14,2) NOT NULL,
    UNIQUE (quote_id, position)
);

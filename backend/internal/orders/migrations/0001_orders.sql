-- Customer orders: what a client has committed to buy, made from an accepted
-- quotation. Each line tracks how much is ready and how much has been
-- delivered, so the order shows what is still outstanding. Orders are never
-- deleted: they are cancelled with a reason, and every change is audited.

CREATE TABLE customer_orders (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reference         text          NOT NULL UNIQUE,     -- DOS-SO-2026-0001
    client_id         uuid          NOT NULL REFERENCES clients(id),
    quote_id          uuid          REFERENCES quotes(id),
    quote_number      text,                              -- as printed, e.g. DOS-Q-2026-0004-R2
    deal_id           uuid          REFERENCES deals(id),
    contact_id        uuid          REFERENCES client_contacts(id),
    title             text          NOT NULL,
    currency          char(3)       NOT NULL,
    client_po         text,                              -- the client's LPO number
    client_po_date    date,
    order_date        date          NOT NULL,
    delivery_due      date,                              -- the date promised to the client
    site_id           uuid          REFERENCES client_sites(id),
    delivery_instructions text,
    payment_terms     text,
    internal_note     text,
    status            text          NOT NULL DEFAULT 'open' CHECK (status IN
                          ('open','in_progress','ready','part_delivered','delivered','invoiced','on_hold','cancelled')),
    hold_reason       text,
    held_at           timestamptz,
    subtotal          numeric(14,2) NOT NULL DEFAULT 0,
    discount_total    numeric(14,2) NOT NULL DEFAULT 0,
    net_total         numeric(14,2) NOT NULL DEFAULT 0,
    vat_total         numeric(14,2) NOT NULL DEFAULT 0,
    total             numeric(14,2) NOT NULL DEFAULT 0,
    delivered_at      timestamptz,                       -- when the last item was delivered
    invoice_ref       text,                              -- until invoicing is built: the invoice number from the accounts system
    invoiced_on       date,
    invoiced_by       uuid          REFERENCES users(id),
    closed_reason     text,                              -- why it was cancelled
    cancelled_at      timestamptz,
    owner_id          uuid          NOT NULL REFERENCES users(id),
    created_by        uuid          NOT NULL REFERENCES users(id),
    created_at        timestamptz   NOT NULL DEFAULT now(),
    updated_at        timestamptz   NOT NULL DEFAULT now(),
    version           int           NOT NULL DEFAULT 1,
    CHECK (delivery_due IS NULL OR delivery_due >= order_date - 365),
    CHECK ((status = 'on_hold') = (hold_reason IS NOT NULL)),
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
    CHECK ((status = 'invoiced') = (invoice_ref IS NOT NULL))
);
-- One live order per quotation; a cancelled one can be replaced.
CREATE UNIQUE INDEX customer_orders_quote_uq ON customer_orders (quote_id) WHERE quote_id IS NOT NULL AND status <> 'cancelled';
CREATE INDEX customer_orders_client_idx ON customer_orders (client_id, created_at DESC);
CREATE INDEX customer_orders_deal_idx   ON customer_orders (deal_id) WHERE deal_id IS NOT NULL;
CREATE INDEX customer_orders_status_idx ON customer_orders (status, delivery_due);
CREATE INDEX customer_orders_owner_idx  ON customer_orders (owner_id);

CREATE TABLE customer_order_lines (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id       uuid          NOT NULL REFERENCES customer_orders(id) ON DELETE CASCADE,
    position       int           NOT NULL,
    product_id     uuid          REFERENCES products(id),
    sku            text,
    description    text          NOT NULL,
    quantity       numeric(12,2) NOT NULL CHECK (quantity > 0),
    unit           text          NOT NULL DEFAULT 'each',
    unit_price     numeric(14,2) NOT NULL,
    discount_pct   numeric(5,2)  NOT NULL DEFAULT 0,
    vat_rate       numeric(5,2)  NOT NULL DEFAULT 16,
    line_net       numeric(14,2) NOT NULL,
    line_vat       numeric(14,2) NOT NULL,
    line_total     numeric(14,2) NOT NULL,
    -- Where it comes from: in stock, ordered from a supplier, or a service DOS performs.
    source         text          NOT NULL DEFAULT 'supplier' CHECK (source IN ('stock','supplier','service')),
    supplier_note  text,                                 -- e.g. "Ordered from Canon SA, ETA 20 Oct"
    qty_ready      numeric(12,2) NOT NULL DEFAULT 0,
    qty_delivered  numeric(12,2) NOT NULL DEFAULT 0,
    UNIQUE (order_id, position),
    CHECK (qty_ready >= 0 AND qty_ready <= quantity),
    CHECK (qty_delivered >= 0 AND qty_delivered <= quantity)
);

-- Deliveries recorded on the order. Delivery notes (phase 3) will create
-- these; until then staff record them by hand with the signed paper's number.
CREATE TABLE customer_order_deliveries (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id      uuid        NOT NULL REFERENCES customer_orders(id),
    delivered_on  date        NOT NULL,
    reference     text,                                  -- delivery note / GRN number on the paper
    received_by   text,
    note          text,
    created_by    uuid        NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_order_deliveries_order_idx ON customer_order_deliveries (order_id, delivered_on DESC);

CREATE TABLE customer_order_delivery_lines (
    delivery_id  uuid          NOT NULL REFERENCES customer_order_deliveries(id),
    line_id      uuid          NOT NULL REFERENCES customer_order_lines(id),
    quantity     numeric(12,2) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (delivery_id, line_id)
);

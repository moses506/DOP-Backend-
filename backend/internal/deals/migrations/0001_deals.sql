-- Deals module: sales opportunities with a client, tracked through a pipeline
-- until they are won or lost. A deal leads to quotations (next module).
-- Deals are never deleted; a lost deal keeps its reason.

CREATE TABLE deals (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reference        text        NOT NULL UNIQUE,     -- server-allocated, e.g. DOS-D-2026-0001
    client_id        uuid        NOT NULL REFERENCES clients(id),
    title            text        NOT NULL,
    description      text,
    stage            text        NOT NULL DEFAULT 'lead'
                     CHECK (stage IN ('lead','qualified','proposal','negotiation','won','lost')),
    value            numeric(14,2) CHECK (value IS NULL OR value >= 0),
    currency         char(3)     NOT NULL DEFAULT 'ZMW',
    probability      int         NOT NULL DEFAULT 10 CHECK (probability BETWEEN 0 AND 100),
    expected_close   date,
    source           text,                            -- e.g. referral, tender, existing client
    next_step        text,                            -- the agreed follow-up
    next_step_date   date,
    lost_reason      text,
    closed_at        timestamptz,                     -- set when won or lost
    stage_changed_at timestamptz NOT NULL DEFAULT now(),
    owner_id         uuid        NOT NULL REFERENCES users(id),  -- responsible sales person
    created_by       uuid        NOT NULL REFERENCES users(id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    version          int         NOT NULL DEFAULT 1,  -- optimistic locking

    CONSTRAINT deals_lost_needs_reason CHECK (stage <> 'lost' OR lost_reason IS NOT NULL),
    CONSTRAINT deals_closed_when_final CHECK ((stage IN ('won','lost')) = (closed_at IS NOT NULL))
);

CREATE INDEX deals_client_idx ON deals (client_id);
CREATE INDEX deals_owner_idx  ON deals (owner_id);
CREATE INDEX deals_stage_idx  ON deals (stage);
CREATE INDEX deals_closed_idx ON deals (closed_at) WHERE closed_at IS NOT NULL;
CREATE INDEX deals_search_trgm ON deals USING gin ((title || ' ' || reference) gin_trgm_ops);

-- Calls, meetings, visits and notes logged against a deal.
CREATE TABLE deal_notes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    deal_id     uuid        NOT NULL REFERENCES deals(id) ON DELETE RESTRICT,
    kind        text        NOT NULL DEFAULT 'note' CHECK (kind IN ('note','call','meeting','visit','email')),
    body        text        NOT NULL,
    created_by  uuid        NOT NULL REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deal_notes_deal_idx ON deal_notes (deal_id, created_at DESC);

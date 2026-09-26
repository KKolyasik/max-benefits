-- Cards shown to students. A card is stored whole, in the same format as in
-- data/knowledge.yaml; position keeps the order they were added in, which
-- breaks ties between cards of the same priority.
CREATE TABLE cards (
    id         text PRIMARY KEY,
    card       jsonb NOT NULL,
    position   bigserial NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- MAX user ID of the admin who approved the last change; NULL for the
    -- initial import.
    updated_by bigint
);

CREATE INDEX cards_categories ON cards USING gin ((card -> 'categories'));

-- Every version a card has had: the base is no longer in git, so this is its
-- history and the way to see who changed what.
CREATE TABLE card_history (
    id         bigserial PRIMARY KEY,
    card_id    text NOT NULL,
    card       jsonb NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now(),
    changed_by bigint,
    -- The draft the change came from, if any.
    draft_id   bigint,
    note       text NOT NULL DEFAULT ''
);

CREATE INDEX card_history_card ON card_history (card_id, changed_at);

-- Cards proposed by the agent, waiting for an admin.
CREATE TABLE drafts (
    id          bigserial PRIMARY KEY,
    -- Identifies the draft at the source, so a draft delivered twice is
    -- stored once.
    external_id text NOT NULL UNIQUE,
    card        jsonb NOT NULL,
    updates     text NOT NULL DEFAULT '',
    query       text NOT NULL DEFAULT '',
    sources     jsonb NOT NULL DEFAULT '[]',
    notes       jsonb NOT NULL DEFAULT '[]',
    found_at    timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    decided_at  timestamptz,
    decided_by  bigint
);

CREATE INDEX drafts_pending ON drafts (id) WHERE status = 'pending';

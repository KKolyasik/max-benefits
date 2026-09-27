-- What the bot must tell the agent through Kafka. A row is written in the
-- same transaction as the change, so Kafka never misses a change even if it
-- is down at the moment; the relay publishes the current state and deletes
-- the row.
CREATE TABLE outbox (
    id         bigserial PRIMARY KEY,
    -- 'card': the card ref changed; 'decision': the draft ref was decided.
    kind       text NOT NULL CHECK (kind IN ('card', 'decision')),
    ref        text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Drafts come through Kafka now, and the agent can't see which of them wait
-- for review: a newer draft of a card replaces the one still pending.
ALTER TABLE drafts DROP CONSTRAINT drafts_status_check;
ALTER TABLE drafts ADD CONSTRAINT drafts_status_check
    CHECK (status IN ('pending', 'approved', 'rejected', 'superseded'));

-- What students write to the team from the bot. Admins read it there and
-- mark it resolved.
CREATE TABLE feedback (
    id          bigserial PRIMARY KEY,
    -- MAX user ID of the author.
    user_id     bigint NOT NULL,
    -- What the feedback is about: a question with no fitting option has both,
    -- a category where nothing was found has only the category, and an idea
    -- from the menu has neither.
    category    text NOT NULL DEFAULT '',
    question    text NOT NULL DEFAULT '',
    -- The answers to the questions of the category where nothing was found;
    -- emptied when the user deletes their data.
    answers     jsonb NOT NULL DEFAULT '{}',
    text        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolved_by bigint
);

CREATE INDEX feedback_open ON feedback (id) WHERE resolved_at IS NULL;
-- For the daily limit of a user.
CREATE INDEX feedback_user ON feedback (user_id, created_at);

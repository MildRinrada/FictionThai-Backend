-- +migrate Up

-- คลังคำทั้งบัญชี: terms a writer uses across EVERY fiction - the fandom's
-- proper nouns, recurring character names. A writer with twenty stories in one
-- fandom teaches the spellchecker once, here, instead of once per story
-- (assistant-settings review, 2026-08).
--
-- A separate table rather than novel_lexicon with a NULL novel_id, so the
-- per-fiction bank's constraints and cascade semantics stay untouched. Applied
-- at check time alongside the fiction's own bank.
CREATE TABLE user_lexicon (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    term       VARCHAR(120) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_lexicon_unique_term
    ON user_lexicon (user_id, lower(term));

-- +migrate Down
DROP TABLE user_lexicon;

-- Phase 13H - Reader variables: a table, not a toggle
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13H, supersedes the 12B schema).
--
-- 12B shipped one switch and one token (novels.yn_enabled, novels.yn_token).
-- That was the right first step and this is its general form: a fiction declares
-- as many variables as it needs, each with a token the author types into the
-- text, a label the reader answers, and a kind.
--
-- The rule that does NOT change, and is the reason any of this is safe:
--
--   Never substitute at save. Store tokens; resolve at render.
--
-- Substituted text could never be renamed afterwards, and every reader would see
-- one reader's name baked into the cached page. Nothing in this migration can
-- modify chapter content, and nothing downstream may either.
--
-- The reader's ANSWERS are still absent from this schema, exactly as in 12B:
-- what name someone inserts into a romance is a sensitive preference, it lives
-- in the reader's browser, and keeping it there is also what lets a guest use
-- the feature at all (docs/11 data minimisation, docs/10 §2.1).

-- +migrate Up

CREATE TABLE novel_variables (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    novel_id      UUID NOT NULL REFERENCES novels (id) ON DELETE CASCADE,

    -- The literal placeholder the author types, e.g. (y/n). Matched LITERALLY
    -- at render, never compiled as a regular expression, so an author cannot
    -- write a token that turns into a catastrophic pattern.
    token         VARCHAR(32) NOT NULL,

    -- What the READER is asked, e.g. "ชื่อของคุณ".
    label         VARCHAR(64) NOT NULL,

    -- What the text shows before the reader answers. NULL means the platform's
    -- neutral fallback.
    default_value VARCHAR(64) NULL,

    kind          VARCHAR(16) NOT NULL,

    -- kind = choice : {"values": ["เขา", "เธอ"]}
    -- kind = pronoun: {"forms": ["ประธาน", "เจ้าของ"],
    --                  "sets": [{"label": "เขา", "values": ["เขา", "ของเขา"]}]}
    -- A pronoun carries a whole linked set, which is what lets one declaration
    -- serve readers of any gender without the writer maintaining three versions
    -- of the text.
    options       JSONB NULL,

    -- 0-based and contiguous, assigned by the API from array order - never
    -- accepted from a client, the same rule chat messages and entries follow.
    position      INTEGER NOT NULL,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT novel_variables_token_key UNIQUE (novel_id, token),
    CONSTRAINT novel_variables_position_key UNIQUE (novel_id, position),
    CONSTRAINT novel_variables_kind_valid CHECK (kind IN ('text', 'choice', 'pronoun')),
    CONSTRAINT novel_variables_position_valid CHECK (position >= 0),
    CONSTRAINT novel_variables_token_present CHECK (btrim(token) <> ''),
    CONSTRAINT novel_variables_label_present CHECK (btrim(label) <> '')
);

CREATE INDEX novel_variables_novel_idx ON novel_variables (novel_id, position);

COMMENT ON TABLE novel_variables IS
    'Reader variables declared by a fiction (13H). The reader''s ANSWERS are never '
    'stored here or anywhere server-side - they live in the reader''s browser.';

-- Carry every existing y/n fiction across. A writer who turned the feature on
-- must find it still on, with the token they chose, after this deploys.
INSERT INTO novel_variables (novel_id, token, label, default_value, kind, position)
SELECT id, yn_token, 'ชื่อของคุณ', 'คุณ', 'text', 0
FROM novels
WHERE yn_enabled = TRUE AND yn_token IS NOT NULL AND btrim(yn_token) <> '';

-- The old columns go only AFTER the data above is in its new home, and the Down
-- migration below puts it back, so this is reversible rather than destructive.
ALTER TABLE novels DROP CONSTRAINT IF EXISTS novels_yn_token_present;
ALTER TABLE novels
    DROP COLUMN IF EXISTS yn_token,
    DROP COLUMN IF EXISTS yn_enabled;

-- +migrate Down

ALTER TABLE novels
    ADD COLUMN yn_enabled BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN yn_token   VARCHAR(32) NULL;

-- Restore from the first text variable of each fiction. A fiction that declared
-- several keeps only the first here, because the old schema could hold only
-- one - which is why this direction is a rollback, not a round trip.
UPDATE novels n
SET yn_enabled = TRUE,
    yn_token   = v.token
FROM (
    SELECT DISTINCT ON (novel_id) novel_id, token
    FROM novel_variables
    WHERE kind = 'text'
    ORDER BY novel_id, position
) v
WHERE v.novel_id = n.id;

ALTER TABLE novels
    ADD CONSTRAINT novels_yn_token_present
    CHECK (NOT yn_enabled OR yn_token IS NOT NULL);

DROP TABLE IF EXISTS novel_variables;

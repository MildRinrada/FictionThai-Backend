-- Phase 13A - What the create form actually asks
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13A, §13B).
--
-- Two fields join the create form, and both earn their place by being things
-- that cannot be answered later without the answer having already mattered:
--
--   * age_rating decides where the work may appear at all. It is required by
--     the API on create - the column default exists so EXISTING rows are valid,
--     not as a way for a new fiction to skip the question.
--
--   * origin_type is the single field that separates fanfiction from original
--     work in search. Everything else about discovery (genres, tags, synopsis)
--     leaves the create form and moves to the fiction's settings, because an
--     unclassified draft is already fine and a synopsis written before the
--     first sentence is a worse synopsis.
--
-- age_gate is stored now although nothing reads it until 13B. It is the
-- writer's choice of how 18+ work is gated - ID-verified readers only, or a
-- warning shown before every read - and it is kept regardless of the current
-- rating so that moving a work to 18+ and back does not lose the setting.
--
-- No cross-column CHECK ties age_gate to age_rating, for the same reason
-- docs/08 §2.4 forbids cross-dimension format constraints: the columns answer
-- different questions and a constraint here would only make a legal edit
-- sequence fail.

-- +migrate Up

ALTER TABLE novels
    ADD COLUMN age_rating  VARCHAR(16)  NOT NULL DEFAULT 'general',
    ADD COLUMN age_gate    VARCHAR(16)  NOT NULL DEFAULT 'warning',
    ADD COLUMN origin_type VARCHAR(16)  NOT NULL DEFAULT 'original',
    ADD COLUMN fandom      VARCHAR(120) NULL,

    ADD CONSTRAINT novels_age_rating_valid
        CHECK (age_rating IN ('general', 'teen', 'mature')),
    ADD CONSTRAINT novels_age_gate_valid
        CHECK (age_gate IN ('warning', 'verified')),
    ADD CONSTRAINT novels_origin_type_valid
        CHECK (origin_type IN ('original', 'fanfiction')),

    -- A fandom name belongs only to a fanfiction; original work naming a source
    -- would be a contradiction the search page could not present.
    ADD CONSTRAINT novels_fandom_requires_fanfiction
        CHECK (fandom IS NULL OR origin_type = 'fanfiction');

COMMENT ON COLUMN novels.age_rating IS
    'The author''s statement about their own work. Required by the API on create.';
COMMENT ON COLUMN novels.age_gate IS
    'How 18+ work is gated (13B). Only consulted when age_rating = mature.';

-- Listings exclude 18+ by default, so the predicate needs the column indexed
-- alongside the visibility filter every public listing already applies.
CREATE INDEX novels_rating_idx ON novels (age_rating)
    WHERE deleted_at IS NULL AND visibility = 'public';

-- "Fanfiction of X" is a browse surface; original work never queries it.
CREATE INDEX novels_fandom_idx ON novels (fandom) WHERE fandom IS NOT NULL;

-- +migrate Down

DROP INDEX IF EXISTS novels_fandom_idx;
DROP INDEX IF EXISTS novels_rating_idx;

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_fandom_requires_fanfiction,
    DROP CONSTRAINT IF EXISTS novels_origin_type_valid,
    DROP CONSTRAINT IF EXISTS novels_age_gate_valid,
    DROP CONSTRAINT IF EXISTS novels_age_rating_valid;

ALTER TABLE novels
    DROP COLUMN IF EXISTS fandom,
    DROP COLUMN IF EXISTS origin_type,
    DROP COLUMN IF EXISTS age_gate,
    DROP COLUMN IF EXISTS age_rating;

-- Phase 13K - ตั้งค่าเพิ่มเติม: the collapsed section the create form was
-- always specified to have, and the author-permission group
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13E, §13K).
--
-- 13A cut the create form to six visible fields on the understanding that the
-- rest lived under a COLLAPSED "ตั้งค่าเพิ่มเติม" section, closed by default.
-- The six shipped; the section did not. These columns are what it needs.
--
-- The permission group is the honest form of "ห้ามแคปจอ". Preventing a
-- screenshot is not technically possible, so a checkbox promising it would be a
-- promise the platform cannot keep. Every flag here is the AUTHOR'S STATED
-- PERMISSION, rendered to readers as a rights card that says so in as many
-- words. Nothing in this migration enforces anything, and nothing downstream
-- may claim it does (docs/11 §43, §13E).

-- +migrate Up

ALTER TABLE novels
    -- The work's language. 'th' for everything that exists today; the column
    -- is here so a future non-Thai fiction is a value, not a schema change.
    ADD COLUMN language VARCHAR(8) NOT NULL DEFAULT 'th',

    -- What this fiction calls a chapter: ตอน, บท, ท่อน, EP. Cheap, and writers
    -- ask for it constantly - a "บท" labelled "ตอน" reads as someone else's
    -- book.
    ADD COLUMN chapter_unit VARCHAR(16) NOT NULL DEFAULT 'ตอน',

    -- Author notes, separate from the synopsis. Without their own field they
    -- end up inside the synopsis, where search and cards then show them.
    ADD COLUMN author_note_start TEXT NULL,
    ADD COLUMN author_note_end   TEXT NULL,

    -- Series membership, deliberately DENORMALISED for now: a name plus a
    -- position, grouped by (author_id, series_name). A series table would need
    -- its own ownership, ordering, and rename semantics; this answers "ภาค 2
    -- ของเรื่องเดียวกัน" today and normalises later without data loss.
    ADD COLUMN series_name     VARCHAR(120) NULL,
    ADD COLUMN series_position INTEGER NULL,

    -- Comments, on or off for the whole fiction. TWO values, not three: guest
    -- commenting does not exist yet (13D), so a "members only" option would be
    -- identical to "on" and the interface would be describing a distinction it
    -- does not make.
    ADD COLUMN comments_enabled BOOLEAN NOT NULL DEFAULT TRUE,

    -- The author's permissions (§13E). DECLARATIONS, not enforcement.
    ADD COLUMN allow_screenshot  BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN allow_translation BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN allow_derivative  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN allow_audio       BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN require_credit    BOOLEAN NOT NULL DEFAULT TRUE,
    -- Free text beside allow_derivative: "ทำต่อได้ แต่บอกก่อน", and similar.
    ADD COLUMN derivative_terms  VARCHAR(280) NULL,

    ADD CONSTRAINT novels_series_position_valid
        CHECK (series_position IS NULL OR series_position > 0),
    -- A position without a series is a number about nothing.
    ADD CONSTRAINT novels_series_position_needs_name
        CHECK (series_position IS NULL OR series_name IS NOT NULL),
    ADD CONSTRAINT novels_chapter_unit_present
        CHECK (btrim(chapter_unit) <> '');

COMMENT ON COLUMN novels.allow_screenshot IS
    'The author''s stated permission, NOT a technical restriction. Screenshots '
    'cannot be prevented and the platform must never claim otherwise (13E).';
COMMENT ON COLUMN novels.chapter_unit IS
    'What this fiction calls a chapter, e.g. ตอน / บท / EP. Presentation only.';
COMMENT ON COLUMN novels.series_name IS
    'Denormalised series membership, grouped by (author_id, series_name) until a '
    'series table earns its place (13K).';

-- "Other parts of this series" is an author-scoped browse, so the index is too.
CREATE INDEX novels_series_idx ON novels (author_id, series_name, series_position)
    WHERE series_name IS NOT NULL AND deleted_at IS NULL;

-- +migrate Down

DROP INDEX IF EXISTS novels_series_idx;

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_chapter_unit_present,
    DROP CONSTRAINT IF EXISTS novels_series_position_needs_name,
    DROP CONSTRAINT IF EXISTS novels_series_position_valid;

ALTER TABLE novels
    DROP COLUMN IF EXISTS derivative_terms,
    DROP COLUMN IF EXISTS require_credit,
    DROP COLUMN IF EXISTS allow_audio,
    DROP COLUMN IF EXISTS allow_derivative,
    DROP COLUMN IF EXISTS allow_translation,
    DROP COLUMN IF EXISTS allow_screenshot,
    DROP COLUMN IF EXISTS comments_enabled,
    DROP COLUMN IF EXISTS series_position,
    DROP COLUMN IF EXISTS series_name,
    DROP COLUMN IF EXISTS author_note_end,
    DROP COLUMN IF EXISTS author_note_start,
    DROP COLUMN IF EXISTS chapter_unit,
    DROP COLUMN IF EXISTS language;

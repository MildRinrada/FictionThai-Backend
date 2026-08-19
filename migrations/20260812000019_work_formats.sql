-- Phase 13J - รูปแบบผลงาน: the work format, and where it is decided
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13J, docs/CONTENT-MODEL.md §2,
--  delivers docs/PHASE-12-STORY-DEPTH.md §12F).
--
-- Three changes, one idea: the presentation selector moves DOWN to the chapter,
-- and headcanon stops being a badge and becomes a stored representation.
--
--   * chapters.presentation_format is NULLABLE and NULL means "follow the
--     fiction". This is the load-bearing detail. A NOT NULL column stamped at
--     creation would force a fiction-level format change to rewrite every
--     chapter row to follow it - turning a metadata update into a mass write
--     over content rows, which is the shape of change docs/08 §43 Rule 7
--     forbids. With NULL-as-inherit, a fiction-level change still moves every
--     chapter that never declared otherwise and writes exactly one row.
--
--   * novels.mixed_formats says a chapter MAY declare its own format. It is a
--     separate column rather than a fourth value of presentation_format,
--     because under "mixed" the fiction-level format still has a job: it is
--     what an inheriting chapter renders as, and the value the per-chapter
--     picker opens on. 'mixed' as a presentation_format would have been a value
--     meaning "ask elsewhere", leaving every inheriting chapter with no answer.
--
--   * chapter_entries is the third representation, exactly as 12F specified: a
--     headcanon topic is a chapter, and its entries are rows beside the prose
--     and the conversation. A chapter may hold all three at once and changing
--     format writes none of them.

-- +migrate Up

ALTER TABLE novels
    ADD COLUMN mixed_formats BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN novels.mixed_formats IS
    'Whether a chapter may declare its own presentation_format (13J). The '
    'fiction''s own presentation_format remains the fallback for chapters that '
    'declare nothing.';

-- The fiction-level format widens to include headcanon. Dropping and recreating
-- the CHECK is the only way to widen it; existing rows all satisfy the new one.
ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_presentation_format_valid;
ALTER TABLE novels
    ADD CONSTRAINT novels_presentation_format_valid
        CHECK (presentation_format IN ('standard', 'chat', 'headcanon'));

ALTER TABLE chapters
    ADD COLUMN presentation_format VARCHAR(32) NULL,
    -- The topic's field labels, defined per topic - and a topic is a chapter
    -- (12F). An ordered array of strings; the entries carry the values.
    ADD COLUMN entry_fields JSONB NOT NULL DEFAULT '[]'::jsonb,

    ADD CONSTRAINT chapters_presentation_format_valid
        CHECK (presentation_format IS NULL
               OR presentation_format IN ('standard', 'chat', 'headcanon'));

COMMENT ON COLUMN chapters.presentation_format IS
    'What THIS chapter renders as. NULL means follow novels.presentation_format '
    '(13J). Declaring a format converts nothing: the other representations stay '
    'exactly where they are.';

-- A revision snapshots EVERY representation, not just the active one
-- (docs/CONTENT-MODEL.md §5), so entries need their place in it. Columns on the
-- existing row rather than a reshape of `messages`: one row still commits or
-- does not, and every revision already written keeps exactly the meaning it was
-- written with.
ALTER TABLE chapter_revisions
    ADD COLUMN entries      JSONB NULL,
    ADD COLUMN entry_fields JSONB NULL;

CREATE TABLE chapter_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chapter_id   UUID NOT NULL REFERENCES chapters (id) ON DELETE CASCADE,

    -- 0-based and contiguous, assigned by the API from array order. Never
    -- accepted from a client, so a gap or a duplicate is not representable
    -- (docs/CONTENT-MODEL.md §4, the same rule chat messages follow).
    position     INTEGER NOT NULL,

    -- The character this entry is about, when there is a record for them.
    -- SET NULL with the name denormalised beside it: an entry may name someone
    -- who has no character record, and deleting a character must empty a link
    -- rather than delete an author's paragraph (12F).
    character_id UUID NULL REFERENCES characters (id) ON DELETE SET NULL,
    name         VARCHAR(120) NOT NULL,

    -- Values for the chapter's entry_fields, positionally. Named field_values
    -- rather than 12F's `values` because VALUES is reserved in PostgreSQL and
    -- every query touching it would need quoting forever.
    field_values JSONB NOT NULL DEFAULT '[]'::jsonb,

    -- Uncapped by design. 12F is explicit that headcanon entry length is
    -- unknown by nature.
    body         TEXT NOT NULL DEFAULT '',

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chapter_entries_position_key UNIQUE (chapter_id, position),
    CONSTRAINT chapter_entries_position_valid CHECK (position >= 0)
);

-- Entries are always read as a whole chapter, in order - the same access shape
-- chapter_messages has.
CREATE INDEX chapter_entries_chapter_idx
    ON chapter_entries (chapter_id, position);

COMMENT ON TABLE chapter_entries IS
    'Headcanon entries - the third chapter representation beside chapters.content '
    'and chapter_messages (12F, 13J). Never derived from either, never deleted '
    'when either is used.';

-- +migrate Down

DROP TABLE IF EXISTS chapter_entries;

ALTER TABLE chapter_revisions
    DROP COLUMN IF EXISTS entry_fields,
    DROP COLUMN IF EXISTS entries;

ALTER TABLE chapters
    DROP CONSTRAINT IF EXISTS chapters_presentation_format_valid;

ALTER TABLE chapters
    DROP COLUMN IF EXISTS entry_fields,
    DROP COLUMN IF EXISTS presentation_format;

-- Narrow the fiction-level format back. Any row already on 'headcanon' would
-- fail the old CHECK, so it is moved to the value that renders its prose - the
-- content itself is untouched either way.
UPDATE novels SET presentation_format = 'standard'
    WHERE presentation_format = 'headcanon';

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_presentation_format_valid;
ALTER TABLE novels
    ADD CONSTRAINT novels_presentation_format_valid
        CHECK (presentation_format IN ('standard', 'chat'));

ALTER TABLE novels
    DROP COLUMN IF EXISTS mixed_formats;

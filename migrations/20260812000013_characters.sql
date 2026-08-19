-- Phase 12A - Characters (docs/PHASE-12-STORY-DEPTH.md §12A).
--
-- The fiction page gives its first section to the cast, and headcanon entries are
-- per character. Neither had anywhere to live: nothing in the schema described a
-- character at all.
--
-- Design:
--
--   * A character BELONGS TO one fiction. It is authored content, not derived
--     from the text, so it is owned by the writer and inherits the fiction's
--     visibility gate exactly - a character of a private fiction is as invisible
--     as the fiction (docs/11 §31). ON DELETE CASCADE follows from that: the cast
--     of a deleted fiction is not independently meaningful.
--
--   * `details` is JSONB rather than columns. The prototype's fields (ชื่อเต็ม,
--     อายุ, อาชีพ, ความสัมพันธ์, สถานะ) are EXAMPLES, not a schema - a fantasy
--     fiction wants different labels than a school romance. It stores an ordered
--     array of {label, value} objects, both plain text, validated and capped in
--     the service. It is never rendered as markup (docs/11 §17).
--
--   * `traits` is JSONB rather than a join table or a TEXT[]: trait chips are
--     free text belonging to one character, never shared, never filtered across
--     fictions, so a join table would add an entity for no query. JSONB rather
--     than TEXT[] keeps it on the same encoding path as `details` and as the
--     existing JSONB columns (chapter_messages.metadata), instead of introducing
--     a driver-level array type for one column.
--
--   * character_appearances is BOTH "ฉากที่ปรากฏ" and the studio timeline. The
--     prototype drew a free-standing event list with its own titles; every event
--     it showed was really "this happened in this chapter", so ordering
--     appearances by chapter number produces the same reading with no second
--     content model to keep consistent.

-- +migrate Up

CREATE TABLE characters (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    novel_id         UUID         NOT NULL REFERENCES novels (id) ON DELETE CASCADE,

    name             VARCHAR(120) NOT NULL,
    -- "ตัวละครหลัก · ผู้เล่าเรื่อง" - the author's own words, not an enum.
    role             VARCHAR(120) NULL,
    -- The single line shown on the card in the cast row.
    summary          VARCHAR(300) NULL,
    avatar_url       TEXT         NULL,
    -- Backstory. Long-form, shown only in the expanded panel.
    description      TEXT         NULL,
    -- The coral-ruled catchphrase.
    quote            VARCHAR(500) NULL,

    traits           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    details          JSONB        NOT NULL DEFAULT '[]'::jsonb,

    -- Author ordering. The cast row is a curated sequence, not alphabetical.
    position         INTEGER      NOT NULL,

    -- "ปรากฏครั้งแรก". SET NULL rather than CASCADE: deleting a chapter must not
    -- delete the character who first appeared in it.
    first_chapter_id UUID         NULL REFERENCES chapters (id) ON DELETE SET NULL,

    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Listing a fiction's cast in author order is the only read path that matters.
CREATE INDEX characters_novel_position_idx ON characters (novel_id, position);

-- Two characters cannot occupy the same slot. DEFERRABLE so a reorder can write
-- the whole new sequence in one transaction without shuffling through a gap.
ALTER TABLE characters
    ADD CONSTRAINT characters_novel_position_unique
    UNIQUE (novel_id, position) DEFERRABLE INITIALLY DEFERRED;

COMMENT ON COLUMN characters.details IS
    'Ordered array of {label, value} plain-text pairs. Author-defined labels: there is deliberately no fixed field schema.';

CREATE TABLE character_appearances (
    character_id UUID        NOT NULL REFERENCES characters (id) ON DELETE CASCADE,
    chapter_id   UUID        NOT NULL REFERENCES chapters (id)   ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (character_id, chapter_id)
);

-- The timeline read: every appearance in one fiction, ordered by chapter.
CREATE INDEX character_appearances_chapter_idx ON character_appearances (chapter_id);

-- +migrate Down

DROP TABLE IF EXISTS character_appearances;
DROP TABLE IF EXISTS characters;

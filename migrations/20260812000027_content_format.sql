-- Phase 13N - the writer editor's content model
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13N, answers docs/CONTENT-MODEL.md §3's
--  open question, delivers docs/01 §18 and docs/04 §8's formatting list).
--
-- CONTENT-MODEL.md §3 fixed chapter content as plain text and named exactly what
-- a richer model would have to bring with it: "which markup, which sanitizer,
-- which content_format discriminator column". This is that column.
--
-- Design notes:
--
--   * The DEFAULT is 'plain', and nothing is backfilled. That is the whole point
--     of having a discriminator at all: every chapter already written was
--     written as plain text, and turning markup on underneath it would silently
--     reinterpret an author's own words - a line beginning "- " becoming a list,
--     *เน้นเสียง* becoming italics. Reinterpreting stored work without the author
--     asking is the transformation docs/08 §43 Rule 7 and the project's
--     writer-first rules forbid. New chapters are created as 'markdown' by the
--     service; an existing one moves only when its author says so, and the move
--     writes no content in either direction.
--
--   * There is no sanitizer, because there is nothing to sanitize. The stored
--     value stays text and the RENDERER builds React elements from it - never
--     dangerouslySetInnerHTML, never an HTML subset that has to be trusted
--     forever (docs/11 §17, docs/13 §38). The markup is a strict superset of
--     plain text, so §3's promise holds: nothing is migrated destructively.
--
--   * chapter_revisions carries the format too. A revision restores the complete
--     authored state (docs/CONTENT-MODEL.md §5), and text restored under the
--     wrong format would render as something the author never wrote. NULL there
--     means a revision taken before this migration, which was necessarily plain.

-- +migrate Up

ALTER TABLE chapters
    ADD COLUMN content_format VARCHAR(16) NOT NULL DEFAULT 'plain',
    ADD CONSTRAINT chapters_content_format_valid
        CHECK (content_format IN ('plain', 'markdown'));

COMMENT ON COLUMN chapters.content_format IS
    'How chapters.content is rendered (13N). plain = literal text, the pre-13N '
    'model and the default for every row written before it. markdown = the '
    'restricted subset the writer editor produces. Changing it writes no '
    'content and requires the author''s own action.';

ALTER TABLE chapter_revisions
    ADD COLUMN content_format VARCHAR(16) NULL;

COMMENT ON COLUMN chapter_revisions.content_format IS
    'The format the snapshotted content was written under (13N). NULL is a '
    'revision taken before the column existed, which was necessarily plain.';

-- +migrate Down

ALTER TABLE chapter_revisions
    DROP COLUMN IF EXISTS content_format;

ALTER TABLE chapters
    DROP CONSTRAINT IF EXISTS chapters_content_format_valid;

-- The manuscripts are untouched: dropping the column returns every chapter to
-- being rendered as literal text, which is what the pre-13N reader did with it.
-- The markers an author typed stay in their text, visible rather than applied.
ALTER TABLE chapters
    DROP COLUMN IF EXISTS content_format;

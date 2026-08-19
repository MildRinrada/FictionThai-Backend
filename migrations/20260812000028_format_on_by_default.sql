-- Phase 13N (revised) - formatting is on from the start
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13N).
--
-- 13N shipped `content_format` defaulting to 'plain' with nothing backfilled,
-- and put an offer in the editor: "เปิดการจัดรูปแบบ". That was the cautious
-- reading of the rule against reinterpreting stored work, and it was the wrong
-- product decision - it made a writer opt in to the editor the platform is
-- built around, one chapter at a time, and it put a banner in front of the
-- writing surface on every chapter that had not.
--
-- The caution is no longer buying anything, because the editor is now WYSIWYG.
-- A writer never types `**` and never sees it: they select and press bold, and
-- what the surface serialises is the marked-up text this column describes.
-- 'plain' would mean the editor showing bold and the reader showing asterisks -
-- the two disagreeing about the same chapter, which is worse than either.
--
-- So: the default flips, and existing rows move with it. The manuscripts are
-- NOT touched - not one byte is written to chapters.content here. What changes
-- is how the reader reads them, which is exactly what a discriminator is for.
-- 'plain' remains a valid value, and the reader still honours it, so an import
-- path that needs literal text has one.

-- +migrate Up

ALTER TABLE chapters
    ALTER COLUMN content_format SET DEFAULT 'markdown';

UPDATE chapters SET content_format = 'markdown' WHERE content_format = 'plain';

COMMENT ON COLUMN chapters.content_format IS
    'How chapters.content is rendered (13N). markdown = the restricted subset '
    'the WYSIWYG editor produces, and the default for every chapter. plain = '
    'literal text, kept as a valid value for imported work; no editor surface '
    'produces it today. Changing it writes no content.';

-- +migrate Down

-- Back to the cautious default. Existing rows are left on 'markdown': moving
-- them to 'plain' would make every chapter render its own markers, which is a
-- change to what readers see and not something a down migration should decide.
ALTER TABLE chapters
    ALTER COLUMN content_format SET DEFAULT 'plain';

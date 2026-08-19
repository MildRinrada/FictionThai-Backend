-- Phase 13J revised - every fiction can mix; "mixed" is an observation, not a
-- setting (docs/PHASE-13-CREATION-AND-CONTROL.md §13J).
--
-- 13J shipped `novels.mixed_formats` as a gate: a chapter could declare its own
-- presentation only while the flag was on. That was wrong, and the review that
-- caught it put the argument better than the original decision did:
--
--   * a writer who picked "ฟิคล้วน" can already mix, simply by changing a
--     chapter later - so the flag was not preventing anything;
--   * the create form's fourth card therefore changed nothing in the system
--     while telling the writer the other three had locked something.
--
-- A setting that changes nothing but implies a lock is worse than no setting.
-- So the gate goes. `chapters.presentation_format` is now honoured whenever it
-- is set, `novels.presentation_format` is what a chapter that declares nothing
-- renders as, and "ผสมรูปแบบ" is DERIVED from the chapters that actually exist.
--
-- No chapter loses its declared format: the column that decided whether to READ
-- those declarations is what is being removed, and every one of them is now
-- read unconditionally.

-- +migrate Up

ALTER TABLE novels DROP COLUMN IF EXISTS mixed_formats;

-- +migrate Down

ALTER TABLE novels
    ADD COLUMN mixed_formats BOOLEAN NOT NULL DEFAULT FALSE;

-- Restoring the gate must not silently stop honouring a declaration a writer
-- already made, so any fiction whose chapters disagree with it comes back with
-- the flag ON.
UPDATE novels n
SET mixed_formats = TRUE
WHERE EXISTS (
    SELECT 1 FROM chapters c
    WHERE c.novel_id = n.id
      AND c.deleted_at IS NULL
      AND c.presentation_format IS NOT NULL
      AND c.presentation_format <> n.presentation_format
);

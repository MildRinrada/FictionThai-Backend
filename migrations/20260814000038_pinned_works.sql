-- ชั้นวางเรื่องที่ปักหมุด (docs/PROFILE-AND-ACHIEVEMENTS.md).
--
-- Three works the writer chooses to put at the top of their own profile, each
-- with one line of their own words - "เริ่มที่เรื่องนี้". A profile ordered
-- only by recency answers "what did you write last", never "where should I
-- start", and the second question is the one a new reader actually has.
--
-- Stored on user_profiles rather than as a column on novels: the order and the
-- reason belong to the PROFILE, not to the fiction, and a work that stops
-- being readable must silently drop out of the list rather than leave a hole
-- in a table. The listing query filters through novels.ReadableSQL, so an
-- unpublished or deleted pin simply does not render.

-- +migrate Up

ALTER TABLE user_profiles
    ADD COLUMN pinned JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN user_profiles.pinned IS
    'Up to 3 pinned works, in the owner''s order: [{"novel_id":"…","note":"เริ่มที่เรื่องนี้"}]. Opaque-but-validated JSON (the ai_prefs precedent); readability is re-checked at read time, never trusted from here.';

-- +migrate Down

ALTER TABLE user_profiles DROP COLUMN IF EXISTS pinned;

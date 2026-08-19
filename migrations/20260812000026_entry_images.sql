-- Phase 13M - a picture on a headcanon entry
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13M, extends docs/PHASE-12-STORY-DEPTH.md §12F).
--
-- Design notes:
--
--   * A URL on the entry row rather than a media id, for the same reason
--     novels.cover_url and characters.avatar_url are URLs: the owning domain
--     stores the reference it renders, and the media table stays the record of
--     BYTES. A join per entry to turn an id back into the same string would buy
--     nothing, and headcanon topics are read as a whole chapter.
--
--   * NULL is the norm and stays the norm. Every entry written before this
--     migration renders exactly as it did, and an entry never needs an image to
--     be complete - 12F's shape is a name, optional field values, and a body of
--     unknown length. The image is a fourth, optional thing.
--
--   * TEXT rather than a bounded VARCHAR, matching every other stored URL here.
--     The length bound is the service's (docs/10 §27): a CHECK would refuse the
--     write with a constraint violation instead of the field-level 422 the API
--     contract promises.

-- +migrate Up

ALTER TABLE chapter_entries
    ADD COLUMN image_url TEXT NULL;

COMMENT ON COLUMN chapter_entries.image_url IS
    'An image the author attached to this entry (13M). NULL is the norm. '
    'Uploaded through the media endpoint with purpose=entry_image; this column '
    'holds the served URL, never a storage key.';

-- +migrate Down

-- Dropping the column drops the author's attachments. The stored OBJECTS are
-- untouched - they remain in the media table and keep serving - so a re-applied
-- Up plus a restore of these references is a complete recovery.
ALTER TABLE chapter_entries
    DROP COLUMN IF EXISTS image_url;

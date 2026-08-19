-- Phase 12C - Engagement counters (docs/PHASE-12-STORY-DEPTH.md §12C).
--
-- Every card in the redesign carries a read count, and the fiction page shows
-- ผู้อ่าน / ถูกใจ / บันทึกไว้อ่าน. None of those numbers had a source, which is
-- why the frontend shipped without them: a fabricated figure on a fiction page is
-- a claim about someone's work.
--
-- Design:
--
--   * The counters are DENORMALISED onto novels and maintained by the events that
--     already exist. A COUNT(*) over bookmarks on every card render would put a
--     scan on the hottest read path in the product (docs/07 §67).
--
--   * bookmark_count and like_count are written in the SAME transaction as the
--     row they count, so they cannot drift from it. Both are backfilled below.
--
--   * view_count is a BIGINT and is written by a background worker draining a
--     buffer, never on the request path - opening a chapter must not take a
--     database write to render.
--
--   * De-duplication is per reader, per fiction, per DAY, and lives in Redis, not
--     here. The key is derived from the session id for a member and from a
--     salted, daily-rotating hash of the IP for a guest; the salt is never
--     stored, so the key cannot be reversed into a reading history. docs/11 §34
--     does not permit building one, and the studio's own privacy note
--     ("เราไม่เก็บสถิติการอ่านรายบุคคล") has to stay true.
--
--   * novel_reactions gives ถูกใจ a source. docs/01 §20.2 lists Like as a
--     supported lightweight reaction; one row per user per fiction, so a like is
--     idempotent and cannot be farmed by repeat submission.

-- +migrate Up

ALTER TABLE novels
    ADD COLUMN view_count     BIGINT  NOT NULL DEFAULT 0,
    ADD COLUMN like_count     INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN bookmark_count INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN novels.view_count IS
    'Display counter only, de-duplicated per reader per day before it is incremented. Never a per-reader reading record.';

-- Fiction likes (docs/01 §20.2). One row per user per fiction makes the action
-- idempotent; the counter above is maintained alongside it.
CREATE TABLE novel_reactions (
    novel_id   UUID        NOT NULL REFERENCES novels (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id)  ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (novel_id, user_id)
);

-- "Which fictions has this reader liked" - the reader's own state on a page.
CREATE INDEX novel_reactions_user_idx ON novel_reactions (user_id);

-- Backfill from the rows that already exist, so the counters are correct the
-- moment the column appears rather than after the first write to each fiction.
UPDATE novels
SET bookmark_count = COALESCE(counts.total, 0)
FROM (
    SELECT novel_id, COUNT(*) AS total FROM bookmarks GROUP BY novel_id
) AS counts
WHERE novels.id = counts.novel_id;

-- +migrate Down

DROP TABLE IF EXISTS novel_reactions;

ALTER TABLE novels DROP COLUMN IF EXISTS bookmark_count;
ALTER TABLE novels DROP COLUMN IF EXISTS like_count;
ALTER TABLE novels DROP COLUMN IF EXISTS view_count;

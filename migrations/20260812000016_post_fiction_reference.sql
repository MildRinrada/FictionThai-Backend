-- Phase 12D - Community post → fiction reference
-- (docs/PHASE-12-STORY-DEPTH.md §12D; docs/06 §34 anticipates the card).
--
-- A post may point at ONE fiction, and optionally at one chapter inside it.
-- That is the whole feature: the composer attaches a chapter, and the feed
-- renders a card for it.
--
-- Design:
--
--   * ON DELETE SET NULL, never CASCADE. Deleting a fiction must not delete
--     other people's posts ABOUT it - the post is their writing, not the
--     fiction author's (docs/11 §37, and the ownership principle generally).
--     A post whose fiction is gone survives as a post with no card.
--
--   * The CHECK keeps the pair coherent: a chapter reference without its
--     fiction cannot exist. The chapter BELONGING to that fiction is enforced
--     by the service on write and by the join on read - a composite FK would
--     need a redundant unique key on (id, novel_id) to express here.
--
--   * Nothing about the referenced fiction is COPIED. No title, no cover, no
--     visibility snapshot. The API resolves the reference against the READER
--     on every read, so a fiction that later goes private stops rendering a
--     card without any backfill, and without leaking its title.
--
--   * The partial index serves two things: the "fictions people are posting
--     about" surface, and the ON DELETE SET NULL scan itself - an unindexed
--     FK would make deleting a fiction scan every post on the platform.

-- +migrate Up

ALTER TABLE community_posts
    ADD COLUMN novel_id   UUID REFERENCES novels   (id) ON DELETE SET NULL,
    ADD COLUMN chapter_id UUID REFERENCES chapters (id) ON DELETE SET NULL,
    ADD CONSTRAINT community_posts_reference_check
        CHECK (chapter_id IS NULL OR novel_id IS NOT NULL);

COMMENT ON COLUMN community_posts.novel_id IS
    'Optional fiction this post is about. Visibility is resolved at read time against the reader, never copied here.';

CREATE INDEX community_posts_novel_idx
    ON community_posts (novel_id) WHERE novel_id IS NOT NULL;

CREATE INDEX community_posts_chapter_idx
    ON community_posts (chapter_id) WHERE chapter_id IS NOT NULL;

-- +migrate Down

DROP INDEX IF EXISTS community_posts_chapter_idx;
DROP INDEX IF EXISTS community_posts_novel_idx;

ALTER TABLE community_posts
    DROP CONSTRAINT IF EXISTS community_posts_reference_check;

ALTER TABLE community_posts DROP COLUMN IF EXISTS chapter_id;
ALTER TABLE community_posts DROP COLUMN IF EXISTS novel_id;

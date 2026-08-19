-- Community feed tools: post types, saved posts, hashtags, and post search
-- (docs/COMMUNITY-FEED.md).
--
-- Four independent pieces, one phase:
--
--   * post_type is the AUTHOR's declared intent for a post (พูดคุย, ประกาศ
--     ตอนใหม่, ขอความช่วยเหลือเรื่องพล็อต, หาเบต้า, รับคำขอเขียน, อีเวนต์เขียน).
--     VARCHAR without a CHECK, exactly like reaction_type and
--     notifications.type: the service enforces the current allowlist, and a
--     new type must not need a migration. The DEFAULT backfills every
--     existing post as ordinary discussion, which is what they all were.
--
--   * community_post_bookmarks is "save this post for later". The composite
--     primary key is the duplicate guard (same pattern as
--     community_reactions); writers use ON CONFLICT DO NOTHING and an
--     idempotent DELETE. Both FKs CASCADE: a bookmark is a pointer with no
--     content of its own, so it dies with either end.
--
--   * community_post_hashtags stores the #tags extracted from a post's
--     content AT WRITE TIME, by the service. Extraction on write rather than
--     regexp on read keeps the trending-tags surface an indexed count instead
--     of a full-content scan, and editing a post replaces its rows, so the
--     table can never disagree with the text. The tags are derived data:
--     dropping and re-extracting them loses nothing the content doesn't hold.
--
--   * The trigram index makes post search (ILIKE, docs/COMMUNITY-FEED.md -
--     trigram, not FTS, because FTS cannot segment Thai) an index scan. The
--     search predicate only ever matches published public posts, but the
--     index is deliberately unfiltered: the planner combines it with the
--     visibility predicate, and a partial index would silently stop serving
--     the day the predicate wording drifts.
--
-- Nothing here copies or transforms writer content; every column is either
-- author intent, a pointer, or derived from content that stays untouched.

-- +migrate Up
ALTER TABLE community_posts
    ADD COLUMN post_type VARCHAR(32) NOT NULL DEFAULT 'discussion';

COMMENT ON COLUMN community_posts.post_type IS
    'The author''s declared intent (discussion, announcement, plot_help, '
    'beta_request, fic_request, event). Allowlist lives in the service, '
    'like reaction_type (docs/COMMUNITY-FEED.md).';

-- Serves the type-filtered feeds (หาเบต้า, อีเวนต์เขียน). Partial: deleted
-- posts never list, and the default type would dominate a full index.
CREATE INDEX community_posts_type_idx
    ON community_posts (post_type, created_at DESC)
    WHERE deleted_at IS NULL;

-- Post search rides trigrams, the same engine as novel title search.
CREATE INDEX community_posts_content_trgm_idx
    ON community_posts USING GIN (content gin_trgm_ops);

CREATE TABLE community_post_bookmarks (
    user_id    UUID        NOT NULL REFERENCES users (id)           ON DELETE CASCADE,
    post_id    UUID        NOT NULL REFERENCES community_posts (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One bookmark per (user, post); the PK prefix also serves "my saved
    -- posts", newest resolved by created_at at query time.
    PRIMARY KEY (user_id, post_id)
);

COMMENT ON TABLE community_post_bookmarks IS
    'Saved community posts (docs/COMMUNITY-FEED.md). A pointer, not content.';

CREATE TABLE community_post_hashtags (
    post_id UUID        NOT NULL REFERENCES community_posts (id) ON DELETE CASCADE,
    tag     VARCHAR(64) NOT NULL,

    PRIMARY KEY (post_id, tag)
);

COMMENT ON TABLE community_post_hashtags IS
    'Hashtags extracted from post content at write time by the service; '
    'derived data, replaced whenever the content changes '
    '(docs/COMMUNITY-FEED.md).';

-- "แท็กที่กำลังพูดถึง" - count posts per tag inside a recent window.
CREATE INDEX community_post_hashtags_tag_idx
    ON community_post_hashtags (tag);

-- +migrate Down
DROP TABLE community_post_hashtags;
DROP TABLE community_post_bookmarks;
DROP INDEX community_posts_content_trgm_idx;
DROP INDEX community_posts_type_idx;
ALTER TABLE community_posts DROP COLUMN post_type;

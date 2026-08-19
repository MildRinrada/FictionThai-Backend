-- Phase 7 - Community
-- (docs/08 - Database Design.md §21 and §44 Phase 7; docs/11 §37).
--
-- Creates community_posts, community_comments, and community_reactions.
-- Nothing speculative: no media, tags, polls, or follower tables - docs/03
-- lists those as POSSIBLE community content, but §21 defines text posts, and
-- media is its own later phase (§44 Phase 9).
--
-- Design notes:
--
--   * Posts carry BOTH visibility and status because they answer different
--     questions (docs/08 §21.1): visibility is the AUTHOR's audience choice
--     (docs/11 §37 - the backend must enforce it), status is the PLATFORM's
--     moderation state. deleted_at is the author taking the post back.
--
--   * community_comments.status values are not enumerated in §21.2; they
--     follow §20.1's comment vocabulary (visible / hidden / removed) - the
--     same semantic family, the same reader predicate.
--
--   * community_reactions has no id column in §21.2 and a recommended
--     UNIQUE(post_id, user_id) - modelled as the composite PRIMARY KEY,
--     exactly like bookmarks: the key IS the duplicate-reaction guard
--     (docs/09 §34 "Duplicate reactions"), and one row per (post, user) means
--     changing your reaction replaces it rather than accumulating.
--     reaction_type stays VARCHAR without an enumerated CHECK for the same
--     reason notifications.type does: docs/01 §20.2's list is explicitly
--     examples, and a new type must not need a migration. The service
--     enforces the current allowlist.
--
--   * Everything hard-CASCADEs from users/posts like the shelf tables: users
--     and posts soft-delete in practice, so the cascade only fires on the
--     deliberate hard-delete of docs/08 §38.

-- +migrate Up

-- ---------------------------------------------------------------------------
-- community_posts - docs/08 §21.1
-- ---------------------------------------------------------------------------
CREATE TABLE community_posts (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id  UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Plain text, never markup (docs/11 §16).
    content    TEXT        NOT NULL,

    visibility VARCHAR(16) NOT NULL DEFAULT 'public',
    status     VARCHAR(16) NOT NULL DEFAULT 'published',

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,

    CONSTRAINT community_posts_visibility_check
        CHECK (visibility IN ('public', 'followers', 'private')),
    CONSTRAINT community_posts_status_check
        CHECK (status IN ('published', 'hidden', 'removed')),
    CONSTRAINT community_posts_content_not_blank CHECK (btrim(content) <> '')
);

-- The docs/08 §37 pair: an author's posts, and the newest-first feed.
CREATE INDEX community_posts_author_idx  ON community_posts (author_id);
CREATE INDEX community_posts_created_idx ON community_posts (created_at DESC);

-- ---------------------------------------------------------------------------
-- community_comments - docs/08 §21.2
-- ---------------------------------------------------------------------------
CREATE TABLE community_comments (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    post_id    UUID        NOT NULL REFERENCES community_posts (id) ON DELETE CASCADE,
    author_id  UUID        NOT NULL REFERENCES users (id)           ON DELETE CASCADE,
    parent_id  UUID        REFERENCES community_comments (id)       ON DELETE CASCADE,

    content    TEXT        NOT NULL,
    status     VARCHAR(16) NOT NULL DEFAULT 'visible',

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,

    CONSTRAINT community_comments_status_check
        CHECK (status IN ('visible', 'hidden', 'removed')),
    CONSTRAINT community_comments_content_not_blank CHECK (btrim(content) <> '')
);

-- §37 lists no community_comments indexes, but "this post's thread" and
-- "this comment's replies" are the only two queries the table serves - the
-- same shape comments(chapter_id)/comments(parent_id) index for fiction.
-- An unindexed FK here would make every post page a sequential scan.
CREATE INDEX community_comments_post_idx   ON community_comments (post_id);
CREATE INDEX community_comments_parent_idx ON community_comments (parent_id);

-- ---------------------------------------------------------------------------
-- community_reactions - docs/08 §21.3
-- ---------------------------------------------------------------------------
CREATE TABLE community_reactions (
    post_id       UUID        NOT NULL REFERENCES community_posts (id) ON DELETE CASCADE,
    user_id       UUID        NOT NULL REFERENCES users (id)           ON DELETE CASCADE,
    reaction_type VARCHAR(32) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- §21.3's recommended UNIQUE(post_id, user_id), as the PK (docs/09 §34).
    PRIMARY KEY (post_id, user_id)
);

-- Reaction counts per post ride the PK prefix; no further index is needed
-- until an "everything I reacted to" surface exists (docs/08 §34).

-- +migrate Down

DROP TABLE IF EXISTS community_reactions;
DROP TABLE IF EXISTS community_comments;
DROP TABLE IF EXISTS community_posts;

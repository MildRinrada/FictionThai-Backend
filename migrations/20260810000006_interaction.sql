-- Phase 6 - Interaction
-- (docs/08 - Database Design.md §20.1, §23.1, §37 and §44 Phase 6).
--
-- Creates comments and notifications. community_* tables are §44 Phase 7 and
-- are deliberately absent.
--
-- Design notes:
--
--   * comments carry BOTH deleted_at and status because they answer different
--     questions (docs/08 §20.1): deleted_at is the author of the comment taking
--     it back (soft delete, like novels and chapters), while status is the
--     moderation state (visible / hidden / removed). A reader-facing list
--     requires deleted_at IS NULL AND status = 'visible'.
--
--   * chapter_id is NULL for a comment on the fiction itself and set for a
--     comment on one chapter (docs/08 §20.1 "Comments can belong to a novel or
--     chapter"). novel_id is ALWAYS set, including for chapter comments, so
--     "everything said about this fiction" never needs a join through chapters.
--
--   * parent_id enables threaded replies (docs/08 §20.1). The service layer
--     constrains threading depth and same-target replies; the schema stays as
--     documented so a deeper thread policy later is a code change, not a
--     migration.
--
--   * comments hard-CASCADE from novels/chapters/users like the shelf tables:
--     those parents all soft-delete in practice, so the cascade only fires on
--     the deliberate, explicit hard-delete of docs/08 §38 - at which point
--     keeping comments pointing at nothing has no value.
--
--   * notifications are per-user DELIVERY state, not authored content: no
--     deleted_at, no updated_at (docs/08 §23.1). actor_id is SET NULL on user
--     hard-delete so the recipient's notification history survives the actor.
--
--   * Indexes are exactly the docs/08 §37 set. The two notification indexes
--     serve the only two queries the API makes: the unread badge count
--     (recipient_id, read_at) and the newest-first list
--     (recipient_id, created_at).

-- +migrate Up

-- ---------------------------------------------------------------------------
-- comments - docs/08 §20.1
-- ---------------------------------------------------------------------------
CREATE TABLE comments (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id)    ON DELETE CASCADE,
    novel_id   UUID        NOT NULL REFERENCES novels (id)   ON DELETE CASCADE,
    chapter_id UUID        REFERENCES chapters (id)          ON DELETE CASCADE,
    parent_id  UUID        REFERENCES comments (id)          ON DELETE CASCADE,

    -- Plain text, never markup (docs/11 §16 - sanitised at render, stored raw).
    content    TEXT        NOT NULL,

    status     VARCHAR(16) NOT NULL DEFAULT 'visible',

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,

    CONSTRAINT comments_status_check
        CHECK (status IN ('visible', 'hidden', 'removed')),

    -- A blank comment is never meaningful; the service validates first, this
    -- is the backstop.
    CONSTRAINT comments_content_not_blank CHECK (btrim(content) <> '')
);

-- The docs/08 §37 set: fiction page listing, chapter listing, reply loading.
CREATE INDEX comments_novel_idx   ON comments (novel_id);
CREATE INDEX comments_chapter_idx ON comments (chapter_id);
CREATE INDEX comments_parent_idx  ON comments (parent_id);

-- ---------------------------------------------------------------------------
-- notifications - docs/08 §23.1
-- ---------------------------------------------------------------------------
CREATE TABLE notifications (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    actor_id     UUID        REFERENCES users (id)          ON DELETE SET NULL,

    -- new_follower / new_comment / comment_reply / novel_update / system …
    -- (docs/08 §23.1). VARCHAR, not an enum: the documented list is explicitly
    -- "Examples", and a new type must not need a migration.
    type         VARCHAR(32) NOT NULL,

    entity_type  VARCHAR(32),
    entity_id    UUID,

    read_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- docs/08 §37: notifications(recipient_id, read_at) - the unread badge -
-- and notifications(recipient_id, created_at) - the newest-first list.
CREATE INDEX notifications_recipient_read_idx
    ON notifications (recipient_id, read_at);
CREATE INDEX notifications_recipient_recency_idx
    ON notifications (recipient_id, created_at DESC);

-- +migrate Down

DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS comments;

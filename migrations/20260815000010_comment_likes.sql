-- ถูกใจความคิดเห็น (comment design review 2026-08): the heart under each
-- comment. One row per (comment, account) - pressing twice is one heart, and
-- unliking deletes the row. Members only: a guest heart would be an anonymous
-- counter nobody could ever take back, the same reason a guest comment cannot
-- be edited (§13D).

-- +migrate Up

CREATE TABLE comment_likes (
    comment_id UUID NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (comment_id, user_id)
);

-- The listing enrichment reads by comment; the PK already serves (comment,
-- user) lookups.
CREATE INDEX comment_likes_comment_idx ON comment_likes (comment_id);

-- +migrate Down

DROP TABLE IF EXISTS comment_likes;

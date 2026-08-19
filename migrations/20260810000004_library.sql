-- Phase 3 - Reader & Library
-- (docs/08 - Database Design.md §16, §17, §18 and §44 Phase 5).
--
-- Creates bookmarks, user_follows, and reading_progress.
--
-- Design notes:
--
--   * All three are pure per-user state, not authored content. That is why they
--     hard-delete via CASCADE while novels and chapters soft-delete: there is
--     nothing to recover and no audit value (docs/08 §37 "do not soft-delete
--     everything automatically"). Deleting a novel row outright (which itself
--     requires the explicit business decision of docs/08 §38) takes the shelf
--     entries pointing at it along - a bookmark of nothing is meaningless.
--
--   * Duplicate prevention is the PRIMARY KEY, not application checks: a
--     pre-flight SELECT would still race (docs/08 §34, docs/09 §33). Writers
--     use ON CONFLICT DO NOTHING so repeats are idempotent.
--
--   * reading_progress keeps ONE row per (user, novel) - the latest position
--     only (docs/08 §18). This is what makes "Continue Reading" one indexed
--     read. reading_history (docs/08 §19) is explicitly optional and is NOT
--     part of §44 Phase 5, so it is deliberately absent.
--
--   * progress_percent is how far through the CHAPTER the reader is. The
--     chapter itself identifies where in the novel they are, so the pair
--     resumes both one-shot and multi-chapter fiction (docs/08 §18).
--
--   * No format columns and no format-dependent constraints: a bookmark or a
--     progress row survives every format change untouched (docs/08 §3
--     "Changing a format must not delete bookmarks / reading progress").

-- +migrate Up

-- ---------------------------------------------------------------------------
-- bookmarks - docs/08 §16
-- ---------------------------------------------------------------------------
CREATE TABLE bookmarks (
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    novel_id   UUID        NOT NULL REFERENCES novels (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The duplicate-bookmark guard (docs/08 §16.1, §34).
    PRIMARY KEY (user_id, novel_id)
);

-- The PK already serves the "my library" lookup (user_id is its prefix), so a
-- separate bookmarks(user_id) index would be pure write cost (docs/08 §34
-- "do not blindly index every column"). The novel side needs its own index for
-- author-facing counts and cleanup.
CREATE INDEX bookmarks_novel_idx ON bookmarks (novel_id);

-- ---------------------------------------------------------------------------
-- user_follows - docs/08 §17
-- ---------------------------------------------------------------------------
CREATE TABLE user_follows (
    follower_id  UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    following_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (follower_id, following_id),

    -- docs/08 §17.1: users cannot follow themselves.
    CONSTRAINT user_follows_not_self CHECK (follower_id <> following_id)
);

-- "Who follows this author" - follower counts and future notification fan-out
-- (docs/02 §"Find followers"). The follower side is covered by the PK prefix.
CREATE INDEX user_follows_following_idx ON user_follows (following_id);

-- ---------------------------------------------------------------------------
-- reading_progress - docs/08 §18
-- ---------------------------------------------------------------------------
CREATE TABLE reading_progress (
    user_id          UUID          NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    novel_id         UUID          NOT NULL REFERENCES novels (id) ON DELETE CASCADE,
    chapter_id       UUID          NOT NULL REFERENCES chapters (id) ON DELETE CASCADE,

    -- 0–100, how far through the chapter. NUMERIC(5,2) per docs/08 §18.1;
    -- the CHECK keeps a buggy client from storing a nonsense position.
    progress_percent NUMERIC(5, 2) NOT NULL DEFAULT 0,
    last_read_at     TIMESTAMPTZ   NOT NULL DEFAULT now(),

    -- One row per (user, novel): the latest position only (docs/08 §18.1).
    -- Every save is an UPSERT against this key.
    PRIMARY KEY (user_id, novel_id),

    CONSTRAINT reading_progress_percent_range
        CHECK (progress_percent >= 0 AND progress_percent <= 100)
);

-- "Continue Reading" is ORDER BY last_read_at DESC for one user (docs/08 §18.1)
-- - this makes it a pure index walk, which matters because it renders on the
-- library page and potentially the home page for every signed-in visitor.
CREATE INDEX reading_progress_user_recency_idx
    ON reading_progress (user_id, last_read_at DESC);

-- Author-side queries and cleanup when a novel goes away.
CREATE INDEX reading_progress_novel_idx ON reading_progress (novel_id);

-- +migrate Down

DROP TABLE IF EXISTS reading_progress;
DROP TABLE IF EXISTS user_follows;
DROP TABLE IF EXISTS bookmarks;

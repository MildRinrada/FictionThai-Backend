-- Phase 4 - Discovery: genres and tags
-- (docs/08 - Database Design.md §14, §15 and §44 Phase 4).
--
-- Creates genres, tags, novel_genres, and novel_tags - nothing else. The
-- statistics tables (§28) belong to a later phase; `sort=popular` is computed
-- from the bookmarks table, which §28.1 names as the source of truth.
--
-- Design notes:
--
--   * Genres and tags are SEPARATE vocabularies (§14 "controlled
--     classification" vs §15 "flexible discovery metadata") - no shared
--     polymorphic table. Genres arrive seeded and curated; tags are created by
--     writers as they tag their work.
--
--   * The assignment tables hard-delete via CASCADE: they are metadata edges,
--     not authored content (§37). A soft-deleted novel keeps its assignments -
--     they become unreachable with it and return with it.
--
--   * Deleting a genre or tag row RESTRICTs while assignments exist: silently
--     un-tagging fictions platform-wide is exactly the kind of destructive
--     surprise §43 Rule 1 exists to prevent. (No delete endpoint exists yet;
--     the constraint is the backstop.)
--
--   * Uniqueness is plain, not partial - these rows are never soft-deleted.

-- +migrate Up

-- ---------------------------------------------------------------------------
-- genres - docs/08 §14.1
-- ---------------------------------------------------------------------------
CREATE TABLE genres (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(64)  NOT NULL UNIQUE,
    slug        VARCHAR(64)  NOT NULL UNIQUE,
    description TEXT         NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- The controlled vocabulary of §14.1, seeded so discovery works from the
-- first deployment. Curation (adding, renaming) is an operational act until an
-- admin surface exists; writers can only SELECT from this list.
INSERT INTO genres (name, slug, description) VALUES
    ('Fantasy',  'fantasy',  'แฟนตาซี โลกเวทมนตร์ และสิ่งเหนือธรรมชาติ'),
    ('Romance',  'romance',  'โรแมนซ์และความสัมพันธ์'),
    ('Horror',   'horror',   'สยองขวัญและเรื่องเขย่าขวัญ'),
    ('Mystery',  'mystery',  'สืบสวน สอบสวน และปริศนา'),
    ('Sci-Fi',   'sci-fi',   'ไซไฟและโลกอนาคต'),
    ('Comedy',   'comedy',   'ตลกและเรื่องเบาสมอง'),
    ('Drama',    'drama',    'ดราม่าและชีวิต');

-- ---------------------------------------------------------------------------
-- tags - docs/08 §15.1
-- ---------------------------------------------------------------------------
CREATE TABLE tags (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name       VARCHAR(64)  NOT NULL UNIQUE,
    slug       VARCHAR(80)  NOT NULL UNIQUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- novel_genres - docs/08 §14.2
-- ---------------------------------------------------------------------------
CREATE TABLE novel_genres (
    novel_id UUID NOT NULL REFERENCES novels (id) ON DELETE CASCADE,
    genre_id UUID NOT NULL REFERENCES genres (id) ON DELETE RESTRICT,

    PRIMARY KEY (novel_id, genre_id)
);

-- "Fictions in this genre" - the ?genre= filter walks this (docs/09 §11).
-- The novel side is covered by the PK prefix.
CREATE INDEX novel_genres_genre_idx ON novel_genres (genre_id);

-- ---------------------------------------------------------------------------
-- novel_tags - docs/08 §15.2
-- ---------------------------------------------------------------------------
CREATE TABLE novel_tags (
    novel_id UUID NOT NULL REFERENCES novels (id) ON DELETE CASCADE,
    tag_id   UUID NOT NULL REFERENCES tags (id) ON DELETE RESTRICT,

    PRIMARY KEY (novel_id, tag_id)
);

-- "Fictions with this tag" - the ?tag= filter, and tag usage counts.
CREATE INDEX novel_tags_tag_idx ON novel_tags (tag_id);

-- +migrate Down

DROP TABLE IF EXISTS novel_tags;
DROP TABLE IF EXISTS novel_genres;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS genres;

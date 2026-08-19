-- Phase 9 - Media
-- (docs/08 - Database Design.md §22 and §44 Phase 9; docs/07 §22–§23;
-- docs/11 §28–§29).
--
-- Creates the central media metadata table. The BINARY file never enters
-- PostgreSQL (docs/07 §22): it lives in object storage behind the storage
-- adapter, and this row is the metadata + reference. Nothing speculative: no
-- per-owner media tables, no junction tables - owning entities reference
-- media through their existing URL columns (novels.cover_url,
-- user_profiles.avatar_url), exactly the "metadata / URL / object key" split
-- docs/07 §22 draws.
--
-- Design notes:
--
--   * media_type stays VARCHAR without a CHECK for the same reason
--     notifications.type does: §22.1's list (avatar / novel_cover /
--     community_image / attachment) is the vocabulary, and community_image
--     and attachment have no owning surface yet - the service allowlists
--     what may actually be UPLOADED today, and widening it later must not
--     need a migration.
--
--   * object_key is UNIQUE per §22.1 and is always GENERATED
--     ({media_type}/{uuid}.{ext}) - user input never reaches a storage path
--     (docs/11 §28 "Generated storage names").
--
--   * size_bytes is NOT NULL because metadata is recorded AFTER the bytes
--     are validated and stored (docs/07 §23's flow) - there is deliberately
--     no "pending" state to leak half-uploaded rows.
--
--   * deleted_at is the platform/owner removal axis. The public serve path
--     checks THIS ROW, not the disk, so a soft-deleted object is unreachable
--     even if the storage delete behind it failed.
--
--   * owner_id CASCADEs like every user-owned row (docs/08 §38: only the
--     deliberate hard delete fires it).

-- +migrate Up

CREATE TABLE media (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id          UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    object_key        TEXT        NOT NULL UNIQUE,
    original_filename TEXT,
    mime_type         VARCHAR(64) NOT NULL,
    size_bytes        BIGINT      NOT NULL,
    media_type        VARCHAR(32) NOT NULL,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ,

    CONSTRAINT media_size_positive CHECK (size_bytes > 0),
    CONSTRAINT media_object_key_not_blank CHECK (btrim(object_key) <> ''),
    CONSTRAINT media_media_type_not_blank CHECK (btrim(media_type) <> '')
);

-- The uploader's rows - backs ownership checks on delete and the users
-- hard-delete cascade; every other user-owned table indexes its owner the
-- same way.
CREATE INDEX media_owner_idx ON media (owner_id);

-- +migrate Down

DROP TABLE IF EXISTS media;

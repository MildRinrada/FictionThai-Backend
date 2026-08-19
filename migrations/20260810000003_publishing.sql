-- Phase 2 - Publishing core + Fiction Format System
-- (docs/08 - Database Design.md §44 Phase 2 and Phase 3).
--
-- Creates novels, chapters, chapter_revisions, and chapter_messages.
--
-- Design notes:
--
--   * The three format dimensions are SEPARATE columns, never one `type` enum
--     (docs/08 §7.3, §43 Rule 6). All 2x2x2 combinations are valid; the CHECK
--     constraints validate each dimension independently and deliberately do not
--     forbid any combination (docs/08 §2.4).
--
--   * A chapter may hold BOTH representations at once - prose in
--     chapters.content and structured chat in chapter_messages (docs/08 §10.3).
--     novels.presentation_format selects which one readers see; it does not
--     select which one exists. That is what makes a format change a metadata
--     UPDATE that touches no content at all (docs/08 §3.1, §43 Rule 7).
--     See docs/CONTENT-MODEL.md.
--
--   * Uniqueness is PARTIAL - `WHERE deleted_at IS NULL`. Novels and chapters
--     are soft-deleted (docs/08 §37), and a plain UNIQUE would let a deleted
--     chapter 3 permanently block the creation of a new chapter 3.
--
--   * Ownership is Novel → Chapter → ChapterMessage (docs/08 §1.3, §10.2).
--     Only novels carry author_id; a chapter's owner is reached through its
--     novel, so ownership cannot drift between the two.

-- +migrate Up

-- ---------------------------------------------------------------------------
-- novels - docs/08 §7.1
--
-- The table name stays `novels`; the product term is "Fiction" (docs/08 §7.1,
-- docs/09 §15). Renaming it is a deliberate future API version, not a casual
-- change.
-- ---------------------------------------------------------------------------
CREATE TABLE novels (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- docs/07 §14: every user-generated resource has an explicit owner.
    -- RESTRICT, not CASCADE: docs/08 §38 requires deleting a user to be an
    -- explicit business decision, never a silent cascade through their work.
    author_id           UUID         NOT NULL REFERENCES users (id) ON DELETE RESTRICT,

    title               VARCHAR(200) NOT NULL,
    slug                VARCHAR(240) NOT NULL,
    description         TEXT         NULL,
    cover_url           TEXT         NULL,

    -- --- Fiction Format System (docs/08 §2) -------------------------------
    story_structure     VARCHAR(32)  NOT NULL DEFAULT 'multi_chapter',
    presentation_format VARCHAR(32)  NOT NULL DEFAULT 'standard',
    content_mode        VARCHAR(32)  NOT NULL DEFAULT 'general',

    status              VARCHAR(32)  NOT NULL DEFAULT 'draft',
    visibility          VARCHAR(32)  NOT NULL DEFAULT 'private',
    content_warning     TEXT         NULL,

    published_at        TIMESTAMPTZ  NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ  NULL,

    -- Each dimension is validated on its own. There is deliberately no
    -- cross-dimension CHECK: docs/08 §2.4 forbids restricting combinations for
    -- implementation convenience.
    CONSTRAINT novels_story_structure_valid
        CHECK (story_structure IN ('one_shot', 'multi_chapter')),
    CONSTRAINT novels_presentation_format_valid
        CHECK (presentation_format IN ('standard', 'chat')),
    CONSTRAINT novels_content_mode_valid
        CHECK (content_mode IN ('general', 'headcanon')),

    -- docs/08 §7.1 publication status and visibility. They are independent:
    -- visibility must remain independent from format AND from status
    -- (docs/11 §32), so "chat + private" stays private.
    CONSTRAINT novels_status_valid
        CHECK (status IN ('draft', 'ongoing', 'completed', 'hiatus', 'cancelled')),
    CONSTRAINT novels_visibility_valid
        CHECK (visibility IN ('private', 'unlisted', 'public')),

    CONSTRAINT novels_title_not_blank CHECK (btrim(title) <> ''),
    CONSTRAINT novels_slug_not_blank  CHECK (btrim(slug) <> ''),

    -- A draft is never publicly readable (docs/08 §7.1, docs/11 §31). The
    -- application enforces this too, but the constraint makes the invalid state
    -- unrepresentable rather than merely unreachable (docs/08 §43 Rule 1).
    CONSTRAINT novels_draft_is_not_public
        CHECK (status <> 'draft' OR visibility = 'private')
);

-- docs/08 §35: slugs are the public URL identity, so they are unique platform
-- wide. Partial, so a soft-deleted fiction does not hold its slug hostage.
CREATE UNIQUE INDEX novels_slug_key ON novels (slug) WHERE deleted_at IS NULL;

-- docs/08 §34 index strategy, driven by the actual query patterns:
--   author's own shelf / writer studio
CREATE INDEX novels_author_idx ON novels (author_id) WHERE deleted_at IS NULL;

--   the public listing: filter on visibility + status, order by published_at.
--   docs/08 §34 lists novels(status, visibility) and novels(published_at);
--   one composite index serves both because the listing always applies both.
CREATE INDEX novels_public_listing_idx
    ON novels (visibility, status, published_at DESC)
    WHERE deleted_at IS NULL;

--   format filters are first-class query filters (docs/08 §33, docs/09 §11).
--   Composite indexes lead with visibility because a public listing always
--   filters on it, matching the composite patterns in docs/08 §34.
CREATE INDEX novels_format_presentation_idx
    ON novels (visibility, presentation_format, published_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX novels_format_structure_idx
    ON novels (visibility, story_structure, published_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX novels_format_mode_idx
    ON novels (visibility, content_mode, published_at DESC)
    WHERE deleted_at IS NULL;

--   title search. docs/08 §33 starts with PostgreSQL rather than a search
--   engine; pg_trgm is installed by the baseline migration and makes a
--   substring match indexable, which a plain B-tree cannot do.
CREATE INDEX novels_title_trgm_idx ON novels USING GIN (title gin_trgm_ops);

COMMENT ON COLUMN novels.story_structure IS
    'How the work is organised. Independent of presentation_format and content_mode (docs/08 §2.1).';
COMMENT ON COLUMN novels.presentation_format IS
    'Which stored representation readers see: standard -> chapters.content, chat -> chapter_messages (docs/08 §11).';
COMMENT ON COLUMN novels.content_mode IS
    'Author content classification. Grants no permissions and changes no access rules (docs/11 §19).';

-- ---------------------------------------------------------------------------
-- chapters - docs/08 §8.1
--
-- `content` is NULLABLE: a chat chapter keeps its content in chapter_messages
-- (docs/08 §8.1). Both may be populated at once (docs/08 §10.3).
-- ---------------------------------------------------------------------------
CREATE TABLE chapters (
    id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- CASCADE is correct here and only here: a chapter has no meaning without
    -- its fiction, and novels are soft-deleted, so this fires only on a genuine
    -- hard delete.
    novel_id       UUID         NOT NULL REFERENCES novels (id) ON DELETE CASCADE,

    chapter_number INTEGER      NOT NULL,
    title          VARCHAR(200) NULL,
    slug           VARCHAR(240) NOT NULL,
    content        TEXT         NULL,

    status         VARCHAR(32)  NOT NULL DEFAULT 'draft',
    published_at   TIMESTAMPTZ  NULL,
    scheduled_at   TIMESTAMPTZ  NULL,
    word_count     INTEGER      NOT NULL DEFAULT 0,

    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ  NULL,

    CONSTRAINT chapters_status_valid
        CHECK (status IN ('draft', 'scheduled', 'published', 'unpublished')),
    CONSTRAINT chapters_number_positive CHECK (chapter_number > 0),
    CONSTRAINT chapters_word_count_not_negative CHECK (word_count >= 0),
    CONSTRAINT chapters_slug_not_blank CHECK (btrim(slug) <> ''),

    -- A scheduled chapter must say when. Without this, 'scheduled' with a NULL
    -- time would be indistinguishable from "published immediately" to any
    -- visibility query.
    CONSTRAINT chapters_scheduled_has_time
        CHECK (status <> 'scheduled' OR scheduled_at IS NOT NULL)
);

-- docs/08 §8.1 recommends UNIQUE(novel_id, chapter_number) and
-- UNIQUE(novel_id, slug). Partial, so soft-deleted chapters release both.
CREATE UNIQUE INDEX chapters_novel_number_key
    ON chapters (novel_id, chapter_number) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX chapters_novel_slug_key
    ON chapters (novel_id, slug) WHERE deleted_at IS NULL;

-- docs/08 §34: chapters(novel_id, chapter_number) is the reader's ordering
-- query and is already covered by the unique index above.
-- chapters(novel_id, status) serves "published chapters of this fiction".
CREATE INDEX chapters_novel_status_idx
    ON chapters (novel_id, status) WHERE deleted_at IS NULL;

COMMENT ON COLUMN chapters.content IS
    'Standard prose, stored as PLAIN TEXT. Never HTML; never rendered as markup (docs/11 §16, §17).';
COMMENT ON COLUMN chapters.word_count IS
    'Approximate. Whitespace-delimited tokens; accurate Thai segmentation needs the NLP service (docs/12).';

-- ---------------------------------------------------------------------------
-- chapter_revisions - docs/08 §12.1, resolved by docs/CONTENT-MODEL.md §5
--
-- One row is a COMPLETE, immutable snapshot of both representations: the prose
-- in `content` and the ordered chat array in `messages`.
--
-- docs/08 §12 left the chat-versioning shape open ("inside the revision model
-- or through a dedicated immutable message snapshot"). This takes the former:
-- a revision is written once and read whole, never queried by speaker or
-- position, so normalising it would buy no query capability while making
-- "revision exists, messages missing" a representable state.
-- ---------------------------------------------------------------------------
CREATE TABLE chapter_revisions (
    id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    chapter_id UUID         NOT NULL REFERENCES chapters (id) ON DELETE CASCADE,
    version    INTEGER      NOT NULL,

    title      VARCHAR(200) NULL,
    content    TEXT         NULL,

    -- The whole ordered message array as it stood. NULL means the chapter had
    -- no chat representation at that version; '[]' means it had an empty one.
    messages   JSONB        NULL,

    word_count INTEGER      NOT NULL DEFAULT 0,

    -- SET NULL, not CASCADE: deleting an account must not erase the authorship
    -- history of revisions that still exist (docs/08 §38).
    created_by UUID         NULL REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT chapter_revisions_version_positive CHECK (version > 0),
    -- docs/08 §12.1. Not partial: revisions are never soft-deleted, and this is
    -- also what makes concurrent MAX(version)+1 allocation safe.
    CONSTRAINT chapter_revisions_chapter_version_key UNIQUE (chapter_id, version),
    CONSTRAINT chapter_revisions_messages_is_array
        CHECK (messages IS NULL OR jsonb_typeof(messages) = 'array')
);

CREATE INDEX chapter_revisions_chapter_idx
    ON chapter_revisions (chapter_id, version DESC);

COMMENT ON TABLE chapter_revisions IS
    'Immutable content snapshots. Never UPDATEd after insert (docs/08 §12.1).';

-- ---------------------------------------------------------------------------
-- chapter_messages - docs/08 §10.1
--
-- Structured chat content. Storing a conversation as an opaque string is
-- exactly what this table exists to prevent (docs/08 §9).
-- ---------------------------------------------------------------------------
CREATE TABLE chapter_messages (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    chapter_id         UUID         NOT NULL REFERENCES chapters (id) ON DELETE CASCADE,

    -- 0-based and contiguous. Assigned by the server from array order, never
    -- accepted from a client, so gaps and duplicates are not reachable.
    position           INTEGER      NOT NULL,

    speaker_name       VARCHAR(64)  NOT NULL,
    speaker_avatar_url TEXT         NULL,
    message_type       VARCHAR(32)  NOT NULL DEFAULT 'message',
    content            TEXT         NOT NULL,

    -- ALLOWLISTED at the service layer, not a free-form bag. docs/11 §18: a
    -- writer must not be able to submit application-level values such as
    -- is_admin or verified through message metadata, because chat content
    -- visually resembles application UI.
    metadata           JSONB        NULL,

    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT chapter_messages_type_valid
        CHECK (message_type IN ('message', 'system', 'separator')),
    CONSTRAINT chapter_messages_position_not_negative CHECK (position >= 0),
    CONSTRAINT chapter_messages_chapter_position_key UNIQUE (chapter_id, position),
    CONSTRAINT chapter_messages_metadata_is_object
        CHECK (metadata IS NULL OR jsonb_typeof(metadata) = 'object'),

    -- A 'message' is someone saying something, so it needs both a speaker and
    -- text. 'system' and 'separator' are narration and dividers, so they may
    -- have neither.
    CONSTRAINT chapter_messages_speaker_required
        CHECK (message_type <> 'message' OR btrim(speaker_name) <> ''),
    CONSTRAINT chapter_messages_content_required
        CHECK (message_type <> 'message' OR btrim(content) <> '')
);

-- docs/08 §34: chapter_messages(chapter_id, position) is the reader's ordering
-- query; the unique constraint above already provides it.

COMMENT ON COLUMN chapter_messages.metadata IS
    'Allowlisted content properties only (currently: side). Never application state (docs/11 §18).';

-- +migrate Down

DROP TABLE IF EXISTS chapter_messages;
DROP TABLE IF EXISTS chapter_revisions;
DROP TABLE IF EXISTS chapters;
DROP TABLE IF EXISTS novels;

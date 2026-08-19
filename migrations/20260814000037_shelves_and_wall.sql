-- Public bookshelves and the profile comment wall
-- (README "Bookmarks & Personal Library"; the profile page's missing social
-- half).
--
-- The product decision these tables encode, and the reason they are NEW tables
-- rather than flags on existing ones:
--
-- README says bookmarks are private by default and that users MAY OPTIONALLY
-- create public collections. backend/internal/library owns the private half and
-- its package doc states the contract plainly - "Nothing here is ever publicly
-- listed - every read is scoped to the authenticated caller". Nothing in this
-- migration touches `bookmarks`. A public shelf is a SEPARATE thing a reader
-- assembles on purpose, one fiction at a time; it is never a switch that
-- publishes what they had already saved in private. Had this been an
-- `is_public` column on `bookmarks`, every private row on the platform would
-- have been one UPDATE away from being published, which is exactly the shape of
-- accident the separation exists to make impossible.
--
-- The wall is the other half: docs/06 §37 orders a profile as identity, work,
-- then social activity, and until now the third had nowhere to live. It is
-- opt-OUT rather than opt-in (wall_enabled defaults true) because a profile with
-- no way to leave a word is the norm on no reading platform, and the person can
-- close it in one switch - the same shape as the fiction-level comment access
-- the author already controls.

-- +migrate Up

-- ---------------------------------------------------------------------------
-- shelves - the opt-in public collection
-- ---------------------------------------------------------------------------

CREATE TABLE shelves (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       VARCHAR(60) NOT NULL,
    note       VARCHAR(160),
    is_public  BOOLEAN     NOT NULL DEFAULT FALSE,
    position   SMALLINT    NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A shelf with no name cannot be referred to by the person who made it;
    -- the service validates first, this is the backstop.
    CONSTRAINT shelves_name_not_blank CHECK (btrim(name) <> '')
);

-- The owner listing IS the query: every shelf read starts from one person and
-- renders in their chosen order.
CREATE INDEX shelves_owner_order_idx ON shelves (user_id, position);

COMMENT ON TABLE shelves IS
    'Reader-assembled collections. Separate from bookmarks on purpose: bookmarks are private forever, a shelf is opt-in publishing.';

COMMENT ON COLUMN shelves.is_public IS
    'FALSE by default and per shelf. A private shelf is invisible to everyone but its owner; making one public is an explicit act on that shelf alone, never a global switch and never retroactive to bookmarks.';

COMMENT ON COLUMN shelves.name IS
    'The readers own words for the collection - it is a label on a shelf, not a taxonomy term the platform curates.';

COMMENT ON COLUMN shelves.note IS
    'Optional one-line description shown under the shelf name. Short by design: it introduces a shelf, it is not a place to write a review.';

COMMENT ON COLUMN shelves.position IS
    'The owners chosen order. SMALLINT because a person orders a handful of shelves by hand; anything that needs more than that is a listing, not a shelf.';

-- ---------------------------------------------------------------------------
-- shelf_items - which fiction sits on which shelf
-- ---------------------------------------------------------------------------

CREATE TABLE shelf_items (
    shelf_id UUID         NOT NULL REFERENCES shelves (id) ON DELETE CASCADE,
    novel_id UUID         NOT NULL REFERENCES novels (id)  ON DELETE CASCADE,
    note     VARCHAR(160),
    added_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (shelf_id, novel_id)
);

COMMENT ON TABLE shelf_items IS
    'One fiction on one shelf. The composite primary key makes adding the same fiction twice idempotent rather than an error, matching every other shelf-shaped write in the API.';

COMMENT ON COLUMN shelf_items.note IS
    'The readers own line about why this one is here. Writer-authored free text belonging to the READER, shown beside the fiction card and never mixed into the fictions own metadata.';

-- ---------------------------------------------------------------------------
-- profile_comments - the wall
-- ---------------------------------------------------------------------------

CREATE TABLE profile_comments (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Whose wall this is, and who wrote on it. Two references to `users`
    -- because they are genuinely two people: the second is who may edit or
    -- withdraw the words, the first is who may clear them from their own page.
    profile_user_id UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    author_id       UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Plain text, never markup (docs/11 §16 - stored raw, escaped at render).
    body            TEXT        NOT NULL,

    status          VARCHAR(16) NOT NULL DEFAULT 'visible',

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,

    CONSTRAINT profile_comments_status_check
        CHECK (status IN ('visible', 'hidden', 'removed')),
    CONSTRAINT profile_comments_body_not_blank CHECK (btrim(body) <> '')
);

-- The wall listing IS the query: newest first, for one person.
CREATE INDEX profile_comments_wall_idx
    ON profile_comments (profile_user_id, created_at DESC);

COMMENT ON TABLE profile_comments IS
    'Messages left on a persons profile page. There are no guest entries: a wall carries no fiction to gate access on, so the only workable rule is that the writer has an account someone can hold responsible.';

COMMENT ON COLUMN profile_comments.status IS
    'The PLATFORMS axis (visible/hidden/removed), the same vocabulary comments.status uses. Independent of deleted_at, which is the authors own axis - a message its writer took back stays gone whatever moderation later decides.';

COMMENT ON COLUMN profile_comments.author_id IS
    'Who wrote it. NOT NULL because the wall accepts no guests; the profile owner may remove any entry, but only the author may claim one.';

-- ---------------------------------------------------------------------------
-- The two profile switches the features need
-- ---------------------------------------------------------------------------

ALTER TABLE user_profiles
    ADD COLUMN wall_enabled BOOLEAN NOT NULL DEFAULT TRUE;

COMMENT ON COLUMN user_profiles.wall_enabled IS
    'The walls on/off switch, owned by the person whose page it is. FALSE hides the wall ENTIRELY - the existing entries are kept, not deleted, so turning it back on restores the page rather than starting an empty one.';

-- boundaries is the writers own answer to "what will you and will you not
-- write" - the warning a reader wants before sending a request, and the writer
-- wants stated once instead of in every reply. Free text on purpose: the
-- platform has no business turning "ไม่รับเรื่องที่มีตัวละครเด็ก" into a
-- checkbox, and any list it maintained would be wrong for somebody.
ALTER TABLE author_profiles
    ADD COLUMN boundaries TEXT;

COMMENT ON COLUMN author_profiles.boundaries IS
    'คำเตือน/ขอบเขตของนักเขียน - what this writer will and will not write, in their own words. Writer-authored free text, displayed verbatim; it is NEVER parsed, normalised, or moderated into a taxonomy, and no filter or recommendation ever reads it.';

-- +migrate Down

ALTER TABLE author_profiles DROP COLUMN IF EXISTS boundaries;
ALTER TABLE user_profiles   DROP COLUMN IF EXISTS wall_enabled;
DROP TABLE IF EXISTS profile_comments;
DROP TABLE IF EXISTS shelf_items;
DROP TABLE IF EXISTS shelves;

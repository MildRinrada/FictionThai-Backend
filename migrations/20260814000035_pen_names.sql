-- Pen names: the identities a writer publishes under
-- (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
--
-- The rule that makes this worth a table: A PEN NAME IS CHANGEABLE, A HANDLE IS
-- NOT. `users.username` stays the permanent, addressable handle. Everything a
-- writer wants to change - the name on the cover, a separate identity for a
-- different วง, a shared name for a collaboration - lives here.
--
-- Impersonation is a real problem in fic communities, and the defence chosen
-- here is not to freeze names but to make a CHANGE VISIBLE FOR A WHILE:
-- pen_name_history keeps the previous name for 30 days so a profile can say
-- «เคยใช้ชื่อ …». It is deliberately not a permanent record - the point is to
-- catch a name being taken over, not to follow someone forever.
--
-- What this migration deliberately does NOT introduce:
--
--   * a fandom table. Per-fandom identity is `note` plus per-work selection; a
--     writer who separates วง picks the pen name when publishing the work, and
--     the platform never needs to know what a fandom is to support the practice.
--   * anything destructive on novels. novels.pen_name_id is ON DELETE SET NULL,
--     so removing an identity can never remove, alter, or hide a single word of
--     the work published under it (CLAUDE.md - Writer-First Principles).

-- +migrate Up

CREATE TABLE pen_names (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    name       VARCHAR(64) NOT NULL,
    -- The writer's own label for what this identity is FOR. Free text, not an
    -- enum: the handoff shows ค่าเริ่มต้น / แยกแนว / ร่วมเขียน as examples of
    -- how writers already describe their own identities, not as a vocabulary
    -- the platform gets to define.
    note       VARCHAR(40) NULL,

    -- Which identity a work falls back to when it names none of its own.
    is_default BOOLEAN     NOT NULL DEFAULT FALSE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE pen_names IS
    'The identities one account publishes under. A pen name is changeable; users.username is not.';

COMMENT ON COLUMN pen_names.name IS
    'The name readers see on the work. Unique per account case-insensitively, because two identities a writer cannot tell apart in their own list are not two identities.';

COMMENT ON COLUMN pen_names.note IS
    'The writer''s own label for what this identity is for (แยกแนว, ร่วมเขียน). Free text - the platform does not define the vocabulary.';

COMMENT ON COLUMN pen_names.is_default IS
    'The identity a work published under no explicit pen name falls back to. At most one per account, enforced by a partial unique index rather than by application code.';

-- Case-insensitive, because two identities the writer cannot tell apart in
-- their own list are not two identities. Scoped to the account: two different
-- people may of course choose the same pen name, and the platform is not in the
-- business of allocating names.
CREATE UNIQUE INDEX pen_names_user_name_key
    ON pen_names (user_id, lower(name));

-- At most one default per account. A partial unique index rather than a rule in
-- Go: the fallback that decides whose name appears on a work must not be able
-- to become ambiguous through a race between two requests.
CREATE UNIQUE INDEX pen_names_one_default_per_user_key
    ON pen_names (user_id) WHERE is_default;

-- Which identity a work is published under. NULL means "whatever my default is",
-- so a writer who renames their default renames the cover of every work that
-- never asked for anything else.
--
-- ON DELETE SET NULL is the whole contract of this column: removing an identity
-- must never remove or alter the work published under it. The fiction keeps its
-- text, its chapters, and its history, and simply falls back to the default -
-- exactly as a format change writes no content (CLAUDE.md - Format Changes).
ALTER TABLE novels
    ADD COLUMN pen_name_id UUID NULL REFERENCES pen_names (id) ON DELETE SET NULL;

COMMENT ON COLUMN novels.pen_name_id IS
    'The identity this work is published under; NULL falls back to the author''s default. ON DELETE SET NULL - deleting a pen name never deletes or alters a word of the work.';

-- The child-side index the SET NULL needs: without it, deleting one pen name
-- would sequentially scan every fiction on the platform.
CREATE INDEX idx_novels_pen_name ON novels (pen_name_id) WHERE pen_name_id IS NOT NULL;

-- The 30-day «เคยใช้ชื่อ …» record.
--
-- Keyed by user_id rather than by pen_name_id on purpose: the question a reader
-- is asking is "was this PERSON recently called something else", and the answer
-- has to survive the pen name row being deleted afterwards.
CREATE TABLE pen_name_history (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    user_id       UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    previous_name VARCHAR(64) NOT NULL,

    changed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE pen_name_history IS
    'Renames, shown as «เคยใช้ชื่อ …» for 30 days. A window, not an archive: it exists to make a takeover visible, never to follow someone forever.';

COMMENT ON COLUMN pen_name_history.previous_name IS
    'The name the writer used BEFORE the rename. The new name is already on the pen_names row; recording the old one is the entire point.';

-- The only read this table has: "what was this person called recently",
-- newest first.
CREATE INDEX idx_pen_name_history_user_changed
    ON pen_name_history (user_id, changed_at DESC);

-- +migrate Down

DROP TABLE pen_name_history;

-- The column goes before the table it references.
DROP INDEX IF EXISTS idx_novels_pen_name;
ALTER TABLE novels DROP COLUMN IF EXISTS pen_name_id;

DROP TABLE pen_names;

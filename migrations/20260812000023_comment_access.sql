-- Phase 13 - คอมเมนต์ 3 ระดับ + ตรวจก่อนโพสต์
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13D).
--
-- 13K shipped `comments_enabled` as one boolean, and one boolean cannot express
-- the thing this platform is actually for. "ไม่ต้องสมัครก็อ่านได้" is a promise
-- about READING; the discussion under a fiction is where a guest most wants to
-- say something, and an account requirement is exactly where they leave.
--
-- So three levels, not two:
--
--   everyone  guests may comment, with a name they type
--   members   a signed-in account is required (what `true` used to mean)
--   off       nobody may add to the thread (what `false` used to mean)
--
-- The three levels DO NOT ship alone. A guest comment carries no account to
-- warn, suspend, or hold responsible, so it is held for the author's approval
-- BY THE PLATFORM whatever `comment_approval` says - see the column comment
-- below. Shipping the levels without the queue would produce the outcome the
-- feature exists to prevent: writers opening the door once, meeting the first
-- drive-by, and closing it permanently.
--
-- Design notes:
--
--   * comments.user_id becomes NULLable, and guest_name carries the name a
--     guest typed. The CHECK requires exactly one of the two identities, so a
--     row can never be both and can never be neither.
--
--   * The comments status axis gains 'pending'. It sits BESIDE 'hidden' and
--     'removed' rather than replacing them: pending is "not decided yet",
--     hidden/removed are moderation decisions, and deleted_at remains the
--     comment author's own axis (docs/08 §20.1). The reader-facing predicate is
--     unchanged - `status = 'visible'` - so a pending comment is invisible to
--     everyone except the fiction's author, by construction rather than by a
--     filter someone has to remember.
--
--   * No guest token, no guest email, no IP column. There is nothing here that
--     could later be used to build a record of who said what from where
--     (docs/11 §34). The cost is that a guest cannot edit or delete their own
--     comment, and the UI says so before they post rather than after.

-- +migrate Up

-- ---------------------------------------------------------------------------
-- novels: the three-level switch, replacing the boolean
-- ---------------------------------------------------------------------------
ALTER TABLE novels
    ADD COLUMN comment_access   VARCHAR(16) NOT NULL DEFAULT 'members',
    ADD COLUMN comment_approval BOOLEAN     NOT NULL DEFAULT FALSE;

-- Carry every existing choice across exactly. `true` meant "signed-in readers
-- may comment", which is 'members'; it never meant guests could.
UPDATE novels
SET comment_access = CASE WHEN comments_enabled THEN 'members' ELSE 'off' END;

ALTER TABLE novels
    ADD CONSTRAINT novels_comment_access_valid
        CHECK (comment_access IN ('everyone', 'members', 'off'));

ALTER TABLE novels DROP COLUMN comments_enabled;

COMMENT ON COLUMN novels.comment_access IS
    'Who may add to this fiction''s thread: everyone (guests included) / members / off (§13D).';
COMMENT ON COLUMN novels.comment_approval IS
    'Hold MEMBER comments for the author''s approval. Guest comments are always held, whatever this says.';

-- ---------------------------------------------------------------------------
-- comments: guest identity and the pending state
-- ---------------------------------------------------------------------------
ALTER TABLE comments
    ALTER COLUMN user_id DROP NOT NULL;

ALTER TABLE comments
    ADD COLUMN guest_name VARCHAR(40);

ALTER TABLE comments
    ADD CONSTRAINT comments_one_identity
        CHECK (num_nonnulls(user_id, guest_name) = 1);

ALTER TABLE comments
    DROP CONSTRAINT comments_status_check;

ALTER TABLE comments
    ADD CONSTRAINT comments_status_check
        CHECK (status IN ('visible', 'pending', 'hidden', 'removed'));

-- The author's review queue: one fiction's undecided comments, newest first.
-- Partial, because the queue is a rounding error next to the comments table and
-- an index over every row would be paid for by every insert.
CREATE INDEX comments_pending_idx
    ON comments (novel_id, created_at DESC)
    WHERE status = 'pending' AND deleted_at IS NULL;

COMMENT ON COLUMN comments.guest_name IS
    'The name a guest typed. NULL for an account comment; exactly one identity is present (§13D).';

-- +migrate Down

DROP INDEX IF EXISTS comments_pending_idx;

-- A pending comment predates any decision. Down-migrating cannot invent one, so
-- it takes the reader-safe reading: undecided means not shown.
UPDATE comments SET status = 'hidden' WHERE status = 'pending';

ALTER TABLE comments
    DROP CONSTRAINT IF EXISTS comments_status_check;
ALTER TABLE comments
    ADD CONSTRAINT comments_status_check
        CHECK (status IN ('visible', 'hidden', 'removed'));

-- Guest comments cannot survive a schema with no way to attribute them.
DELETE FROM comments WHERE user_id IS NULL;

ALTER TABLE comments
    DROP CONSTRAINT IF EXISTS comments_one_identity,
    DROP COLUMN IF EXISTS guest_name;

ALTER TABLE comments
    ALTER COLUMN user_id SET NOT NULL;

ALTER TABLE novels
    ADD COLUMN comments_enabled BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE novels SET comments_enabled = (comment_access <> 'off');

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_comment_access_valid,
    DROP COLUMN IF EXISTS comment_approval,
    DROP COLUMN IF EXISTS comment_access;

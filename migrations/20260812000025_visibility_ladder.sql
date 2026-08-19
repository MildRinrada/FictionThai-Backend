-- Phase 13 - บันไดการมองเห็น
-- (docs/PHASE-13-CREATION-AND-CONTROL.md §13C).
--
-- Three values could only say "everyone", "anyone with the link", or "nobody".
-- The two rungs writers actually ask for were both missing, and both are the
-- same request: publish, but not to the open internet.
--
--   public     ทุกคน - listed, indexed, readable by a guest
--   members    เฉพาะสมาชิก - a signed-in reader; still listed, so it is
--              findable, and the gate is at the door rather than the map
--   followers  เฉพาะผู้ติดตาม - readers who follow the author. NOT listed:
--              a work whose audience is a named set has no business appearing
--              in a browse surface for people outside it
--   unlisted   ลิงก์ลับ - reachable by link, never discoverable
--   private    ส่วนตัว - the author alone
--
-- The order in the CHECK is deliberate and is the order the UI presents: widest
-- first, so the ladder reads downward into narrowness.
--
-- Nothing here touches the draft rule. novels_draft_visibility still requires a
-- draft to be private, because a draft is not a smaller audience - it is work
-- that has not been published at all (docs/08 §7.1).
--
-- The SQL predicate that reads this column is novels.ReadableSQLFor, and 13C
-- flagged the shape it had to take before this migration could land: an
-- ALLOWLIST. The old predicate was `visibility <> 'private'`, which would have
-- silently published every fiction on both new rungs the moment this CHECK
-- widened. The Go side changed first; this widens the column behind it.

-- +migrate Up

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_visibility_valid;

ALTER TABLE novels
    ADD CONSTRAINT novels_visibility_valid
        CHECK (visibility IN ('public', 'members', 'followers', 'unlisted', 'private'));

COMMENT ON COLUMN novels.visibility IS
    'Who may reach the work: public / members / followers / unlisted / private (§13C). Independent of status.';

-- The follower gate resolves through user_follows on every read of a
-- followers-only fiction, keyed by (follower_id, following_id). That is the
-- table's primary key, so no new index is needed.

-- +migrate Down

-- Narrowing the vocabulary must never widen an audience. Both new rungs fold
-- DOWN to unlisted: reachable by someone who already has the link, invisible to
-- everyone else. Folding them up to public would publish work its author
-- deliberately kept off the open internet.
UPDATE novels SET visibility = 'unlisted' WHERE visibility IN ('members', 'followers');

ALTER TABLE novels
    DROP CONSTRAINT IF EXISTS novels_visibility_valid;

ALTER TABLE novels
    ADD CONSTRAINT novels_visibility_valid
        CHECK (visibility IN ('private', 'unlisted', 'public'));

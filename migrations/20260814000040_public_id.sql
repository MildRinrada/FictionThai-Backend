-- public_id - the short, permanent handle at the end of every fiction address.
--
-- A slug made from a title alone collides the moment two writers pick the same
-- title, and on a fanfiction platform they will: "x reader", "one shot
-- collection", "test". The old answer was to append a random suffix ONLY to the
-- unlucky second one, which meant two fictions with the same name got addresses
-- of different shapes and neither writer could predict which they would get.
--
-- Every fiction now carries its own short id and every slug ends with it:
--
--     genshin-impact-x-reader-headcanon-one-shot-collection-b7k2m9
--
-- Duplicate titles stop being a problem at all, the shape is the same for
-- everyone, and the id is stable: it is a column, not a fragment of the slug,
-- so renaming a fiction cannot change it.
--
-- Lowercase only, from an alphabet with no vowels and no confusable characters
-- (see pkg/slug): the id cannot accidentally spell a word, cannot be misread
-- over the phone, and cannot depend on the case a URL happened to preserve.

-- +migrate Up

ALTER TABLE novels ADD COLUMN public_id VARCHAR(16);

COMMENT ON COLUMN novels.public_id IS
    'Short permanent handle, lowercase and unambiguous. Every slug ends with it, so identical titles never collide. Assigned once at creation and never changed - a rename must not move a fiction''s address.';

-- Backfill: existing rows get one derived from their own id, so the value is
-- deterministic for a given row and this migration is safe to re-run against a
-- restored snapshot. Sixteen hex characters, not six: six gave ~16.7M values,
-- and a database holding tens of thousands of rows walks into a birthday
-- collision that makes the unique index below unbuildable. Sixty-four bits
-- cannot. Rows created by the application still get the short 6-character
-- handle from pkg/slug.
UPDATE novels
SET public_id = substr(md5(id::text), 1, 16)
WHERE public_id IS NULL;

-- UNIQUE rather than a primary key: it is an address component, not the
-- identity of the row, and nothing joins on it.
CREATE UNIQUE INDEX novels_public_id_key ON novels (public_id);

-- +migrate Down

DROP INDEX IF EXISTS novels_public_id_key;
ALTER TABLE novels DROP COLUMN IF EXISTS public_id;

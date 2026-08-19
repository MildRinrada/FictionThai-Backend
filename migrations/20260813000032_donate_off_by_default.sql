-- +migrate Up

-- 13V - money is opt-in, never a default.
--
-- show_donate shipped defaulting TRUE on the reasoning that an author who set
-- a support link wants support. The review overruled it: a money-facing
-- control must be something the writer turned ON for this fiction, not
-- something they discover was on. Existing rows keep whatever their authors
-- have; only what a new fiction starts as changes.
ALTER TABLE novels ALTER COLUMN show_donate SET DEFAULT FALSE;

-- +migrate Down
ALTER TABLE novels ALTER COLUMN show_donate SET DEFAULT TRUE;

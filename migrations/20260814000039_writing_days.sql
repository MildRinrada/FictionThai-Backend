-- ตัวนับคำวันนี้ - one row per writer per day.
--
-- The navbar shows "วันนี้ 1,240 คำ" quietly, and it needs a number that is
-- true. It cannot be derived from `chapters`: a chapter carries its CURRENT
-- word count and its last-updated stamp, so summing chapters touched today
-- would count a 4,000-word chapter as 4,000 words written because the writer
-- fixed one typo in it. What is wanted is the DELTA at each save, and a delta
-- exists only at the moment of saving - so it is recorded then.
--
-- Only growth is recorded. A writer who cuts 800 words has not written -800
-- words; they have edited, which is writing too, and a counter that goes
-- backwards when someone tightens their prose would punish exactly the
-- behaviour a fiction platform wants. The day never drops below what it
-- reached.
--
-- The day is the WRITER'S day, computed in Asia/Bangkok by the application
-- rather than by the database's clock, so "วันนี้" means the day the writer is
-- living in and a deploy to a UTC host cannot move midnight.
--
-- This table is also what Wrapped will read: a year of these rows is the whole
-- "words written this year" answer, at one row per active day.

-- +migrate Up

CREATE TABLE writing_days (
    user_id UUID    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day     DATE    NOT NULL,
    words   INTEGER NOT NULL DEFAULT 0 CHECK (words >= 0),
    PRIMARY KEY (user_id, day)
);

COMMENT ON TABLE writing_days IS
    'Words a writer ADDED on one day, in their own timezone (Asia/Bangkok). Written as a delta at save time because a chapter row cannot answer it after the fact. Never negative: deleting text is editing, not un-writing.';

COMMENT ON COLUMN writing_days.day IS
    'The writer''s local date, supplied by the application. Not now()::date - the database''s day is the host''s day, and the writer does not live there.';

-- +migrate Down

DROP TABLE IF EXISTS writing_days;

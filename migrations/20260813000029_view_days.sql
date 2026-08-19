-- Phase 13R - "ผู้อ่านสัปดาห์นี้"
--
-- novels.view_count is a running total and can only ever answer "how many, ever".
-- The studio overview asks a different question - how many THIS WEEK - and a
-- running total cannot be differenced after the fact. This is the smallest table
-- that can answer it: one row per fiction per day.
--
-- Design notes:
--
--   * It is a COUNTER, not a history. The row holds a fiction, a date, and a
--     number. There is no reader, no session, no chapter, and no time of day, so
--     it cannot be turned into "who read what" no matter who reads the table -
--     which keeps the studio's own promise to writers ("เราไม่เก็บสถิติการอ่าน
--     รายบุคคล") true. The de-duplication that decides whether a read counts at
--     all still happens in Redis against a salted hash that expires within the
--     day and is never written here.
--
--   * It is written by the same background flusher that already updates
--     novels.view_count, in the same statement batch, so counting a view still
--     costs the reader's request nothing.
--
--   * The date is UTC, matching the de-duplication window the recorder uses. A
--     "week" is therefore seven of these rows, which is close enough for a number
--     a writer reads as a trend rather than as an invoice.

-- +migrate Up

CREATE TABLE IF NOT EXISTS novel_view_days (
    novel_id   UUID NOT NULL REFERENCES novels(id) ON DELETE CASCADE,
    day        DATE NOT NULL,
    view_count BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (novel_id, day)
);

COMMENT ON TABLE novel_view_days IS
    'Daily view totals per fiction (13R). A counter, never a reading history: '
    'no reader, no session, no chapter, no time of day. Written in batches by '
    'the view flusher alongside novels.view_count.';

-- The studio asks for one fiction over a recent range, which is exactly the
-- primary key's leading column plus a bounded scan - no second index needed.

-- +migrate Down

DROP TABLE IF EXISTS novel_view_days;

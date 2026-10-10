-- 0001: the daemon's runtime state (ADR-001 D13, amendment A1).
--
-- Every time is INTEGER nanoseconds since the Unix epoch, UTC: integers order
-- and compare exactly, which the retention cut-off relies on. To read one in
-- sqlite3: datetime(focused_at / 1000000000, 'unixepoch').
--
-- The messages table belongs to messaging (NERD-5256) and arrives in its own
-- migration. Tables are STRICT, so a value of the wrong type is an error.

-- workers binds each logical worker id to its current pane (I1).
CREATE TABLE workers (
    logical_id TEXT    NOT NULL PRIMARY KEY CHECK (logical_id <> ''),
    project    TEXT    NOT NULL,
    -- "group" is an SQL keyword.
    grp        TEXT    NOT NULL,
    pane_id    TEXT    NOT NULL CHECK (pane_id <> ''),
    generation INTEGER NOT NULL CHECK (generation >= 0),
    -- The workforce commands (NERD-5254) define the values; a CHECK here
    -- would cost a table rebuild to change.
    status     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    retired_at INTEGER CHECK (retired_at IS NULL OR retired_at >= created_at)
) STRICT;

-- handoffs records each replacement of a worker's session (D7, I1 to I4).
-- Finished ones (acknowledged or failed) are pruned 14 days after their last
-- change (A1).
CREATE TABLE handoffs (
    id         INTEGER PRIMARY KEY,
    logical_id TEXT    NOT NULL REFERENCES workers (logical_id),
    old_pane   TEXT    NOT NULL CHECK (old_pane <> ''),
    new_pane   TEXT    NOT NULL CHECK (new_pane <> ''),
    state      TEXT    NOT NULL DEFAULT 'pending'
                       CHECK (state IN ('pending', 'acknowledged', 'failed')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL CHECK (updated_at >= created_at)
) STRICT;

CREATE INDEX handoffs_by_worker ON handoffs (logical_id);
CREATE INDEX handoffs_by_state ON handoffs (state, updated_at);

-- focus is the recently focused panes, newest last, that Back reads (D5).
CREATE TABLE focus (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    pane_id    TEXT    NOT NULL CHECK (pane_id <> ''),
    focused_at INTEGER NOT NULL
) STRICT;

-- focus_cap keeps the newest 32 rows (store.FocusCap): the insert that adds
-- the 33rd deletes the oldest in the same statement, whoever the writer is.
-- It counts rows rather than trusting seq to have no gaps.
CREATE TRIGGER focus_cap AFTER INSERT ON focus
BEGIN
    DELETE FROM focus
    WHERE seq NOT IN (SELECT seq FROM focus ORDER BY seq DESC LIMIT 32);
END;

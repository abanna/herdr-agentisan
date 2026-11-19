-- 0011: the user's acknowledgements of board posts (ADR-001 A74, NERD-5533).
-- One row per post the user acknowledged in the dashboard's board panel. It
-- is a record, not a mark: it moves neither reader's mark (board_marks). A
-- post's acknowledgement goes with it when the board's prune deletes it.
CREATE TABLE board_acks (
    seq      INTEGER PRIMARY KEY REFERENCES board_posts (seq) ON DELETE CASCADE,
    project  TEXT    NOT NULL,
    acked_at INTEGER NOT NULL
) STRICT;

CREATE INDEX board_acks_by_project ON board_acks (project, acked_at);

-- The user's acknowledgement of the messages waiting in a boss's inbox, which
-- are not posts yet (held, A74): one row per project and boss. `A` in the
-- board panel acknowledged every message then queued for the boss: seqs up
-- to through_seq, last changed at or before through_at. A message queued
-- later, a notice added to a waiting one and a message returned to the boss
-- each change updated_at, so each counts again. A later `A` replaces the row.
CREATE TABLE board_held_acks (
    project     TEXT    NOT NULL,
    boss        TEXT    NOT NULL CHECK (boss <> ''),
    through_seq INTEGER NOT NULL CHECK (through_seq >= 0),
    through_at  INTEGER NOT NULL CHECK (through_at >= 0),
    acked_at    INTEGER NOT NULL,
    PRIMARY KEY (project, boss)
) STRICT;

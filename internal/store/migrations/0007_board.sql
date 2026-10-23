-- 0007: the team board (ADR-001 A60, NERD-5533).
--
-- board_posts holds every post on each team's board, oldest first by seq:
-- the team channel's posts by workers, panes and bosses, and the daemon
-- channel's notices. Only the boss and the user read it. seq is
-- AUTOINCREMENT, so a seq is never handed out twice, even after every post
-- was pruned: a read mark at an old seq must never hide a new post. Posts go
-- 30 days after they were made, and past 2,000 a project, oldest first
-- (store.PruneBoard).
CREATE TABLE board_posts (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    id          TEXT    NOT NULL UNIQUE CHECK (id <> ''),
    project     TEXT    NOT NULL,
    channel     TEXT    NOT NULL CHECK (channel IN ('team', 'daemon')),
    author_kind TEXT    NOT NULL CHECK (author_kind IN ('worker', 'boss', 'pane', 'daemon')),
    author      TEXT    NOT NULL CHECK (author <> ''),
    kind        TEXT    NOT NULL CHECK (kind <> ''),
    subject     TEXT    NOT NULL DEFAULT '',
    worker      TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL CHECK (body <> ''),
    created_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX board_posts_by_project ON board_posts (project, seq);

-- board_marks is how far each reader of a project's board has read: every
-- post up to and including seq. The boss's mark moves when it reads, the
-- user's when the dashboard's panel shows a post; neither moves the other.
CREATE TABLE board_marks (
    project TEXT    NOT NULL,
    reader  TEXT    NOT NULL CHECK (reader IN ('boss', 'user')),
    seq     INTEGER NOT NULL CHECK (seq >= 0),
    PRIMARY KEY (project, reader)
) STRICT;

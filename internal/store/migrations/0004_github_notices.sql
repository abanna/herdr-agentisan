-- 0004: the state behind the boss's GitHub notices (ADR-001 A45, NERD-5326).
--
-- The GitHub collector diffs each successful open-PR list of a project's
-- repo against these tables, so the boss is told each event once, across
-- restarts. Rows are keyed by project as well as repo: two projects may name
-- one repo, and each is told. Every row goes 30 days after the collector
-- last observed it (store.PruneGitHub). A clock that steps back never moves
-- last_seen back.
--
-- github_seeded marks a repo whose first collection was recorded. That one
-- tells nothing: every event already true went into github_seen. Without the
-- mark, a repo with no rows could not tell "never seeded" from "nothing
-- happened", and each restart would seed again and swallow what happened
-- while the daemon was down.
CREATE TABLE github_seeded (
    project    TEXT    NOT NULL CHECK (project <> ''),
    repo       TEXT    NOT NULL CHECK (repo <> ''),
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (project, repo)
) STRICT;

-- github_open is the repo's open PRs at its last successful collection. A PR
-- that leaves the list is looked up: merged or closed, its row goes.
CREATE TABLE github_open (
    project    TEXT    NOT NULL CHECK (project <> ''),
    repo       TEXT    NOT NULL CHECK (repo <> ''),
    pr         INTEGER NOT NULL CHECK (pr > 0),
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (project, repo, pr)
) STRICT;

-- github_seen is every event the boss was told, or that its team skips, by
-- kind and key: a merge commit, a head commit, or '' for once per PR. An open
-- PR's rows are observed again at each collection.
CREATE TABLE github_seen (
    project    TEXT    NOT NULL CHECK (project <> ''),
    repo       TEXT    NOT NULL CHECK (repo <> ''),
    pr         INTEGER NOT NULL CHECK (pr > 0),
    kind       TEXT    NOT NULL CHECK (kind <> ''),
    key        TEXT    NOT NULL,
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (project, repo, pr, kind, key)
) STRICT;
CREATE INDEX github_seen_by_age ON github_seen (last_seen);

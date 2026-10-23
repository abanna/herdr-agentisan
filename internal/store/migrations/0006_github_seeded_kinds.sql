-- 0006: a seed mark per kind of GitHub notice (ADR-001 A45, NERD-5326).
--
-- github_seeded marks a repo whose first collection recorded the kinds 0004
-- shipped with. A kind added later needs a mark of its own: in a repo
-- already seeded, its first collection would otherwise tell everything
-- already true of it, such as every open PR's latest review. That
-- collection records it and tells nothing, as github_seeded's first did.
-- Each mark is observed again at every successful collection, and goes 30
-- days after it last was (store.PruneGitHub).
CREATE TABLE github_seeded_kinds (
    project    TEXT    NOT NULL CHECK (project <> ''),
    repo       TEXT    NOT NULL CHECK (repo <> ''),
    kind       TEXT    NOT NULL CHECK (kind <> ''),
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (project, repo, kind)
) STRICT;

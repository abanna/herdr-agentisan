-- 0009: the notice admission ledger (ADR-001 A60, NERD-5533 B0a). Dormant:
-- nothing in production writes it yet. This migration is one-way: a build
-- older than it refuses the database (ErrSchemaTooNew), so no build holding
-- it is installed before the board's activation.
--
-- notice_identities is permanent: no row is ever deleted, so a notice once
-- admitted is never admitted again. A GitHub notice is also unique by its
-- receipt (project, repo, PR, kind, key): its capture ID covers when it was
-- observed and its text, so the same event seen after a restart gets a new
-- ID, and the receipt still catches it. A row is pending until it is
-- settled: posted to the board, or dropped (skipped, muted or a repeat). An
-- imported row is an event the boss was already told before the ledger
-- (github_seen), recorded so it is never told again; it has no payload.
CREATE TABLE notice_identities (
    capture_id  TEXT    NOT NULL PRIMARY KEY CHECK (capture_id <> ''),
    project     TEXT    NOT NULL,
    source      TEXT    NOT NULL CHECK (source IN ('github', 'daemon', 'team')),
    kind        TEXT    NOT NULL CHECK (kind <> '' AND length(CAST(kind AS BLOB)) <= 64),
    repo        TEXT,
    pr          INTEGER CHECK (pr BETWEEN 1 AND 2147483647),
    receipt_key TEXT,
    state       TEXT    NOT NULL CHECK (state IN ('pending', 'imported', 'posted', 'dropped')),
    admitted_at INTEGER NOT NULL,
    settled_at  INTEGER,
    post_seq    INTEGER,
    drop_reason TEXT    CHECK (drop_reason IN ('skipped', 'muted', 'repeat')),
    attempts    INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    CHECK ((source = 'github') = (repo IS NOT NULL)),
    CHECK ((repo IS NULL) = (pr IS NULL) AND (repo IS NULL) = (receipt_key IS NULL)),
    CHECK ((state IN ('pending', 'imported')) = (settled_at IS NULL)),
    CHECK ((state = 'dropped') = (drop_reason IS NOT NULL)),
    CHECK ((state = 'posted') = (post_seq IS NOT NULL)),
    UNIQUE (project, repo, pr, kind, receipt_key)
) STRICT;
CREATE INDEX notice_identities_by_state ON notice_identities (project, state, admitted_at);
CREATE INDEX notice_identities_by_time  ON notice_identities (admitted_at);
CREATE INDEX notice_identities_by_pr    ON notice_identities (project, repo, pr, state);
-- store_state serves the store-wide count of one state, such as every
-- pending notice, which by_state, led by the project, cannot. It is a plain
-- index, not a partial one on pending rows: SQLite uses a partial index only
-- when the query repeats its WHERE literally, so a bound state would scan
-- every identity.
CREATE INDEX notice_identities_store_state ON notice_identities (state);

-- notice_payloads holds the frozen envelope of each identity not yet
-- settled. bytes is the length of its source's canonical encoding and digest
-- that encoding's SHA-256, lowercase hex: a read recomputes both, so a row
-- edited by hand is never handed on.
CREATE TABLE notice_payloads (
    capture_id      TEXT    NOT NULL PRIMARY KEY REFERENCES notice_identities (capture_id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL CHECK (version = 1),
    receipt_version INTEGER CHECK (receipt_version = 1),
    subject         TEXT    NOT NULL,
    worker          TEXT    NOT NULL,
    observed_at     INTEGER NOT NULL,
    text            TEXT    NOT NULL CHECK (text <> ''),
    bytes           INTEGER NOT NULL CHECK (bytes > 0),
    digest          TEXT    NOT NULL CHECK (length(digest) = 64 AND digest NOT GLOB '*[^0-9a-f]*')
) STRICT;

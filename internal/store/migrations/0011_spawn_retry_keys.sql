-- worker spawn's retry keys (ADR-001 D11 and its retry-key amendment,
-- NERD-5254). The file's number is whatever is free when it merges (P10).
--
-- A plain worker spawn carries a retry key: 32 hex characters the boss's
-- wrapper mints, new for each request and the same on its retries.
-- spawn_requests records the request a key was first used for, as it was
-- asked: the project the first attempt resolved to, the group and count, the
-- model, effort and provider as passed ('' for none), and the task packet's
-- SHA-256 ('' for none), never its text (A50). spawn_request_workers holds
-- the ids the spawn planned, in order, written under the workforce lock
-- before the first tab opens, and, once each is bound, its generation.
--
-- A record is running until its answer is stored: outcome, the code the
-- daemon answered; message, the refusal's (NULL when ok); answer, the
-- SpawnResult as JSON (NULL unless ok). A running record no op holds was left
-- by a daemon that died, or by an answer that could not be stored; only a
-- retry with its key settles it, from its workers' rows, never the daemon
-- alone (I5). Records are pruned after their retention, running or done, with
-- their planned rows, except a running one with a bound row still starting.
CREATE TABLE spawn_requests (
    retry_key   TEXT    NOT NULL PRIMARY KEY
                        CHECK (length(CAST(retry_key AS BLOB)) = 32 AND length(retry_key) = 32
                               AND retry_key NOT GLOB '*[^0-9a-f]*'),
    project     TEXT    NOT NULL CHECK (project <> ''),
    grp         TEXT    NOT NULL CHECK (grp <> ''),
    count       INTEGER NOT NULL CHECK (count BETWEEN 1 AND 64),
    model       TEXT    NOT NULL CHECK (length(CAST(model AS BLOB)) <= 128),
    effort      TEXT    NOT NULL CHECK (length(CAST(effort AS BLOB)) = length(effort) AND length(effort) <= 16
                                        AND effort NOT GLOB '*[^a-z]*'),
    provider    TEXT    NOT NULL CHECK (provider IN ('', 'claude', 'codex')),
    task_digest TEXT    NOT NULL CHECK (task_digest = ''
                                        OR (length(CAST(task_digest AS BLOB)) = 64 AND length(task_digest) = 64
                                            AND task_digest NOT GLOB '*[^0-9a-f]*')),
    state       TEXT    NOT NULL CHECK (state IN ('running', 'done')),
    outcome     TEXT    CHECK (outcome IS NULL
                               OR (length(CAST(outcome AS BLOB)) = length(outcome) AND length(outcome) BETWEEN 1 AND 32
                                   AND outcome NOT GLOB '*[^a-z_]*')),
    message     TEXT    CHECK (message IS NULL
                               OR (message <> '' AND length(CAST(message AS BLOB)) <= 16384)),
    answer      TEXT    CHECK (answer IS NULL
                               OR (json_valid(answer) AND length(CAST(answer AS BLOB)) <= 65536)),
    created_at  INTEGER NOT NULL,
    finished_at INTEGER CHECK (finished_at IS NULL OR finished_at >= created_at),
    CHECK ((state = 'done') = (finished_at IS NOT NULL)),
    CHECK ((state = 'done') = (outcome IS NOT NULL)),
    CHECK ((answer IS NOT NULL) = (outcome IS 'ok')),
    CHECK ((message IS NOT NULL) = (outcome IS NOT NULL AND outcome IS NOT 'ok'))
) STRICT;

-- The prune walks created_at.
CREATE INDEX spawn_requests_by_time ON spawn_requests (created_at);

-- One row per planned id, in order (seq from 1). bound_id and generation are
-- NULL until the id is bound under the key, then set in the bind's own
-- transaction, once: no row a keyed spawn binds is ever without its link, and
-- a link can name only an id the key planned. workers rows are never deleted
-- and their logical_id never changes, so bound_id needs no index of its own.
CREATE TABLE spawn_request_workers (
    retry_key  TEXT    NOT NULL REFERENCES spawn_requests (retry_key) ON DELETE CASCADE,
    seq        INTEGER NOT NULL CHECK (seq BETWEEN 1 AND 64),
    logical_id TEXT    NOT NULL CHECK (logical_id <> ''),
    bound_id   TEXT    REFERENCES workers (logical_id),
    generation INTEGER CHECK (generation IS NULL OR generation >= 0),
    CHECK (bound_id IS NULL OR bound_id = logical_id),
    CHECK ((bound_id IS NULL) = (generation IS NULL)),
    PRIMARY KEY (retry_key, seq),
    UNIQUE (retry_key, logical_id)
) STRICT;

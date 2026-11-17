-- 0010: the notice ledger's running totals (NERD-5533 B0a.3-totals). Dormant:
-- nothing in production writes the ledger yet. One-way, as 0009 is.
--
-- Quotas Q6 and Q7 compare how many identities a project and the whole
-- store hold. Counting notice_identities would read up to 100,000 and
-- 500,000 rows under the write lock on every admission; these totals make
-- each check one row. Triggers keep them in step with every insert, every
-- delete and every change of project, in the writer's own transaction.
-- A REPLACE's hidden delete fires notice_counts_delete only on a connection
-- with recursive_triggers on. The store's own connection sets it, and
-- checks it at open. On any other connection, a sqlite3 shell included, a
-- REPLACE that collides here leaves the totals one too high per replaced
-- row, which fails closed until the recount in docs/architecture.md
-- repairs them.
-- A project's row is made by its first identity and stays, at 0, when
-- its last one is deleted or moved. A project may be '', if B0a.4 admits
-- the ◆ spaces' notices, so the store's total is a row of its own, never
-- a project's.
CREATE TABLE notice_counts (
    project    TEXT    NOT NULL PRIMARY KEY,
    identities INTEGER NOT NULL CHECK (identities >= 0)
) STRICT;
CREATE TABLE notice_store_counts (
    id         INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    identities INTEGER NOT NULL CHECK (identities >= 0)
) STRICT;

-- The backfill: a database 0009 already wrote starts with its true totals.
INSERT INTO notice_counts (project, identities)
    SELECT project, count(*) FROM notice_identities GROUP BY project;
INSERT INTO notice_store_counts (id, identities)
    SELECT 1, count(*) FROM notice_identities;

CREATE TRIGGER notice_counts_insert AFTER INSERT ON notice_identities
BEGIN
    INSERT INTO notice_counts (project, identities) VALUES (NEW.project, 1)
        ON CONFLICT (project) DO UPDATE SET identities = identities + 1;
    UPDATE notice_store_counts SET identities = identities + 1 WHERE id = 1;
END;

-- Identities are meant to be permanent, but the schema does not enforce it
-- yet (B0b will). Until then a delete or a change of project is counted,
-- never refused.
CREATE TRIGGER notice_counts_delete AFTER DELETE ON notice_identities
BEGIN
    UPDATE notice_counts SET identities = identities - 1 WHERE project = OLD.project;
    UPDATE notice_store_counts SET identities = identities - 1 WHERE id = 1;
END;
CREATE TRIGGER notice_counts_move AFTER UPDATE OF project ON notice_identities
WHEN NEW.project IS NOT OLD.project
BEGIN
    UPDATE notice_counts SET identities = identities - 1 WHERE project = OLD.project;
    INSERT INTO notice_counts (project, identities) VALUES (NEW.project, 1)
        ON CONFLICT (project) DO UPDATE SET identities = identities + 1;
END;

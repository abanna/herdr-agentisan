-- 0002: messaging's inbox (ADR-001 D8 and I3, NERD-5256).
--
-- A message is addressed to a worker by logical id, to a team's boss
-- (<short>-boss, no workers row), or to a bare pane: a sender with no other
-- identity, which is where its notices go. The daemon itself only sends.
--
-- A message to a worker belongs to one binding: generation is the workers
-- row's when it was queued. A handoff keeps the generation
-- (store.CompleteHandoff), so the replacement session receives what was
-- queued for the worker (I3). A logical id reused after its retirement gets
-- the next generation (NERD-5254), so it never inherits the old inbox: those
-- messages go back to the boss (store.ReturnToBoss). sender_generation pins a
-- worker sender the same way, for the notice a failure sends it.
--
-- seq is the inbox order, oldest first. It is the insert order, which a clock
-- that steps back cannot reorder the way it would reorder the ULIDs.
CREATE TABLE messages (
    seq               INTEGER PRIMARY KEY,
    id                TEXT    NOT NULL UNIQUE CHECK (length(id) = 26),
    recipient_kind    TEXT    NOT NULL CHECK (recipient_kind IN ('worker', 'boss', 'pane')),
    recipient         TEXT    NOT NULL CHECK (recipient <> ''),
    -- The project of the worker it was sent to; '' for a boss or a pane.
    project           TEXT    NOT NULL,
    generation        INTEGER CHECK ((recipient_kind = 'worker') = (generation IS NOT NULL) AND (generation IS NULL OR generation >= 0)),
    sender_kind       TEXT    NOT NULL CHECK (sender_kind IN ('worker', 'boss', 'pane', 'daemon')),
    sender            TEXT    NOT NULL CHECK (sender <> ''),
    sender_generation INTEGER CHECK ((sender_kind = 'worker') = (sender_generation IS NOT NULL) AND (sender_generation IS NULL OR sender_generation >= 0)),
    body              TEXT    NOT NULL CHECK (body <> ''),
    state             TEXT    NOT NULL DEFAULT 'queued'
                              CHECK (state IN ('queued', 'dispatching', 'delivered', 'failed')),
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- Why the last attempt did not deliver it.
    reason            TEXT    NOT NULL DEFAULT '',
    -- A notice's: the message whose failure it reports. No foreign key: the
    -- two are pruned on their own clocks.
    notice_of         TEXT    NOT NULL DEFAULT '',
    -- A message returned to the boss: the worker it was sent to.
    returned_from     TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL CHECK (updated_at >= created_at)
) STRICT;

-- One prompt in flight per recipient: a second claim cannot even be written.
CREATE UNIQUE INDEX messages_one_in_flight ON messages (recipient_kind, recipient) WHERE state = 'dispatching';
-- The inbox, oldest first.
CREATE INDEX messages_inbox ON messages (recipient_kind, recipient, state, seq);
-- Pruning: delivered and failed messages go 14 days after their last change (A1).
CREATE INDEX messages_by_state ON messages (state, updated_at);

-- One pending handoff per worker. A pending handoff holds the worker's inbox
-- (store.Claim) until it is completed or failed.
CREATE UNIQUE INDEX handoffs_one_pending ON handoffs (logical_id) WHERE state = 'pending';

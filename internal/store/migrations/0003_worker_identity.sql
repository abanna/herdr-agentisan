-- 0003: a workers row's identity (ADR-001 A35, NERD-5327).
--
-- pane_id is now a hint: herdr gives a pane moved to another workspace a new
-- id, and can reissue a closed workspace's ids. A row binds the pane herdr
-- lists with its terminal_id, which herdr never reissues but renews on a live
-- handoff and a restart. shell_pid and shell_start are the pane's shell as the
-- team poll first saw it bound, which a live handoff hands over: the poll
-- re-keys a row to the pane holding that same shell, and otherwise marks it
-- lost. Both are NULL until the poll has read them. A row written before this
-- migration has no terminal and goes lost at the first reconcile.
ALTER TABLE workers ADD COLUMN terminal_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN shell_pid INTEGER CHECK (shell_pid IS NULL OR shell_pid > 0);
ALTER TABLE workers ADD COLUMN shell_start INTEGER CHECK ((shell_start IS NULL) = (shell_pid IS NULL) AND (shell_start IS NULL OR shell_start >= 0));

-- A handoff completes onto the binding it began on: the worker's generation
-- then, and the new pane's terminal, which completing binds. A handoff begun
-- before this migration has no generation, so it cannot complete; it is
-- failed, as one whose worker was retired meanwhile.
ALTER TABLE handoffs ADD COLUMN generation INTEGER CHECK (generation IS NULL OR generation >= 0);
ALTER TABLE handoffs ADD COLUMN new_terminal_id TEXT NOT NULL DEFAULT '';

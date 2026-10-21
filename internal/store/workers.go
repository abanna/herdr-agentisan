package store

import (
	"context"
	"errors"
	"fmt"
)

// StatusLost is a live row the team poll could not prove on any pane
// (ADR-001 A35): herdr no longer lists its terminal, and no pane that may
// hold it now has its shell. It binds no pane, but it is not retired: it
// keeps its generation and inbox until the boss retires it or reuses its id.
// The poll never writes a lost row again.
const StatusLost = "lost"

// ErrWorkerChanged means a workers row is no longer the one a write was
// planned on: it was retired, rebound at another generation or terminal,
// or marked lost since it was read. The write changes nothing.
var ErrWorkerChanged = errors.New("the workers row changed since it was read")

// Worker is one row of the workers table that is not retired: a logical
// worker and the pane it is bound to now (I1). The workforce commands
// (NERD-5254) write the table; the daemon's team poll keeps each row's
// binding (A35), and its team model reads it to give the panes it finds
// their logical ids (D5).
type Worker struct {
	LogicalID string
	Project   string
	Group     string
	// PaneID is the pane the row was last seen on: a hint, since herdr
	// gives a pane moved to another workspace a new id and can reissue a
	// closed workspace's ids. The row binds its terminal's pane (A35).
	PaneID     string
	Generation int64
	// TerminalID is the herdr terminal the row binds; "" for a row written
	// before A35, which binds nothing.
	TerminalID string
	// ShellPID and ShellStart are the shell of the row's pane, as the team
	// poll first read them; 0 until then. ShellStart is in
	// report.SystemStat's units, so it is only ever compared for equality.
	ShellPID   uint32
	ShellStart uint64
	Status     string
	// HandoffPending is true while a handoff holds the worker's inbox.
	HandoffPending bool
}

// Workers returns every worker not retired, ordered by logical id. Nothing in
// the schema stops two of them naming one pane; both are returned, and the
// caller decides.
func (s *Store) Workers(ctx context.Context) ([]Worker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT logical_id, project, grp, pane_id, generation, terminal_id,
	coalesce(shell_pid, 0), coalesce(shell_start, 0), status,
	EXISTS (SELECT 1 FROM handoffs AS h WHERE h.logical_id = workers.logical_id AND h.state = 'pending')
FROM workers WHERE retired_at IS NULL ORDER BY logical_id`)
	if err != nil {
		return nil, fmt.Errorf("%w: read workers: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	out := []Worker{}
	for rows.Next() {
		var w Worker
		if err := rows.Scan(&w.LogicalID, &w.Project, &w.Group, &w.PaneID, &w.Generation, &w.TerminalID,
			&w.ShellPID, &w.ShellStart, &w.Status, &w.HandoffPending); err != nil {
			return nil, fmt.Errorf("%w: read workers: %w", ErrStore, err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read workers: %w", ErrStore, err)
	}
	return out, nil
}

// The team poll's writes (A35). Each is a compare-and-set on the row as the
// poll read it: its logical id, generation and terminal, still live and not
// lost. A row the workforce commands changed meanwhile is left alone
// (ErrWorkerChanged), and the next poll judges it as it is then, so the poll
// and the commands share no lock. In each statement, ?1 to ?3 are the row's
// logical id, generation and terminal as read.
const (
	rebindWorker = `UPDATE workers SET pane_id = ?4
WHERE logical_id = ?1 AND generation = ?2 AND terminal_id = ?3 AND retired_at IS NULL AND status <> 'lost'`
	setWorkerShell = `UPDATE workers SET shell_pid = ?4, shell_start = ?5
WHERE logical_id = ?1 AND generation = ?2 AND terminal_id = ?3 AND retired_at IS NULL AND status <> 'lost'`
	rekeyWorker = `UPDATE workers SET pane_id = ?4, terminal_id = ?5
WHERE logical_id = ?1 AND generation = ?2 AND terminal_id = ?3 AND retired_at IS NULL AND status <> 'lost'`
	markWorkerLost = `UPDATE workers SET status = 'lost'
WHERE logical_id = ?1 AND generation = ?2 AND terminal_id = ?3 AND retired_at IS NULL AND status <> 'lost'
	AND NOT EXISTS (SELECT 1 FROM handoffs AS h WHERE h.logical_id = ?1 AND h.state = 'pending')`
)

// RebindWorker records that herdr now lists w's terminal under pane: the
// pane moved to another workspace.
func (s *Store) RebindWorker(ctx context.Context, w Worker, pane string) error {
	if err := checkID(KindPane, pane); err != nil {
		return err
	}
	return s.updateWorker(ctx, w, "rebind", rebindWorker, pane)
}

// SetWorkerShell records the shell of w's pane: its pid, never 0, and its
// start time.
func (s *Store) SetWorkerShell(ctx context.Context, w Worker, pid uint32, start uint64) error {
	if pid == 0 {
		return fmt.Errorf("%w: shell pid 0 names no process", ErrInvalid)
	}
	return s.updateWorker(ctx, w, "record the shell of", setWorkerShell, pid, start)
}

// RekeyWorker binds w to terminal in pane: herdr renewed every terminal
// (a live handoff), and that pane holds w's shell.
func (s *Store) RekeyWorker(ctx context.Context, w Worker, pane, terminal string) error {
	if err := errors.Join(checkID(KindPane, pane), checkTerminal(terminal)); err != nil {
		return err
	}
	return s.updateWorker(ctx, w, "re-key", rekeyWorker, pane, terminal)
}

// MarkWorkerLost marks w lost, unless a handoff of it is pending: the
// handoff's new pane is the worker's next binding, and its caller completes
// or fails it.
func (s *Store) MarkWorkerLost(ctx context.Context, w Worker) error {
	return s.updateWorker(ctx, w, "mark lost", markWorkerLost)
}

// updateWorker runs stmt, one of the writes above, on w's row as it was
// read, with values as ?4 on.
func (s *Store) updateWorker(ctx context.Context, w Worker, what, stmt string, values ...any) error {
	args := append([]any{w.LogicalID, w.Generation, w.TerminalID}, values...)
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrStore, what, w.LogicalID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("%w: %s %s", ErrWorkerChanged, what, w.LogicalID)
	}
	return nil
}

// checkTerminal refuses a terminal id no pane can have.
func checkTerminal(id string) error {
	return checkID("terminal", id)
}

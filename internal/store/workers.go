package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
	// Routing is the routing decision the worker was launched with; nil
	// when routing did not launch it. BindWorker writes it and WorkerRow
	// reads it; Workers leaves it nil.
	Routing *Routing
}

// Routing is the routing decision a worker was launched with (ADR-001 A50):
// the role and provider it was made for, the binding, model id and effort it
// chose, the override that chose them, and the assignment and policy it was
// made by. Each field but RoutedAt is the decision's own, unchecked here.
type Routing struct {
	Role, Provider, Binding, ModelID, Effort string
	// OverrideBinding, OverrideBy and OverrideEffort are the override's;
	// empty for none.
	OverrideBinding, OverrideBy, OverrideEffort string
	AssignmentDigest, PolicyVersion             string
	RoutedAt                                    time.Time
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

// The statuses a workers row goes through (ADR-001 A23, A35). A row is
// starting from the moment its tab exists until its agent is up, and active
// after; StatusLost while the team poll can prove it on no pane; retired,
// failed and gone end it, with retired_at set.
const (
	StatusStarting = "starting"
	StatusActive   = "active"
	// StatusRetired is a worker the boss retired.
	StatusRetired = "retired"
	// StatusFailed is a worker whose agent never came up; its pane was
	// closed.
	StatusFailed = "failed"
	// StatusGone is a worker whose proven pane herdr stopped listing while
	// an op watched it.
	StatusGone = "gone"
)

// ErrWorkerLive means a logical id is already bound and not retired, or is
// another project's.
var ErrWorkerLive = errors.New("worker is already bound")

// ErrNoWorker means no row has the logical id.
var ErrNoWorker = errors.New("no such worker")

// Row is one row of the workers table, retired or not.
type Row struct {
	Worker
	CreatedAt time.Time
	// RetiredAt is zero while the worker is not retired.
	RetiredAt time.Time
}

// bindWorker inserts a logical id never seen, or rebinds a retired or lost
// one of the same project: the new pane and terminal and group, the routing
// it was spawned with, the next generation, starting again, with the shell
// left for the team poll to read. Any other row is left alone, and changes
// nothing: another project's id is never taken over, since its queue and
// handoffs are that project's (QA #78 F4).
const bindWorker = `INSERT INTO workers (logical_id, project, grp, pane_id, terminal_id, generation, status, created_at,
	routed_at, routed_role, routed_provider, routed_binding, routed_model_id, routed_effort,
	override_binding, override_by, override_effort, assignment_digest, policy_version)
VALUES (?1, ?2, ?3, ?4, ?5, 0, 'starting', ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17)
ON CONFLICT (logical_id) DO UPDATE SET
	grp = excluded.grp, pane_id = excluded.pane_id, terminal_id = excluded.terminal_id,
	shell_pid = NULL, shell_start = NULL,
	generation = workers.generation + 1, status = 'starting', retired_at = NULL,
	routed_at = excluded.routed_at, routed_role = excluded.routed_role, routed_provider = excluded.routed_provider,
	routed_binding = excluded.routed_binding, routed_model_id = excluded.routed_model_id, routed_effort = excluded.routed_effort,
	override_binding = excluded.override_binding, override_by = excluded.override_by, override_effort = excluded.override_effort,
	assignment_digest = excluded.assignment_digest, policy_version = excluded.policy_version
WHERE (workers.retired_at IS NOT NULL OR workers.status = 'lost') AND workers.project = excluded.project
RETURNING generation`

// failStaleHandoffs fails every handoff still pending for a logical id that
// is being bound again: it belonged to an earlier generation, so it must not
// hold the new one's inbox or block its own handoff.
const failStaleHandoffs = `UPDATE handoffs SET state = 'failed', updated_at = max(?2, created_at)
WHERE logical_id = ?1 AND state = 'pending'`

// BindWorker binds w's logical id to its pane and terminal as a starting
// worker, with w's routing (none when nil), and returns the row's
// generation: 0 for an id never seen, one more than before for one reused
// after its retirement or once lost (I1, A35).
// A handoff an earlier generation left pending fails in the same
// transaction, whatever path left it (#29's QA N2). w.Generation is ignored.
// ErrWorkerLive means the id is bound, not retired and not lost, or is
// another project's, and then nothing changes.
func (s *Store) BindWorker(ctx context.Context, w Worker, at time.Time) (int64, error) {
	if err := errors.Join(checkID(KindWorker, w.LogicalID), checkID(KindPane, w.PaneID), checkTerminal(w.TerminalID)); err != nil {
		return 0, err
	}
	ns, err := nanos(at)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: bind worker %s: %w", ErrStore, w.LogicalID, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	routed, err := routingArgs(w.Routing)
	if err != nil {
		return 0, err
	}
	var gen int64
	err = tx.QueryRowContext(ctx, bindWorker, append([]any{w.LogicalID, w.Project, w.Group, w.PaneID, w.TerminalID, ns}, routed...)...).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s is live, or is another project's than %s", ErrWorkerLive, w.LogicalID, w.Project)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: bind worker %s: %w", ErrStore, w.LogicalID, err)
	}
	if _, err := tx.ExecContext(ctx, failStaleHandoffs, w.LogicalID, ns); err != nil {
		return 0, fmt.Errorf("%w: bind worker %s: %w", ErrStore, w.LogicalID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: bind worker %s: %w", ErrStore, w.LogicalID, err)
	}
	return gen, nil
}

// routingArgs are bindWorker's ?7 to ?17 for r: every one NULL for none.
func routingArgs(r *Routing) ([]any, error) {
	if r == nil {
		return make([]any, 11), nil
	}
	at, err := nanos(r.RoutedAt)
	if err != nil {
		return nil, err
	}
	orNull := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	return []any{
		at, r.Role, r.Provider, r.Binding, r.ModelID, r.Effort,
		orNull(r.OverrideBinding), orNull(r.OverrideBy), orNull(r.OverrideEffort), r.AssignmentDigest, r.PolicyVersion,
	}, nil
}

// endsWorker are the statuses that retire a row.
var endsWorker = map[string]bool{StatusRetired: true, StatusFailed: true, StatusGone: true}

// setWorkerStatus moves the live row at a generation to a status. ?4 is the
// retirement time, NULL for starting and active, which never bring a lost row
// back: only BindWorker does.
const setWorkerStatus = `UPDATE workers SET status = ?3, retired_at = max(?4, created_at)
WHERE logical_id = ?1 AND generation = ?2 AND retired_at IS NULL AND (?4 IS NOT NULL OR status <> 'lost')`

// SetWorkerStatus moves w's row, at w's generation and not retired, to status
// at at. Retired, failed and gone retire it, lost or not, at at, or at its
// creation time if at is earlier. ErrNoWorker means no such row is live: it
// was retired, rebound at another generation, or, for starting and active,
// marked lost.
func (s *Store) SetWorkerStatus(ctx context.Context, w Worker, status string, at time.Time) error {
	if status != StatusStarting && status != StatusActive && !endsWorker[status] {
		return fmt.Errorf("%w: worker status %q", ErrInvalid, status)
	}
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	var retired any
	if endsWorker[status] {
		retired = ns
	}
	res, err := s.db.ExecContext(ctx, setWorkerStatus, w.LogicalID, w.Generation, status, retired)
	if err != nil {
		return fmt.Errorf("%w: set worker %s %s: %w", ErrStore, w.LogicalID, status, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("%w: %s is not a live worker at generation %d", ErrNoWorker, w.LogicalID, w.Generation)
	}
	return nil
}

// OtherProjectsIDs returns the logical id of every row of a project other
// than project, retired or not, in logical id order: ids a spawn into project
// must not take, since one id has one row across every project and
// BindWorker never takes another project's (QA #78 F4).
func (s *Store) OtherProjectsIDs(ctx context.Context, project string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT logical_id FROM workers WHERE project <> ? ORDER BY logical_id", project)
	if err != nil {
		return nil, fmt.Errorf("%w: read other projects' workers: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%w: read other projects' workers: %w", ErrStore, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read other projects' workers: %w", ErrStore, err)
	}
	return out, nil
}

// WorkerRow returns the row for logicalID, retired or not. ErrNoWorker means
// there is none.
func (s *Store) WorkerRow(ctx context.Context, logicalID string) (Row, error) {
	var (
		r         Row
		created   int64
		retiredAt sql.NullInt64
		routedAt  sql.NullInt64
		rt        Routing
		override  [3]sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT logical_id, project, grp, pane_id, terminal_id, generation, status, created_at, retired_at,
	routed_at, coalesce(routed_role, ''), coalesce(routed_provider, ''), coalesce(routed_binding, ''),
	coalesce(routed_model_id, ''), coalesce(routed_effort, ''), override_binding, override_by, override_effort,
	coalesce(assignment_digest, ''), coalesce(policy_version, '')
FROM workers WHERE logical_id = ?`,
		logicalID).Scan(&r.LogicalID, &r.Project, &r.Group, &r.PaneID, &r.TerminalID, &r.Generation, &r.Status, &created, &retiredAt,
		&routedAt, &rt.Role, &rt.Provider, &rt.Binding, &rt.ModelID, &rt.Effort, &override[0], &override[1], &override[2],
		&rt.AssignmentDigest, &rt.PolicyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, fmt.Errorf("%w: %s", ErrNoWorker, logicalID)
	}
	if err != nil {
		return Row{}, fmt.Errorf("%w: read worker %s: %w", ErrStore, logicalID, err)
	}
	r.CreatedAt = time.Unix(0, created)
	if retiredAt.Valid {
		r.RetiredAt = time.Unix(0, retiredAt.Int64)
	}
	if routedAt.Valid {
		rt.RoutedAt = time.Unix(0, routedAt.Int64)
		rt.OverrideBinding, rt.OverrideBy, rt.OverrideEffort = override[0].String, override[1].String, override[2].String
		r.Routing = &rt
	}
	return r, nil
}

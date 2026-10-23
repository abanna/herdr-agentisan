package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrHandoffPending means the worker has a handoff pending: BeginHandoff
	// starts no second one, and SetWorkerStatus ends no row while it lasts.
	ErrHandoffPending = errors.New("the worker already has a handoff pending")
	// ErrNoHandoff means no handoff with the id is pending.
	ErrNoHandoff = errors.New("no such pending handoff")
	// ErrWorkerStarting means BeginHandoff found the worker still starting:
	// its agent is not up yet, and spawn may still mark it failed.
	ErrWorkerStarting = errors.New("the worker is still starting")
)

// Handoff is a pending handoff: the worker it replaces, at the generation
// it completes onto, and the session replacing it.
type Handoff struct {
	ID        int64
	LogicalID string
	// Generation is -1 for a handoff begun before migration 0003, which
	// completes onto no row.
	Generation int64
	OldPane    string
	NewPane    string
	// NewTerminal is the new pane's terminal when the handoff began; "" for
	// one begun before migration 0003.
	NewTerminal string
	CreatedAt   time.Time
	// Project and Group are the worker's row's at the handoff's generation;
	// "" when no row has that generation.
	Project, Group string
}

// PendingHandoffs returns every pending handoff, oldest first. BeginHandoff
// allows one per worker, so the list is short; the caller matches it against
// herdr's panes.
func (s *Store) PendingHandoffs(ctx context.Context) ([]Handoff, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.id, h.logical_id, coalesce(h.generation, -1), h.old_pane, h.new_pane, h.new_terminal_id, h.created_at,
	coalesce(w.project, ''), coalesce(w.grp, '')
FROM handoffs AS h LEFT JOIN workers AS w ON w.logical_id = h.logical_id AND w.generation = h.generation
WHERE h.state = 'pending' ORDER BY h.id`)
	if err != nil {
		return nil, fmt.Errorf("%w: read pending handoffs: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	out := []Handoff{}
	for rows.Next() {
		var h Handoff
		var created int64
		if err := rows.Scan(&h.ID, &h.LogicalID, &h.Generation, &h.OldPane, &h.NewPane, &h.NewTerminal, &created, &h.Project, &h.Group); err != nil {
			return nil, fmt.Errorf("%w: read pending handoffs: %w", ErrStore, err)
		}
		h.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read pending handoffs: %w", ErrStore, err)
	}
	return out, nil
}

// BeginHandoff starts replacing logicalID's session with the one in newPane,
// whose terminal is newTerminal (D7), and returns the handoff's id. The
// worker must be live and not lost (ErrNotBound), past starting
// (ErrWorkerStarting), newPane and newTerminal not the ones it is on, and no
// other handoff pending for it (ErrHandoffPending).
// The handoff records the worker's generation, the binding it completes onto
// (A35).
//
// While the handoff is pending it holds the worker's inbox: Claim takes
// nothing for it, so no message reaches the old session once it may be
// writing its handoff. A dispatch already in flight finishes into the old
// session, which is still running then.
func (s *Store) BeginHandoff(ctx context.Context, logicalID, newPane, newTerminal string, at time.Time) (int64, error) {
	if err := errors.Join(checkID(KindWorker, logicalID), checkID(KindPane, newPane), checkTerminal(newTerminal)); err != nil {
		return 0, err
	}
	ns, err := nanos(at)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: begin handoff: %w", ErrStore, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	var oldPane, oldTerminal, status string
	var gen int64
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT pane_id, terminal_id, generation, status,
	EXISTS (SELECT 1 FROM handoffs WHERE logical_id = ?1 AND state = 'pending')
FROM workers WHERE logical_id = ?1 AND retired_at IS NULL`, logicalID).Scan(&oldPane, &oldTerminal, &gen, &status, &pending)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %s", ErrNotBound, logicalID)
	case err != nil:
		return 0, fmt.Errorf("%w: begin handoff: %w", ErrStore, err)
	case status == StatusLost:
		return 0, fmt.Errorf("%w: %s is lost: no pane is proven to hold it", ErrNotBound, logicalID)
	case status == StatusStarting:
		return 0, fmt.Errorf("%w: %s: its agent is not up yet", ErrWorkerStarting, logicalID)
	case pending:
		return 0, fmt.Errorf("%w: %s", ErrHandoffPending, logicalID)
	case oldPane == newPane || oldTerminal == newTerminal:
		return 0, fmt.Errorf("%w: %s is already on %s (%s)", ErrInvalid, logicalID, newPane, newTerminal)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO handoffs (logical_id, old_pane, new_pane, generation, new_terminal_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id",
		logicalID, oldPane, newPane, gen, newTerminal, ns, ns).Scan(&id); err != nil {
		return 0, fmt.Errorf("%w: begin handoff: %w", ErrStore, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: begin handoff: %w", ErrStore, err)
	}
	return id, nil
}

// CompleteHandoff binds the worker to the handoff's new pane and terminal and
// ends the handoff, in one transaction, which releases the worker's inbox to
// the replacement (I3, A2's atomic rebind). The messages queued before and
// during the handoff are then claimed one at a time, each exactly once,
// since a claim is one statement that moves a message out of queued. The
// shell is the replacement's, which the team poll reads next (A35); the
// status is left as it is.
//
// The worker keeps its generation (I1): its queued messages belong to the
// replacement. Only a logical id reused after its retirement starts a new
// generation, whose session inherits nothing.
//
// ErrNoHandoff means id is not pending. ErrNotBound means the worker is no
// longer live at the handoff's generation: it was retired, or its id reused,
// meanwhile. The handoff then stays pending for the caller to fail. The
// team poll may have rebound or re-keyed the worker meanwhile; that does
// not stop it.
func (s *Store) CompleteHandoff(ctx context.Context, id int64, at time.Time) error {
	return s.completeHandoff(ctx, id, nil, at)
}

// CompleteHandoffOnto completes handoff id as CompleteHandoff does, but binds
// the new pane at terminal, the one the caller proved it shows: the recorded
// one, or the one a herdr live handoff renewed it to since the handoff began
// (A33 part 2b-1). A terminal some live row binds already, the worker's own
// included, is ErrInvalid, with nothing changed.
func (s *Store) CompleteHandoffOnto(ctx context.Context, id int64, terminal string, at time.Time) error {
	if err := checkTerminal(terminal); err != nil {
		return err
	}
	return s.completeHandoff(ctx, id, &terminal, at)
}

// completeHandoff is CompleteHandoff onto the recorded terminal, or onto
// *onto when it is set.
func (s *Store) completeHandoff(ctx context.Context, id int64, onto *string, at time.Time) error {
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	var logicalID, newPane, newTerminal string
	var gen sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT logical_id, generation, new_pane, new_terminal_id FROM handoffs WHERE id = ? AND state = 'pending'", id).
		Scan(&logicalID, &gen, &newPane, &newTerminal)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrNoHandoff, id)
	}
	if err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	if onto != nil {
		newTerminal = *onto
		var bound bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM workers WHERE terminal_id = ? AND retired_at IS NULL)", newTerminal).Scan(&bound); err != nil {
			return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
		}
		if bound {
			return fmt.Errorf("%w: a live worker binds terminal %s already", ErrInvalid, newTerminal)
		}
	}
	// A handoff begun before migration 0003 has no generation, which
	// matches no row.
	res, err := tx.ExecContext(ctx, `UPDATE workers SET pane_id = ?3, terminal_id = ?4, shell_pid = NULL, shell_start = NULL
WHERE logical_id = ?1 AND generation = ?2 AND retired_at IS NULL`, logicalID, gen, newPane, newTerminal)
	if err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("%w: %s is no longer live at the handoff's generation", ErrNotBound, logicalID)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE handoffs SET state = 'acknowledged', updated_at = max(?2, created_at) WHERE id = ?1", id, ns); err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	return nil
}

// HandoffAcknowledgedOnto reports whether a handoff of logicalID at
// generation completed onto newPane: what an ack repeated after a crash past
// its complete finds (A33 part 2b-1).
func (s *Store) HandoffAcknowledgedOnto(ctx context.Context, logicalID string, generation int64, newPane string) (bool, error) {
	var ok bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM handoffs
WHERE logical_id = ? AND generation = ? AND new_pane = ? AND state = 'acknowledged')`, logicalID, generation, newPane).Scan(&ok); err != nil {
		return false, fmt.Errorf("%w: read handoffs of %s: %w", ErrStore, logicalID, err)
	}
	return ok, nil
}

// FailHandoff ends a handoff that did not complete. The worker stays bound
// where it was, and its inbox is released there. ErrNoHandoff means id is
// not pending.
func (s *Store) FailHandoff(ctx context.Context, id int64, at time.Time) error {
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE handoffs SET state = 'failed', updated_at = max(?2, created_at) WHERE id = ?1 AND state = 'pending'", id, ns)
	if err != nil {
		return fmt.Errorf("%w: fail handoff: %w", ErrStore, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("%w: %d", ErrNoHandoff, id)
	}
	return nil
}

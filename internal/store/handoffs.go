package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrHandoffPending means the worker already has a handoff pending.
	ErrHandoffPending = errors.New("the worker already has a handoff pending")
	// ErrNoHandoff means no handoff with the id is pending.
	ErrNoHandoff = errors.New("no such pending handoff")
)

// BeginHandoff starts replacing logicalID's session with the one in newPane
// (D7) and returns the handoff's id. The worker must be live (ErrNotBound),
// newPane a pane it is not on, and no other handoff pending for it
// (ErrHandoffPending).
//
// While the handoff is pending it holds the worker's inbox: Claim takes
// nothing for it, so no message reaches the old session once it may be
// writing its handoff. A dispatch already in flight finishes into the old
// session, which is still running then.
func (s *Store) BeginHandoff(ctx context.Context, logicalID, newPane string, at time.Time) (int64, error) {
	if err := errors.Join(checkID(KindWorker, logicalID), checkID(KindPane, newPane)); err != nil {
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
	var oldPane string
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT pane_id, EXISTS (SELECT 1 FROM handoffs WHERE logical_id = ?1 AND state = 'pending')
FROM workers WHERE logical_id = ?1 AND retired_at IS NULL`, logicalID).Scan(&oldPane, &pending)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %s", ErrNotBound, logicalID)
	case err != nil:
		return 0, fmt.Errorf("%w: begin handoff: %w", ErrStore, err)
	case pending:
		return 0, fmt.Errorf("%w: %s", ErrHandoffPending, logicalID)
	case oldPane == newPane:
		return 0, fmt.Errorf("%w: %s is already on %s", ErrInvalid, logicalID, newPane)
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO handoffs (logical_id, old_pane, new_pane, created_at, updated_at) VALUES (?, ?, ?, ?, ?) RETURNING id",
		logicalID, oldPane, newPane, ns, ns).Scan(&id); err != nil {
		return 0, fmt.Errorf("%w: begin handoff: %w", ErrStore, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: begin handoff: %w", ErrStore, err)
	}
	return id, nil
}

// CompleteHandoff binds the worker to the handoff's new pane and ends the
// handoff, in one transaction, which releases the worker's inbox to the
// replacement (I3, A2's atomic rebind). The messages queued before and
// during the handoff are then claimed one at a time, each exactly once,
// since a claim is one statement that moves a message out of queued.
//
// The worker keeps its generation (I1): its queued messages belong to the
// replacement. Only a logical id reused after its retirement starts a new
// generation, whose session inherits nothing.
//
// ErrNoHandoff means id is not pending. ErrNotBound means the worker was
// retired or moved meanwhile; the handoff stays pending for the caller to
// fail.
func (s *Store) CompleteHandoff(ctx context.Context, id int64, at time.Time) error {
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	var logicalID, oldPane, newPane string
	err = tx.QueryRowContext(ctx, "SELECT logical_id, old_pane, new_pane FROM handoffs WHERE id = ? AND state = 'pending'", id).
		Scan(&logicalID, &oldPane, &newPane)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrNoHandoff, id)
	}
	if err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE workers SET pane_id = ?3 WHERE logical_id = ?1 AND pane_id = ?2 AND retired_at IS NULL", logicalID, oldPane, newPane)
	if err != nil {
		return fmt.Errorf("%w: complete handoff: %w", ErrStore, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("%w: %s is no longer live on %s", ErrNotBound, logicalID, oldPane)
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

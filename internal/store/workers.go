package store

import (
	"context"
	"fmt"
)

// Worker is one row of the workers table that is not retired: a logical
// worker and the pane it is bound to now (I1). The workforce commands
// (NERD-5254) write the table; the daemon's team model reads it to give the
// panes it finds their logical ids (D5).
type Worker struct {
	LogicalID  string
	Project    string
	Group      string
	PaneID     string
	Generation int64
}

// Workers returns every worker not retired, ordered by logical id. Nothing in
// the schema stops two of them naming one pane; both are returned, and the
// caller decides.
func (s *Store) Workers(ctx context.Context) ([]Worker, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT logical_id, project, grp, pane_id, generation FROM workers WHERE retired_at IS NULL ORDER BY logical_id")
	if err != nil {
		return nil, fmt.Errorf("%w: read workers: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	out := []Worker{}
	for rows.Next() {
		var w Worker
		if err := rows.Scan(&w.LogicalID, &w.Project, &w.Group, &w.PaneID, &w.Generation); err != nil {
			return nil, fmt.Errorf("%w: read workers: %w", ErrStore, err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read workers: %w", ErrStore, err)
	}
	return out, nil
}

package store_test

import (
	"os"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestUpgradeFromTheFirstSchema is the live state.db's path: a database at
// schema 1, with workers, handoffs and focus history in it, opened by this
// build is migrated in place to the newest schema. Every row survives, and
// the inbox is there and takes a message. The worker has no terminal (A35),
// so it binds nothing until the team poll judges it, and its pending
// handoff, which has no generation, cannot complete: it can only fail.
func TestUpgradeFromTheFirstSchema(t *testing.T) {
	t.Parallel()
	first, err := os.ReadFile("migrations/0001_init.sql")
	require.NoError(t, err)
	path := dbPath(t)
	v1, err := store.OpenWith(t.Context(), path, fstest.MapFS{"0001_init.sql": {Data: first}})
	require.NoError(t, err)
	require.Equal(t, 1, v1.UserVersion())
	_, err = store.DB(v1).ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at) VALUES ('w', 'agentisan', 'coders', 'w14:p2', 2, 'active', 0)")
	require.NoError(t, err)
	_, err = store.DB(v1).ExecContext(t.Context(), `INSERT INTO handoffs (id, logical_id, old_pane, new_pane, state, created_at, updated_at) VALUES
	(1, 'w', 'w14:p1', 'w14:p2', 'acknowledged', 1, 2), (2, 'w', 'w14:p2', 'w14:p3', 'pending', 3, 3)`)
	require.NoError(t, err)
	require.NoError(t, v1.RecordFocus(t.Context(), "w14:p2", t1))
	require.NoError(t, v1.Close())

	s := open(t, path)
	assert.Equal(t, store.SchemaVersion(), s.UserVersion())
	assert.Equal(t, 4, s.UserVersion())
	ws, err := s.Workers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []store.Worker{{LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w14:p2", Generation: 2, Status: "active", HandoffPending: true}}, ws)
	var handoffs int
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT count(*) FROM handoffs").Scan(&handoffs))
	assert.Equal(t, 2, handoffs)
	require.ErrorIs(t, s.CompleteHandoff(t.Context(), 2, t1), store.ErrNotBound, "begun with no generation to complete onto")
	require.NoError(t, s.FailHandoff(t.Context(), 2, t1))
	focus, err := s.RecentFocus(t.Context(), store.FocusCap)
	require.NoError(t, err)
	require.Len(t, focus, 1)
	assert.Equal(t, int64(2), send(t, s, worker("w"), boss, "after the upgrade").Generation)
}

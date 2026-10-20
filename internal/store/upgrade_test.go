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
// schema 1, with workers, a handoff and focus history in it, opened by this
// build is migrated in place to the newest schema. Every row survives, and
// the inbox is there and takes a message.
func TestUpgradeFromTheFirstSchema(t *testing.T) {
	t.Parallel()
	first, err := os.ReadFile("migrations/0001_init.sql")
	require.NoError(t, err)
	path := dbPath(t)
	v1, err := store.OpenWith(t.Context(), path, fstest.MapFS{"0001_init.sql": {Data: first}})
	require.NoError(t, err)
	require.Equal(t, 1, v1.UserVersion())
	seedWorker(t, v1, "w", "w14:p2", 2)
	_, err = store.DB(v1).ExecContext(t.Context(),
		"INSERT INTO handoffs (logical_id, old_pane, new_pane, state, created_at, updated_at) VALUES ('w', 'w14:p1', 'w14:p2', 'acknowledged', 1, 2)")
	require.NoError(t, err)
	require.NoError(t, v1.RecordFocus(t.Context(), "w14:p2", t1))
	require.NoError(t, v1.Close())

	s := open(t, path)
	assert.Equal(t, store.SchemaVersion(), s.UserVersion())
	assert.Equal(t, 2, s.UserVersion())
	pane, gen := bound(t, s, "w")
	assert.Equal(t, "w14:p2", pane)
	assert.Equal(t, int64(2), gen)
	var handoffs int
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT count(*) FROM handoffs").Scan(&handoffs))
	assert.Equal(t, 1, handoffs)
	focus, err := s.RecentFocus(t.Context(), store.FocusCap)
	require.NoError(t, err)
	require.Len(t, focus, 1)
	assert.Equal(t, int64(2), send(t, s, worker("w"), boss, "after the upgrade").Generation)
}

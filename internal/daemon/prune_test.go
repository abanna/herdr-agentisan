package daemon_test

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestRunPrunesFinishedMessages is A1's retention for the inbox, run by the
// daemon's prune: a delivered or failed message past 14 days goes, a newer
// one and a queued one stay.
func TestRunPrunesFinishedMessages(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	h := newHerdr(t, 22)
	dir := shortDir(t)
	p := paths(t, dir, h)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	st, err := store.Open(t.Context(), p.DB)
	require.NoError(t, err)
	aged := map[string]struct {
		state   string
		updated time.Time
	}{
		"old":        {state: "delivered", updated: t0.Add(-15 * day)},
		"old-failed": {state: "failed", updated: t0.Add(-15 * day)},
		"recent":     {state: "delivered", updated: t0.Add(-13 * day)},
		"queued":     {state: "queued", updated: t0.Add(-30 * day)},
	}
	for to := range aged {
		_, err := st.Send(t.Context(), store.Address{Kind: store.KindPane, ID: to}, store.Daemon, "x", t0)
		require.NoError(t, err)
	}
	require.NoError(t, st.Close())
	raw, err := sql.Open("sqlite", p.DB)
	require.NoError(t, err)
	for to, m := range aged {
		_, err := raw.ExecContext(t.Context(), "UPDATE messages SET state = ?, created_at = ?, updated_at = ? WHERE recipient = ?",
			m.state, m.updated.UnixNano(), m.updated.UnixNano(), to)
		require.NoError(t, err)
	}
	require.NoError(t, raw.Close())

	o := opts(t, dir, h)
	o.Now = func() time.Time { return t0 }
	run(t, o)
	require.Eventually(t, func() bool {
		return assert.ObjectsAreEqual([]string{"queued", "recent"}, query(t, p.DB, "SELECT recipient FROM messages ORDER BY recipient"))
	}, ready, 10*time.Millisecond, "the prune at start removes the finished messages past retention")
}

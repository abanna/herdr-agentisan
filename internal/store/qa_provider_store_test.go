package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

func TestQA146ProviderConstraint(t *testing.T) {
	s := open(t, dbPath(t))
	// Check the schema independently of BindWorker's input validation.
	for _, provider := range []any{nil, "claude", "codex", "gemini", "Claude", ""} {
		result, err := store.DB(s).ExecContext(t.Context(), `INSERT INTO workers (logical_id,project,grp,pane_id,terminal_id,generation,status,created_at,provider) VALUES ('qa146','ha','coder','w14:p4','term_qa146',0,'active',0,?)`, provider)
		if provider == nil || provider == "claude" || provider == "codex" {
			require.NoError(t, err)
			n, err := result.RowsAffected()
			require.NoError(t, err)
			require.Equal(t, int64(1), n)
			_, err = store.DB(s).ExecContext(t.Context(), `DELETE FROM workers WHERE logical_id='qa146'`)
			require.NoError(t, err)
		} else {
			require.Error(t, err, "the SQL CHECK must reject %v", provider)
		}
	}
}

func TestQA146CodexHandoffKeepsProvider(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := t.Context()
	w := store.Worker{LogicalID: "qa146", Project: "ha", Group: "coder", PaneID: "w14:p4", TerminalID: "term_a", Provider: "codex"}
	_, err := s.BindWorker(ctx, w, t1)
	require.NoError(t, err)
	require.NoError(t, s.SetWorkerStatus(ctx, w, store.StatusActive, t1))
	handoff, err := s.BeginHandoff(ctx, w.LogicalID, "w14:p5", "term_b", t1)
	require.NoError(t, err)
	require.NoError(t, s.CompleteHandoff(ctx, handoff, t1))
	row, err := s.WorkerRow(ctx, w.LogicalID)
	require.NoError(t, err)
	assert.Equal(t, "codex", row.Provider)
	assert.Equal(t, "term_b", row.TerminalID)
	require.NoError(t, s.RekeyWorker(ctx, row.Worker, "w14:p6", "term_c"))
	row, err = s.WorkerRow(ctx, w.LogicalID)
	require.NoError(t, err)
	assert.Equal(t, "codex", row.Provider)
}

package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestWorkersReadsTheActiveBindings: Workers is what the team model joins on
// (D5): every worker not retired, whatever its status, ordered by logical id
// so two reads of the same rows agree. The store only reads the table; the
// workforce commands (NERD-5254) write it, so this test writes its rows the
// way they will, through SQL.
func TestWorkersReadsTheActiveBindings(t *testing.T) {
	t.Parallel()

	const insert = "INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, retired_at) VALUES "
	tests := map[string]struct {
		rows []string
		want []store.Worker
	}{
		"no rows": {want: []store.Worker{}},
		"active rows, ordered by logical id whatever the insert order": {
			rows: []string{
				"('qa-1', 'agentisan', 'qa', 'wY:p1', 0, 'active', 1, NULL)",
				"('coders-2', 'agentisan', 'coders', 'wN:p3', 4, 'active', 1, NULL)",
				"('coders-1', 'agentisan', 'coders', 'wN:p2', 1, 'starting', 1, NULL)",
			},
			want: []store.Worker{
				{LogicalID: "coders-1", Project: "agentisan", Group: "coders", PaneID: "wN:p2", Generation: 1},
				{LogicalID: "coders-2", Project: "agentisan", Group: "coders", PaneID: "wN:p3", Generation: 4},
				{LogicalID: "qa-1", Project: "agentisan", Group: "qa", PaneID: "wY:p1"},
			},
		},
		"a retired worker is not bound to its last pane": {
			rows: []string{
				"('coders-1', 'agentisan', 'coders', 'wN:p2', 1, 'retired', 1, 2)",
				"('coders-2', 'agentisan', 'coders', 'wN:p3', 0, 'active', 1, NULL)",
			},
			want: []store.Worker{{LogicalID: "coders-2", Project: "agentisan", Group: "coders", PaneID: "wN:p3"}},
		},
		// The schema does not stop two workers naming one pane; Workers
		// reports both, in order, and the model decides (team.Build).
		"two active workers on one pane": {
			rows: []string{
				"('b', '', 'coders', 'wN:p2', 0, 'active', 1, NULL)",
				"('a', '', 'coders', 'wN:p2', 0, 'active', 1, NULL)",
			},
			want: []store.Worker{
				{LogicalID: "a", Group: "coders", PaneID: "wN:p2"},
				{LogicalID: "b", Group: "coders", PaneID: "wN:p2"},
			},
		},
		"non-ASCII ids and names are read back byte for byte": {
			rows: []string{"('cödér 1', 'prøject', '◆ grp', 'w1:p1', 0, 'active', 1, NULL)"},
			want: []store.Worker{{LogicalID: "cödér 1", Project: "prøject", Group: "◆ grp", PaneID: "w1:p1"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			for _, row := range tc.rows {
				_, err := store.DB(s).ExecContext(t.Context(), insert+row)
				require.NoError(t, err)
			}

			got, err := s.Workers(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWorkersOnAClosedStore fails the way every other read does.
func TestWorkersOnAClosedStore(t *testing.T) {
	t.Parallel()
	s, err := store.Open(t.Context(), dbPath(t))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	_, err = s.Workers(context.Background())
	require.ErrorIs(t, err, store.ErrStore)
}

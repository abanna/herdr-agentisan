package team_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // the store's driver, for writing workers rows as NERD-5254 will

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// liveLayout is the layout research found live on 2026-10-10, trimmed: the
// boss space, two group spaces, the ignored edit shell and a plain workspace.
func liveLayout() team.Layout {
	return team.Layout{
		Workspaces: []herdr.WorkspaceInfo{
			{WorkspaceID: "wJ", Label: "◆ boss"},
			{WorkspaceID: "wN", Label: "◆ coders"},
			{WorkspaceID: "wY", Label: "◆ qa"},
			{WorkspaceID: "w12", Label: "◆ herdr"},
			{WorkspaceID: "w2", Label: "notes"},
		},
		Panes: []herdr.PaneInfo{
			p("wJ:p1", "wJ", "working"), p("wN:p1", "wN", "working"), p("wN:p2", "wN", "done", "⟳ READY TO RESTART"),
			shell("wN:p3", "wN"), p("wY:p1", "wY", "idle"), shell("w12:p1", "w12"), p("w2:p1", "w2", "working"),
		},
	}
}

// TestBuildTheModel is D5's tree for the live layout: project → group →
// logical worker → current pane. Bound panes take their logical ids from
// state.db's workers rows; a pane no row binds (a hand-started agent, a
// shell) is still in the model, its provisional id its pane id.
func TestBuildTheModel(t *testing.T) {
	t.Parallel()
	rows := []store.Worker{
		{LogicalID: "boss", Project: "agentisan", Group: "boss", PaneID: "wJ:p1", TerminalID: "term_wJ:p1"},
		{LogicalID: "coders-1", Project: "agentisan", Group: "coders", PaneID: "wN:p1", TerminalID: "term_wN:p1", Generation: 2},
	}

	got := team.Build(liveLayout(), team.Spaces{Project: "agentisan"}, rows)

	wN, wY := team.Target{WorkspaceID: "wN"}, team.Target{WorkspaceID: "wY"}
	assert.Equal(t, team.Model{Projects: []team.Project{{
		Name:       "agentisan",
		Workspaces: []string{"w12", "wJ", "wN", "wY"},
		Order:      team.Spaces{}.GroupOrder(),
		Boss:       []team.Worker{{ID: "boss", Bound: true, Pane: team.Pane{ID: "wJ:p1", Agent: "claude", Status: team.StatusWorking}}},
		Groups: []team.Group{
			{Name: "coders", Targets: []team.Target{wN}, Workers: []team.Worker{
				{ID: "coders-1", Bound: true, Target: wN, Pane: team.Pane{ID: "wN:p1", Agent: "claude", Status: team.StatusWorking}},
				{ID: "wN:p2", Target: wN, Pane: team.Pane{ID: "wN:p2", Agent: "claude", Status: team.StatusDone, Handoff: true, Ctx: "40"}},
				{ID: "wN:p3", Target: wN, Pane: team.Pane{ID: "wN:p3", Status: team.StatusUnknown}},
			}},
			{Name: "qa", Targets: []team.Target{wY}, Workers: []team.Worker{
				{ID: "wY:p1", Target: wY, Pane: team.Pane{ID: "wY:p1", Agent: "claude", Status: team.StatusIdle}},
			}},
		},
	}}}, got)
}

// TestBuildJoinsWorkersRows: the join with state.db's workers rows is by
// terminal, never by pane id (A35), and only gives logical ids; where a pane
// sits is the resolver's.
func TestBuildJoinsWorkersRows(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rows []store.Worker
		want map[string]string // pane id -> worker id; a provisional id has "?" appended
	}{
		"no rows: every worker is provisional": {
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		"a row binds its pane": {
			rows: []store.Worker{{LogicalID: "qa-1", Group: "qa", PaneID: "wY:p1", TerminalID: "term_wY:p1"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "qa-1"},
		},
		// The table does not forbid it; the lowest logical id takes the
		// pane, whatever order the rows come in, so a rebuild agrees.
		"two rows on one pane": {
			rows: []store.Worker{{LogicalID: "coders-9", PaneID: "wN:p1", TerminalID: "term_wN:p1"}, {LogicalID: "coders-1", PaneID: "wN:p1", TerminalID: "term_wN:p1"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "coders-1", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// A worker whose pane herdr no longer lists has no current pane, so
		// it is not in the model; the workforce commands own that row.
		"a row whose pane is gone": {
			rows: []store.Worker{{LogicalID: "coders-7", Group: "coders", PaneID: "wN:p99", TerminalID: "term_wN:p99"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// Moved by hand to ◆ coders while its row says qa: it is counted
		// where it sits, and keeps its logical id.
		"a bound pane in another group's space": {
			rows: []store.Worker{{LogicalID: "qa-1", Group: "qa", PaneID: "wN:p3", TerminalID: "term_wN:p3"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "qa-1", "wY:p1": "wY:p1?"},
		},
		// herdr reissued the row's pane id to another terminal: a stranger.
		"a stranger at the row's pane id": {
			rows: []store.Worker{{LogicalID: "coders-1", Group: "coders", PaneID: "wN:p1", TerminalID: "term_closed"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// Moved to another workspace: a new pane id, the same terminal.
		"a row whose pane moved": {
			rows: []store.Worker{{LogicalID: "coders-1", Group: "coders", PaneID: "w7:p1", TerminalID: "term_wN:p2"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "coders-1", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		"a lost row, and one from before A35 with no terminal": {
			rows: []store.Worker{
				{LogicalID: "coders-1", PaneID: "wN:p1", TerminalID: "term_wN:p1", Status: store.StatusLost},
				{LogicalID: "coders-2", PaneID: "wN:p2"},
			},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// A row binding a pane outside the model binds nothing in it.
		"a row on the edit shell": {
			rows: []store.Worker{{LogicalID: "x", PaneID: "w12:p1", TerminalID: "term_w12:p1"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Build(liveLayout(), team.Spaces{}, tc.rows)

			got := map[string]string{}
			groupOf := map[string]string{}
			for _, pr := range m.Projects {
				all := slices.Clone(pr.Boss)
				for _, g := range pr.Groups {
					all = append(all, g.Workers...)
					for _, w := range g.Workers {
						groupOf[w.Pane.ID] = g.Name
					}
				}
				for _, w := range all {
					id := w.ID
					if !w.Bound {
						assert.Equal(t, w.Pane.ID, w.ID, "a provisional id is the pane id")
						id += "?"
					}
					got[w.Pane.ID] = id
				}
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, "coders", groupOf["wN:p3"], "placement is the resolver's, never the row's")
		})
	}
}

// TestRebuildIsEqual is D3's "a crash costs only the restart": the model is
// a function of herdr's answers and state.db alone, so building it twice
// from the same state gives equal models, whatever order herdr lists panes
// and workspaces in and whatever order the rows come in.
func TestRebuildIsEqual(t *testing.T) {
	t.Parallel()
	rows := []store.Worker{{LogicalID: "b", PaneID: "wN:p2", TerminalID: "term_wN:p2"}, {LogicalID: "a", PaneID: "wN:p1", TerminalID: "term_wN:p1"}, {LogicalID: "c", PaneID: "wJ:p1", TerminalID: "term_wJ:p1"}}
	first := team.Build(liveLayout(), team.Spaces{Project: "x"}, rows)

	shuffled := liveLayout()
	slices.Reverse(shuffled.Panes)
	slices.Reverse(shuffled.Workspaces)
	reordered := slices.Clone(rows)
	slices.Reverse(reordered)

	assert.Equal(t, first, team.Build(liveLayout(), team.Spaces{Project: "x"}, rows), "the same inputs")
	assert.Equal(t, first, team.Build(shuffled, team.Spaces{Project: "x"}, reordered), "the same state in another order")
	assert.NotEqual(t, first, team.Build(liveLayout(), team.Spaces{Project: "y"}, rows), "the project is part of the model")
}

// TestRebuildAfterARestartIsEqual: a daemon restart rebuilds the same model
// from state.db and herdr. The rows are written to a temp state.db as the
// workforce commands will write them, read through one store, and read again
// through a fresh store after the first is closed.
func TestRebuildAfterARestartIsEqual(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.db")
	build := func() team.Model {
		st, err := store.Open(t.Context(), path)
		require.NoError(t, err)
		defer func() { require.NoError(t, st.Close()) }()
		rows, err := st.Workers(t.Context())
		require.NoError(t, err)
		return team.Build(liveLayout(), team.Spaces{Project: "agentisan"}, rows)
	}
	before := build() // creates and migrates state.db

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, retired_at, terminal_id) VALUES
		('coders-1', 'agentisan', 'coders', 'wN:p1', 1, 'active', 1, NULL, 'term_wN:p1'),
		('qa-1', 'agentisan', 'qa', 'wN:p3', 0, 'active', 1, NULL, 'term_wN:p3'),
		('coders-0', 'agentisan', 'coders', 'wN:p2', 0, 'retired', 1, 2, 'term_wN:p2')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	first, second := build(), build()
	assert.Equal(t, first, second)
	assert.NotEqual(t, before, first, "the rows written in between are read")
	ids := map[string]bool{}
	for _, w := range first.Projects[0].Groups[0].Workers {
		ids[w.ID] = w.Bound
	}
	assert.Equal(t, map[string]bool{"coders-1": true, "qa-1": true, "wN:p2": false}, ids, "a retired row binds nothing")
}

// TestBuildReadsNamesLabelsAndTokens: what the snapshot shows of a pane
// comes into the model with it. agent.list gives agent names, joined by pane
// id: a name for a pane outside the model, or the empty name of an unnamed
// agent, names nothing, and a pane listed twice takes the lowest name, so
// the model does not depend on herdr's order. pane.list gives the label and
// the ctx, item and stage tokens, as written.
func TestBuildReadsNamesLabelsAndTokens(t *testing.T) {
	t.Parallel()

	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}},
		Panes: []herdr.PaneInfo{
			{
				PaneID: "wN:p1", WorkspaceID: "wN", Agent: "claude", AgentStatus: "working", Label: "left",
				Tokens: map[string]string{"ctx": "38", "item": "NERD-5255", "stage": "build_test", "other": "x"},
			},
			{PaneID: "wN:p2", WorkspaceID: "wN", Agent: "codex", AgentStatus: "idle"},
			{PaneID: "wN:p3", WorkspaceID: "wN", AgentStatus: "unknown", Label: "notes"},
		},
		Agents: []herdr.AgentInfo{
			{PaneID: "wN:p1", Name: "pee02"},
			{PaneID: "wN:p1", Name: "pee01"},
			{PaneID: "wN:p2", Name: ""},
			{PaneID: "w9:p9", Name: "elsewhere"},
		},
	}
	want := []team.Pane{
		{ID: "wN:p1", Name: "pee01", Label: "left", Agent: "claude", Status: team.StatusWorking, Ctx: "38", Item: "NERD-5255", Stage: "build_test"},
		{ID: "wN:p2", Agent: "codex", Status: team.StatusIdle},
		{ID: "wN:p3", Label: "notes", Status: team.StatusUnknown},
	}
	for name, layout := range map[string]team.Layout{"as listed": l, "listed in reverse": reversed(l)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Build(layout, team.Spaces{}, nil)
			require.Len(t, m.Projects, 1)
			require.Len(t, m.Projects[0].Groups, 1)
			var got []team.Pane
			for _, w := range m.Projects[0].Groups[0].Workers {
				got = append(got, w.Pane)
			}
			assert.Equal(t, want, got)
		})
	}
}

// reversed is l with every list in reverse order.
func reversed(l team.Layout) team.Layout {
	out := team.Layout{Workspaces: slices.Clone(l.Workspaces), Panes: slices.Clone(l.Panes), Agents: slices.Clone(l.Agents)}
	slices.Reverse(out.Workspaces)
	slices.Reverse(out.Panes)
	slices.Reverse(out.Agents)
	return out
}

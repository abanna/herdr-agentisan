package team_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // the store's driver, for writing workers rows as NERD-5254 will

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// The live team, a tabs team (A19), which every test of the model builds
// on: one workspace labelled with its project's name, the boss's tab and a
// tab per worker.
const (
	liveProject = "agentisan"
	liveShort   = "ag"
	// liveWS is its workspace, labelled liveProject.
	liveWS = "wT"
)

// liveTeam resolves the live team.
var liveTeam = team.Tabs{Project: liveProject, Short: liveShort}

// liveTarget is the live team's workspace: every worker's target, and its
// $team's.
var liveTarget = team.Target{WorkspaceID: liveWS}

// inTeam is p in the live team's tab <short>-<tab>, whose id is wT:<tab>:
// "boss" is the boss's tab, "coder-1" the first coder's.
func inTeam(p herdr.PaneInfo, tab string) herdr.PaneInfo {
	p.WorkspaceID, p.TabID = liveWS, liveWS+":"+tab
	return p
}

// liveTab is the live team's tab <short>-<tab>, as inTeam names it.
func liveTab(tab string) herdr.TabInfo {
	return herdr.TabInfo{TabID: liveWS + ":" + tab, WorkspaceID: liveWS, Label: liveShort + "-" + tab}
}

// teamOf is the live team's workspace holding panes, with the tab of each
// pane placed there, in the order the panes name them, then the empty tabs
// named in empty. A pane outside the workspace is listed, in no tab.
func teamOf(panes []herdr.PaneInfo, empty ...string) team.Layout {
	l := team.Layout{Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: liveWS, Label: liveProject}}, Panes: panes}
	seen := map[string]bool{}
	for _, p := range panes {
		tab, ok := strings.CutPrefix(p.TabID, liveWS+":")
		if p.WorkspaceID == liveWS && ok && !seen[tab] {
			seen[tab] = true
			l.Tabs = append(l.Tabs, liveTab(tab))
		}
	}
	for _, tab := range empty {
		l.Tabs = append(l.Tabs, liveTab(tab))
	}
	return l
}

// liveLayout is the layout research found live on 2026-10-10, trimmed, as a
// tabs team: the boss's tab, three coders' tabs, the third a shell, a qa's,
// a tab outside the model holding a shell, and a plain workspace.
func liveLayout() team.Layout {
	l := teamOf([]herdr.PaneInfo{
		inTeam(p("wJ:p1", "", "working"), "boss"),
		inTeam(p("wN:p1", "", "working"), "coder-1"),
		inTeam(p("wN:p2", "", "done", "⟳ READY TO RESTART"), "coder-2"),
		inTeam(shell("wN:p3", ""), "coder-3"),
		inTeam(p("wY:p1", "", "idle"), "qa-1"),
		inTeam(shell("w12:p1", ""), "notes"),
		p("w2:p1", "w2", "working"),
	})
	l.Workspaces = append(l.Workspaces, herdr.WorkspaceInfo{WorkspaceID: "w2", Label: "notes"})
	return l
}

// TestBuildTheModel is D5's tree for the live layout: project → group →
// logical worker → current pane. Bound panes take their logical ids from
// state.db's workers rows; a pane no row binds (a hand-started agent, a
// shell) is still in the model, its provisional id its pane id.
func TestBuildTheModel(t *testing.T) {
	t.Parallel()
	rows := []store.Worker{
		{LogicalID: "boss", Project: liveProject, Group: "boss", PaneID: "wJ:p1", TerminalID: "term_wJ:p1"},
		{LogicalID: "ag-coder-1", Project: liveProject, Group: "coder", PaneID: "wN:p1", TerminalID: "term_wN:p1", Generation: 2},
	}

	got := team.Build(liveLayout(), liveTeam, rows)

	tab := func(name string) string { return liveWS + ":" + name }
	assert.Equal(t, team.Model{Projects: []team.Project{{
		Name:       liveProject,
		Short:      liveShort,
		Workspaces: []string{liveWS},
		Order:      team.TypeOrder(),
		Target:     liveTarget,
		Boss:       []team.Worker{{ID: "boss", Bound: true, Pane: team.Pane{ID: "wJ:p1", TabID: tab("boss"), TerminalID: "term_wJ:p1", Agent: "claude", Status: team.StatusWorking}}},
		Groups: []team.Group{
			{Name: "coder", Targets: []team.Target{liveTarget}, Workers: []team.Worker{
				{ID: "ag-coder-1", Bound: true, Target: liveTarget, Pane: team.Pane{ID: "wN:p1", TabID: tab("coder-1"), TerminalID: "term_wN:p1", Agent: "claude", Status: team.StatusWorking}},
				{ID: "wN:p2", Target: liveTarget, Pane: team.Pane{ID: "wN:p2", TabID: tab("coder-2"), TerminalID: "term_wN:p2", Agent: "claude", Status: team.StatusDone, Handoff: true, Ctx: "40"}},
				{ID: "wN:p3", Target: liveTarget, Pane: team.Pane{ID: "wN:p3", TabID: tab("coder-3"), TerminalID: "term_wN:p3", Status: team.StatusUnknown}},
			}},
			{Name: "qa", Targets: []team.Target{liveTarget}, Workers: []team.Worker{
				{ID: "wY:p1", Target: liveTarget, Pane: team.Pane{ID: "wY:p1", TabID: tab("qa-1"), TerminalID: "term_wY:p1", Agent: "claude", Status: team.StatusIdle}},
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
		// blank is a pane herdr lists with no terminal id.
		blank string
		want  map[string]string // pane id -> worker id; a provisional id has "?" appended
	}{
		"no rows: every worker is provisional": {
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		"a row binds its pane": {
			rows: []store.Worker{{LogicalID: "ag-qa-1", Group: "qa", PaneID: "wY:p1", TerminalID: "term_wY:p1"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "ag-qa-1"},
		},
		// The table does not forbid it; the lowest logical id takes the
		// pane, whatever order the rows come in, so a rebuild agrees.
		"two rows on one pane": {
			rows: []store.Worker{{LogicalID: "ag-coder-9", PaneID: "wN:p1", TerminalID: "term_wN:p1"}, {LogicalID: "ag-coder-1", PaneID: "wN:p1", TerminalID: "term_wN:p1"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "ag-coder-1", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// A worker whose pane herdr no longer lists has no current pane, so
		// it is not in the model; the workforce commands own that row.
		"a row whose pane is gone": {
			rows: []store.Worker{{LogicalID: "ag-coder-7", Group: "coder", PaneID: "wN:p99", TerminalID: "term_wN:p99"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// Moved by hand to a coder's tab while its row says qa: it is
		// counted where it sits, and keeps its logical id.
		"a bound pane in another type's tab": {
			rows: []store.Worker{{LogicalID: "ag-qa-1", Group: "qa", PaneID: "wN:p3", TerminalID: "term_wN:p3"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "ag-qa-1", "wY:p1": "wY:p1?"},
		},
		// herdr reissued the row's pane id to another terminal: a stranger.
		"a stranger at the row's pane id": {
			rows: []store.Worker{{LogicalID: "ag-coder-1", Group: "coder", PaneID: "wN:p1", TerminalID: "term_closed"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// Moved to another workspace: a new pane id, the same terminal.
		"a row whose pane moved": {
			rows: []store.Worker{{LogicalID: "ag-coder-1", Group: "coder", PaneID: "w7:p1", TerminalID: "term_wN:p2"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "ag-coder-1", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		"a lost row, and one from before A35 with no terminal": {
			rows: []store.Worker{
				{LogicalID: "ag-coder-1", PaneID: "wN:p1", TerminalID: "term_wN:p1", Status: store.StatusLost},
				{LogicalID: "ag-coder-2", PaneID: "wN:p2"},
			},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// No terminal is no binding, on either side: a row from before A35
		// never binds a pane herdr lists with no terminal id.
		"a row and a pane with no terminal": {
			rows:  []store.Worker{{LogicalID: "ag-coder-2", PaneID: "wN:p1"}},
			blank: "wN:p1",
			want:  map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
		// A row binding a pane outside the model binds nothing in it.
		"a row on a tab outside the model": {
			rows: []store.Worker{{LogicalID: "x", PaneID: "w12:p1", TerminalID: "term_w12:p1"}},
			want: map[string]string{"wJ:p1": "wJ:p1?", "wN:p1": "wN:p1?", "wN:p2": "wN:p2?", "wN:p3": "wN:p3?", "wY:p1": "wY:p1?"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := liveLayout()
			for i := range l.Panes {
				if l.Panes[i].PaneID == tc.blank {
					l.Panes[i].TerminalID = ""
				}
			}
			m := team.Build(l, liveTeam, tc.rows)

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
			assert.Equal(t, "coder", groupOf["wN:p3"], "placement is the resolver's, never the row's")
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
	first := team.Build(liveLayout(), liveTeam, rows)

	shuffled := liveLayout()
	slices.Reverse(shuffled.Panes)
	slices.Reverse(shuffled.Workspaces)
	slices.Reverse(shuffled.Tabs)
	reordered := slices.Clone(rows)
	slices.Reverse(reordered)

	// The same layout, its workspace labelled with another project's name.
	other := liveLayout()
	other.Workspaces[0].Label = "y"

	assert.Equal(t, first, team.Build(liveLayout(), liveTeam, rows), "the same inputs")
	assert.Equal(t, first, team.Build(shuffled, liveTeam, reordered), "the same state in another order")
	assert.NotEqual(t, first, team.Build(other, team.Tabs{Project: "y", Short: liveShort}, rows), "the project is part of the model")
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
		return team.Build(liveLayout(), liveTeam, rows)
	}
	before := build() // creates and migrates state.db

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, retired_at, terminal_id) VALUES
		('ag-coder-1', 'agentisan', 'coder', 'wN:p1', 1, 'active', 1, NULL, 'term_wN:p1'),
		('ag-qa-1', 'agentisan', 'qa', 'wN:p3', 0, 'active', 1, NULL, 'term_wN:p3'),
		('ag-coder-0', 'agentisan', 'coder', 'wN:p2', 0, 'retired', 1, 2, 'term_wN:p2')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	first, second := build(), build()
	assert.Equal(t, first, second)
	assert.NotEqual(t, before, first, "the rows written in between are read")
	require.Len(t, first.Projects, 1)
	require.NotEmpty(t, first.Projects[0].Groups)
	ids := map[string]bool{}
	for _, w := range first.Projects[0].Groups[0].Workers {
		ids[w.ID] = w.Bound
	}
	assert.Equal(t, map[string]bool{"ag-coder-1": true, "ag-qa-1": true, "wN:p2": false}, ids, "a retired row binds nothing")
}

// TestBuildReadsNamesLabelsAndTokens: what the snapshot shows of a pane
// comes into the model with it. agent.list gives agent names, joined by pane
// id: a name for a pane outside the model, or the empty name of an unnamed
// agent, names nothing, and a pane listed twice takes the lowest name, so
// the model does not depend on herdr's order. pane.list gives the label and
// the ctx, item and stage tokens, as written, but ctx only from a pane herdr
// detects an agent in (ADR-001 A27).
func TestBuildReadsNamesLabelsAndTokens(t *testing.T) {
	t.Parallel()

	l := teamOf([]herdr.PaneInfo{
		inTeam(herdr.PaneInfo{
			PaneID: "wN:p1", Agent: "claude", AgentStatus: "working", Label: "left",
			Tokens: map[string]string{"ctx": "38", "item": "NERD-5255", "stage": "build_test", "other": "x"},
		}, "coder-1"),
		inTeam(herdr.PaneInfo{PaneID: "wN:p2", Agent: "codex", AgentStatus: "idle"}, "coder-2"),
		inTeam(herdr.PaneInfo{PaneID: "wN:p3", AgentStatus: "unknown", Label: "notes", Tokens: map[string]string{"ctx": "61", "item": "NERD-5318"}}, "coder-3"),
	})
	l.Agents = []herdr.AgentInfo{
		{PaneID: "wN:p1", Name: "pee02"},
		{PaneID: "wN:p1", Name: "pee01"},
		{PaneID: "wN:p2", Name: ""},
		{PaneID: "w9:p9", Name: "elsewhere"},
	}
	want := []team.Pane{
		{ID: "wN:p1", TabID: "wT:coder-1", Name: "pee01", Label: "left", Agent: "claude", Status: team.StatusWorking, Ctx: "38", Item: "NERD-5255", Stage: "build_test"},
		{ID: "wN:p2", TabID: "wT:coder-2", Agent: "codex", Status: team.StatusIdle},
		{ID: "wN:p3", TabID: "wT:coder-3", Label: "notes", Status: team.StatusUnknown, Item: "NERD-5318"},
	}
	for name, layout := range map[string]team.Layout{"as listed": l, "listed in reverse": reversed(l)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Build(layout, liveTeam, nil)
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
	out := team.Layout{Workspaces: slices.Clone(l.Workspaces), Panes: slices.Clone(l.Panes), Agents: slices.Clone(l.Agents), Tabs: slices.Clone(l.Tabs)}
	slices.Reverse(out.Workspaces)
	slices.Reverse(out.Panes)
	slices.Reverse(out.Agents)
	slices.Reverse(out.Tabs)
	return out
}

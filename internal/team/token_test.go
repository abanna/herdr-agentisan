package team_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// p is a pane in workspace ws with an agent status; handoff, when set, is its
// handoff token.
func p(id, ws, status string, handoff ...string) herdr.PaneInfo {
	pane := herdr.PaneInfo{PaneID: id, TerminalID: "term_" + id, WorkspaceID: ws, AgentStatus: status}
	if status != "unknown" {
		pane.Agent = "claude"
	}
	if len(handoff) > 0 {
		pane.Tokens = map[string]string{"handoff": handoff[0], "ctx": "40"}
	}
	return pane
}

// shell is a plain shell: no agent, status unknown.
func shell(id, ws string) herdr.PaneInfo {
	return herdr.PaneInfo{PaneID: id, TerminalID: "term_" + id, WorkspaceID: ws, AgentStatus: "unknown"}
}

// TestTeamTokenGoldens is the `$team` format (ADR-001 A28, amending A17's
// frozen v12 one): "N agents · W working · R ready", then " · B blocked" when
// any are, and "1 agent" for one. N counts every pane in the team's worker
// tabs, shells and unknown included; W counts working; R counts the panes carrying a
// handoff token, whatever their status; B counts blocked. Idle and done are
// not counted. The middle dots are spelled U+00B7 escapes so the test cannot
// share a mistyped glyph with the code.
func TestTeamTokenGoldens(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		panes []herdr.PaneInfo
		want  string
	}{
		"3 working, 1 idle, 1 done, nothing ready": {
			panes: []herdr.PaneInfo{p("w1:p1", "w1", "working"), p("w1:p2", "w1", "working"), p("w1:p3", "w1", "working"), p("w1:p4", "w1", "idle"), p("w1:p5", "w1", "done")},
			want:  "5 agents \u00b7 3 working \u00b7 0 ready",
		},
		"2 working, 1 idle, 1 done, 1 blocked, 2 ready": {
			panes: []herdr.PaneInfo{
				p("w1:p1", "w1", "working", "⟳ READY TO RESTART"), p("w1:p2", "w1", "working"), p("w1:p3", "w1", "idle"),
				p("w1:p4", "w1", "done", "⟳ READY TO RESTART"), p("w1:p5", "w1", "blocked"),
			},
			want: "5 agents \u00b7 2 working \u00b7 2 ready \u00b7 1 blocked",
		},
		"one plain shell":         {panes: []herdr.PaneInfo{shell("w1:p1", "w1")}, want: "1 agent \u00b7 0 working \u00b7 0 ready"},
		"a team with no worker":   {want: "0 agents \u00b7 0 working \u00b7 0 ready"},
		"shells counted in N":     {panes: []herdr.PaneInfo{shell("w1:p1", "w1"), shell("w1:p2", "w1"), p("w1:p3", "w1", "working")}, want: "3 agents \u00b7 1 working \u00b7 0 ready"},
		"blocked only":            {panes: []herdr.PaneInfo{p("w1:p1", "w1", "blocked")}, want: "1 agent \u00b7 0 working \u00b7 0 ready \u00b7 1 blocked"},
		"ready only, on idle":     {panes: []herdr.PaneInfo{p("w1:p1", "w1", "idle", "x")}, want: "1 agent \u00b7 0 working \u00b7 1 ready"},
		"idle and done uncounted": {panes: []herdr.PaneInfo{p("w1:p1", "w1", "done"), p("w1:p2", "w1", "done")}, want: "2 agents \u00b7 0 working \u00b7 0 ready"},
		"ready on a shell":        {panes: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "unknown", Tokens: map[string]string{"handoff": "on"}}}, want: "1 agent \u00b7 0 working \u00b7 1 ready"},
		"an empty handoff":        {panes: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle", Tokens: map[string]string{"handoff": ""}}}, want: "1 agent \u00b7 0 working \u00b7 0 ready"},
		"a status herdr adds":     {panes: []herdr.PaneInfo{p("w1:p1", "w1", "napping"), p("w1:p2", "w1", "WORKING")}, want: "2 agents \u00b7 0 working \u00b7 0 ready"},
		"a pane with no status":   {panes: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1"}}, want: "1 agent \u00b7 0 working \u00b7 0 ready"},
		"two-digit counts": {
			panes: func() []herdr.PaneInfo {
				var out []herdr.PaneInfo
				for i := range 12 {
					out = append(out, p("w1:p"+string(rune('a'+i)), "w1", "working", "x"))
				}
				return append(out, p("w1:pz", "w1", "blocked"))
			}(),
			want: "13 agents \u00b7 12 working \u00b7 12 ready \u00b7 1 blocked",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			panes := make([]herdr.PaneInfo, 0, len(tc.panes))
			for _, pane := range tc.panes {
				panes = append(panes, inTeam(pane, "coder-1"))
			}

			got := team.Tokens(team.Build(teamOf(panes), liveTeam, nil))
			require.Len(t, got, 1)
			assert.Equal(t, team.Token{Target: liveTarget, Value: tc.want}, got[0])
		})
	}
}

// TestTokensForEveryGroupTarget: each team's workspace, its every group's
// target, gets the count of its own worker panes, a team with none
// included; the boss's tab, a tab outside the model and a workspace no team
// holds count for nothing; tokens come sorted by target.
func TestTokensForEveryGroupTarget(t *testing.T) {
	t.Parallel()
	l := teamOf([]herdr.PaneInfo{
		inTeam(p("wJ:p1", "", "working"), "boss"), inTeam(p("wN:p1", "", "working"), "coder-1"),
		inTeam(p("wN:p2", "", "blocked"), "coder-2"), inTeam(shell("wN:p3", ""), "coder-2"),
		inTeam(p("wY:p1", "", "done"), "qa-1"), inTeam(shell("w12:p1", ""), "notes"),
		p("w2:p1", "w2", "working"), p("w13:p1", "w13", "working"),
	}, "precheck-1")
	// A second team, assay, with only its boss's tab, and a plain workspace.
	l.Workspaces = append(l.Workspaces, herdr.WorkspaceInfo{WorkspaceID: "w13", Label: "assay"}, herdr.WorkspaceInfo{WorkspaceID: "w2", Label: "notes"})
	l.Tabs = append(l.Tabs, herdr.TabInfo{TabID: "w13:t1", WorkspaceID: "w13", Label: "as-boss"})
	l.Panes[len(l.Panes)-1].TabID = "w13:t1"

	assert.Equal(t, []team.Token{
		{Target: team.Target{WorkspaceID: "w13"}, Value: "0 agents · 0 working · 0 ready"},
		{Target: liveTarget, Value: "4 agents · 1 working · 0 ready · 1 blocked"},
	}, team.Tokens(team.Build(l, team.Resolvers{liveTeam, team.Tabs{Project: "assay", Short: "as"}}, nil)))
	assert.Empty(t, team.Tokens(team.Model{}), "no model, no tokens")
}

// TestTokenContract pins the key and TTL ADR-001's token contract gives
// $team: about 9,000 ms, three times the 3 s poll, so tokens a stopped
// daemon left behind are gone within about 9 s.
func TestTokenContract(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "team", team.Key)
	assert.Equal(t, 9*time.Second, team.TTL)
}

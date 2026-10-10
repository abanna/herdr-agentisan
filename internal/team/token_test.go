package team_test

import (
	"strings"
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
	pane := herdr.PaneInfo{PaneID: id, WorkspaceID: ws, AgentStatus: status}
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
	return herdr.PaneInfo{PaneID: id, WorkspaceID: ws, AgentStatus: "unknown"}
}

// TestTeamTokenGoldens is the frozen `$team` format, byte for byte as v12
// writes it (herdr-dashboard-v12.py:142-143): N · ◐w ●i[ ⚠b][ ⟳r]. N counts
// every pane in the space, shells and unknown included; ◐ counts working; ●
// counts idle plus done; ⚠ counts blocked and ⟳ the panes carrying a handoff
// token, each left out at zero. The wants are spelled with escapes so the
// test cannot share a mistyped glyph with the code: U+00B7 MIDDLE DOT, U+25D0
// ◐, U+25CF ●, U+26A0 ⚠ with no U+FE0F, U+27F3 ⟳. An escape takes exactly
// four hex digits, so "\u25d03" is ◐ then 3.
func TestTeamTokenGoldens(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		panes []herdr.PaneInfo
		want  string
	}{
		"3 working, 1 idle, 1 done": {
			panes: []herdr.PaneInfo{p("w1:p1", "w1", "working"), p("w1:p2", "w1", "working"), p("w1:p3", "w1", "working"), p("w1:p4", "w1", "idle"), p("w1:p5", "w1", "done")},
			want:  "5 \u00b7 \u25d03 \u25cf2",
		},
		"2 working, 1 idle, 1 done, 1 blocked, 2 ready": {
			panes: []herdr.PaneInfo{
				p("w1:p1", "w1", "working", "⟳ READY TO RESTART"), p("w1:p2", "w1", "working"), p("w1:p3", "w1", "idle"),
				p("w1:p4", "w1", "done", "⟳ READY TO RESTART"), p("w1:p5", "w1", "blocked"),
			},
			want: "5 \u00b7 \u25d02 \u25cf2 \u26a01 \u27f32",
		},
		"one plain shell":       {panes: []herdr.PaneInfo{shell("w1:p1", "w1")}, want: "1 \u00b7 \u25d00 \u25cf0"},
		"an empty space":        {want: "0 \u00b7 \u25d00 \u25cf0"},
		"shells counted in N":   {panes: []herdr.PaneInfo{shell("w1:p1", "w1"), shell("w1:p2", "w1"), p("w1:p3", "w1", "working")}, want: "3 \u00b7 \u25d01 \u25cf0"},
		"blocked only":          {panes: []herdr.PaneInfo{p("w1:p1", "w1", "blocked")}, want: "1 \u00b7 \u25d00 \u25cf0 \u26a01"},
		"ready only, on idle":   {panes: []herdr.PaneInfo{p("w1:p1", "w1", "idle", "x")}, want: "1 \u00b7 \u25d00 \u25cf1 \u27f31"},
		"done counts in idle":   {panes: []herdr.PaneInfo{p("w1:p1", "w1", "done"), p("w1:p2", "w1", "done")}, want: "2 \u00b7 \u25d00 \u25cf2"},
		"ready on a shell":      {panes: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "unknown", Tokens: map[string]string{"handoff": "on"}}}, want: "1 \u00b7 \u25d00 \u25cf0 \u27f31"},
		"an empty handoff":      {panes: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle", Tokens: map[string]string{"handoff": ""}}}, want: "1 \u00b7 \u25d00 \u25cf1"},
		"a status herdr adds":   {panes: []herdr.PaneInfo{p("w1:p1", "w1", "napping"), p("w1:p2", "w1", "WORKING")}, want: "2 \u00b7 \u25d00 \u25cf0"},
		"a pane with no status": {panes: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1"}}, want: "1 \u00b7 \u25d00 \u25cf0"},
		"two-digit counts": {
			panes: func() []herdr.PaneInfo {
				var out []herdr.PaneInfo
				for i := range 12 {
					out = append(out, p("w1:p"+string(rune('a'+i)), "w1", "working", "x"))
				}
				return append(out, p("w1:pz", "w1", "blocked"))
			}(),
			want: "13 \u00b7 \u25d012 \u25cf0 \u26a01 \u27f312",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := team.Layout{Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "w1", Label: "◆ coders"}}, Panes: tc.panes}

			got := team.Tokens(team.Build(l, team.Spaces{}, nil))
			require.Len(t, got, 1)
			assert.Equal(t, team.Token{Target: team.Target{WorkspaceID: "w1"}, Value: tc.want}, got[0])
			assert.NotContains(t, got[0].Value, "\ufe0f", "⚠ is the text glyph, never the emoji presentation")
		})
	}
}

// TestTokensForEveryGroupTarget: each ◆ group space gets the count of its own
// panes, an empty one included; the boss and herdr spaces and spaces without
// the prefix get nothing; tokens come sorted by target.
func TestTokensForEveryGroupTarget(t *testing.T) {
	t.Parallel()
	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{
			{WorkspaceID: "wJ", Label: "◆ boss"},
			{WorkspaceID: "wN", Label: "◆ coders"},
			{WorkspaceID: "wY", Label: "◆ qa"},
			{WorkspaceID: "w12", Label: "◆ herdr"},
			{WorkspaceID: "w13", Label: "◆ precheck"},
			{WorkspaceID: "w2", Label: "notes"},
		},
		Panes: []herdr.PaneInfo{
			p("wJ:p1", "wJ", "working"), p("wN:p1", "wN", "working"), p("wN:p2", "wN", "blocked"), shell("wN:p3", "wN"),
			p("wY:p1", "wY", "done"), shell("w12:p1", "w12"), p("w2:p1", "w2", "working"),
		},
	}

	assert.Equal(t, []team.Token{
		{Target: team.Target{WorkspaceID: "w13"}, Value: "0 · ◐0 ●0"},
		{Target: team.Target{WorkspaceID: "wN"}, Value: "3 · ◐1 ●0 ⚠1"},
		{Target: team.Target{WorkspaceID: "wY"}, Value: "1 · ◐0 ●1"},
	}, team.Tokens(team.Build(l, team.Spaces{Project: "agentisan"}, nil)))
	assert.Empty(t, team.Tokens(team.Model{}), "no model, no tokens")
}

// TestTokenContract pins the key and TTL ADR-001's token contract gives
// $team: about 9,000 ms, three times the 3 s poll, so tokens a stopped
// daemon left behind are gone within about 9 s.
func TestTokenContract(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "team", team.Key)
	assert.Equal(t, 9*time.Second, team.TTL)
	assert.True(t, strings.HasPrefix(team.SpacePrefix, "◆ "), "the space prefix is ◆ and a space")
}

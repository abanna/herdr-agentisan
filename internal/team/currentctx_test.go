package team_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// stamped are a pane's tokens as a ctx report leaves them: ctx with its
// ctx_at (A61).
func stamped(ctx string, at time.Time) map[string]string {
	return map[string]string{report.CtxKey: ctx, report.CtxAtKey: report.FormatModelAt(at)}
}

// TestPaneReadsCtxAt: a pane carries its ctx_at only as the report writes
// it, and only while herdr detects an agent in it, as ctx (A27, A61).
func TestPaneReadsCtxAt(t *testing.T) {
	t.Parallel()
	_, got := modelPanes(
		pane("wN:p1", "wN", "codex", "idle", stamped("70", modelAt)),
		pane("wN:p2", "wN", "", "unknown", stamped("70", modelAt)),
		pane("wN:p3", "wN", "codex", "idle", map[string]string{report.CtxKey: "70", report.CtxAtKey: "1761066000.5"}),
		pane("wN:p4", "wN", "codex", "idle", map[string]string{report.CtxKey: "70"}),
	)
	assert.True(t, modelAt.Equal(got["wN:p1"].CtxAt))
	assert.Equal(t, "70", got["wN:p1"].Ctx)
	for id, why := range map[string]string{"wN:p2": "no agent in the pane", "wN:p3": "a ctx_at the report never writes", "wN:p4": "no ctx_at"} {
		assert.True(t, got[id].CtxAt.IsZero(), why)
	}
	assert.Equal(t, "70", got["wN:p4"].Ctx, "a ctx with no ctx_at is still read")
}

// TestCurrentCtx is A61's rule, in A56's shape: a ctx counts while it is not
// older than the pane's agent process; one reported before the agent
// started is a previous agent's, and is none. With no start known, or a ctx
// with no ctx_at, it counts, as before.
func TestCurrentCtx(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	for name, tc := range map[string]struct {
		at, start time.Time
		want      string
	}{
		"reported after the agent started": {at: modelAt, start: start, want: "70"},
		"reported as the agent started":    {at: start, start: start, want: "70"},
		"reported before it started":       {at: start.Add(-time.Millisecond), start: start, want: ""},
		"no start known":                   {at: start.Add(-time.Hour), want: "70"},
		"no ctx_at":                        {start: start, want: "70"},
	} {
		p := team.Pane{Agent: "codex", Ctx: "70", CtxAt: tc.at}
		assert.Equal(t, tc.want, team.CurrentCtx(p, tc.start), name)
	}
}

// TestGateCtx: the poll's gate clears, in the model, the ctx of every boss
// and worker pane an agent before the current one left, by the starts it
// holds by pane, and leaves the rest; the snapshot then reads it as unknown.
func TestGateCtx(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	m, _ := modelPanes(
		pane("wJ:p1", "wJ", "codex", "working", stamped("88", start.Add(-time.Hour))),
		pane("wN:p1", "wN", "codex", "idle", stamped("70", start.Add(-time.Hour))),
		pane("wN:p2", "wN", "claude", "idle", stamped("40", modelAt)),
		pane("wN:p3", "wN", "codex", "idle", stamped("30", start.Add(-time.Hour))),
	)
	team.GateCtx(&m, map[string]time.Time{"wJ:p1": start, "wN:p1": start, "wN:p2": start})

	s := team.Snapshot(m, "", modelAt, nil, nil)
	require.NotNil(t, s.Boss)
	assert.Nil(t, s.Boss.Ctx, "the boss's too")
	ctxs := map[string]*int{}
	for _, a := range s.Groups[0].Agents {
		ctxs[a.PaneID] = a.Ctx
	}
	assert.Nil(t, ctxs["wN:p1"], "a previous agent's 70% is unknown")
	require.NotNil(t, ctxs["wN:p2"])
	assert.Equal(t, 40, *ctxs["wN:p2"])
	require.NotNil(t, ctxs["wN:p3"], "no start known: kept")
	assert.Equal(t, 30, *ctxs["wN:p3"])
}

// TestSidebarTokensShowTheGatedCtx: a team's pane takes its band from the
// model's ctx, as the poll gated it, so a previous agent's ctx shows
// "ctx  --"; a pane in no team keeps its own token, having no start to
// gate it by.
func TestSidebarTokensShowTheGatedCtx(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}, {WorkspaceID: "w2", Label: "notes"}},
		Panes: []herdr.PaneInfo{
			pane("wN:p1", "wN", "codex", "idle", stamped("70", start.Add(-time.Hour))),
			pane("w2:p1", "w2", "codex", "idle", stamped("70", start.Add(-time.Hour))),
		},
	}
	m := team.Build(l, team.Spaces{}, nil)
	team.GateCtx(&m, map[string]time.Time{"wN:p1": start, "w2:p1": start})

	bands := map[string]map[string]string{}
	for _, pt := range team.SidebarTokens(m, l, nil, modelAt) {
		bands[pt.PaneID] = map[string]string{"ok": pt.Tokens[team.CtxOKKey], "hot": pt.Tokens[team.CtxHotKey]}
	}
	assert.Equal(t, map[string]string{"ok": "ctx  --", "hot": ""}, bands["wN:p1"])
	assert.Equal(t, map[string]string{"ok": "", "hot": "ctx 70%"}, bands["w2:p1"])
}

// TestStartClock: the clock holds each team pane's start, the boss's too,
// while the pane shows the terminal the poll saw it on, so a reissued pane id
// gets none; each Observe replaces the last, and a nil clock knows nothing.
func TestStartClock(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	boss := pane("wJ:p1", "wJ", "claude", "working", nil)
	boss.TerminalID = "term_boss"
	p1 := pane("wN:p1", "wN", "codex", "idle", nil)
	p1.TerminalID = "term_a"
	m, _ := modelPanes(boss, p1, pane("wN:p2", "wN", "codex", "idle", nil))

	c := &team.StartClock{}
	c.Observe(m, map[string]time.Time{"wJ:p1": start.Add(-time.Hour), "wN:p1": start, "w9:p9": start})
	assert.True(t, start.Add(-time.Hour).Equal(c.Start("wJ:p1", "term_boss")), "the boss's")
	assert.True(t, start.Equal(c.Start("wN:p1", "term_a")))
	assert.True(t, c.Start("wN:p1", "term_b").IsZero(), "another terminal: a reissued id")
	assert.True(t, c.Start("wN:p2", "").IsZero(), "no start held")
	assert.True(t, c.Start("w9:p9", "").IsZero(), "a pane in no team")

	c.Observe(m, map[string]time.Time{})
	assert.True(t, c.Start("wN:p1", "term_a").IsZero(), "replaced")
	var none *team.StartClock
	none.Observe(m, map[string]time.Time{"wN:p1": start})
	assert.True(t, none.Start("wN:p1", "term_a").IsZero())
}

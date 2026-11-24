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
		inTeam(pane("wN:p1", "", "codex", "idle", stamped("70", modelAt)), "coder-1"),
		inTeam(pane("wN:p2", "", "", "unknown", stamped("70", modelAt)), "coder-1"),
		inTeam(pane("wN:p3", "", "codex", "idle", map[string]string{report.CtxKey: "70", report.CtxAtKey: "1761066000.5"}), "coder-1"),
		inTeam(pane("wN:p4", "", "codex", "idle", map[string]string{report.CtxKey: "70"}), "coder-1"),
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
		after     time.Time
		fresh     bool
		want      string
	}{
		"reported after the agent started": {at: modelAt, start: start, want: "70"},
		"reported as the agent started":    {at: start, start: start, want: "70"},
		"reported before it started":       {at: start.Add(-time.Millisecond), start: start, want: ""},
		"no start known":                   {at: start.Add(-time.Hour), want: "70"},
		"no ctx_at":                        {start: start, want: "70"},
		"lost foreground":                  {at: modelAt, fresh: true},
		"undated after replacement":        {start: start, fresh: true},
		"same stamp after replacement":     {at: modelAt, start: start, after: modelAt, fresh: true},
		"new stamp before new start":       {at: start.Add(-time.Millisecond), start: start, fresh: true},
		"new stamp after replacement":      {at: modelAt, start: start, after: start, fresh: true, want: "70"},
	} {
		p := team.Pane{Agent: "codex", Ctx: "70", CtxAt: tc.at}
		gate := team.CtxGate{Start: tc.start, After: tc.after, RequireFresh: tc.fresh}
		assert.Equal(t, tc.want, gate.Ctx(p), name)
	}
}

// TestGateCtx: the poll's gate clears, in the model, the ctx of every boss
// and worker pane an agent before the current one left, by the starts it
// holds by pane, and leaves the rest; the snapshot then reads it as unknown.
func TestGateCtx(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	m, _ := modelPanes(
		inTeam(pane("wJ:p1", "", "codex", "working", stamped("88", start.Add(-time.Hour))), "boss"),
		inTeam(pane("wN:p1", "", "codex", "idle", stamped("70", start.Add(-time.Hour))), "coder-1"),
		inTeam(pane("wN:p2", "", "claude", "idle", stamped("40", modelAt)), "coder-1"),
		inTeam(pane("wN:p3", "", "codex", "idle", stamped("30", start.Add(-time.Hour))), "coder-1"),
	)
	team.GateCtx(&m, map[string]team.CtxGate{"wJ:p1": {Start: start}, "wN:p1": {Start: start}, "wN:p2": {Start: start}})

	s := team.Snapshot(m, liveProject, modelAt, nil, nil)
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
	l := teamOf([]herdr.PaneInfo{
		inTeam(pane("wN:p1", "", "codex", "idle", stamped("70", start.Add(-time.Hour))), "coder-1"),
		pane("w2:p1", "w2", "codex", "idle", stamped("70", start.Add(-time.Hour))),
	})
	l.Workspaces = append(l.Workspaces, herdr.WorkspaceInfo{WorkspaceID: "w2", Label: "notes"})
	m := team.Build(l, liveTeam, nil)
	team.GateCtx(&m, map[string]team.CtxGate{"wN:p1": {Start: start}, "w2:p1": {Start: start}})

	bands := map[string]map[string]string{}
	for _, pt := range team.SidebarTokens(m, l, nil, modelAt) {
		bands[pt.PaneID] = map[string]string{"ok": pt.Tokens[team.CtxOKKey], "hot": pt.Tokens[team.CtxHotKey]}
	}
	assert.Equal(t, map[string]string{"ok": "ctx  --", "hot": ""}, bands["wN:p1"])
	assert.Equal(t, map[string]string{"ok": "", "hot": "ctx 70%"}, bands["w2:p1"])
}

// TestStartClock covers each team pane's last gate, including the boss.
// A reissued pane ID and a nil clock leave the raw report available.
func TestStartClock(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	boss := inTeam(pane("wJ:p1", "", "claude", "working", nil), "boss")
	boss.TerminalID = "term_boss"
	p1 := inTeam(pane("wN:p1", "", "codex", "idle", nil), "coder-1")
	p1.TerminalID = "term_a"
	m, _ := modelPanes(boss, p1, inTeam(pane("wN:p2", "", "codex", "idle", nil), "coder-1"))

	c := &team.StartClock{}
	gate := team.CtxGate{RequireFresh: true}
	entries := map[string]team.CtxGate{"wJ:p1": {Start: start}, "wN:p1": gate, "w9:p9": gate}
	ctx := func(id, terminal string) team.Pane {
		return team.Pane{ID: id, TerminalID: terminal, Ctx: "70", CtxAt: start.Add(-time.Second)}
	}
	c.Observe(m, entries)
	assert.Empty(t, c.Ctx(ctx("wJ:p1", "term_boss")), "the boss's")
	assert.Empty(t, c.Ctx(ctx("wN:p1", "term_a")), "a missing replacement")
	assert.Equal(t, "70", c.Ctx(ctx("wN:p1", "term_b")), "another terminal: a reissued id")
	assert.Equal(t, "70", c.Ctx(ctx("wN:p2", "")), "no gate held")
	assert.Equal(t, "70", c.Ctx(ctx("w9:p9", "")), "a pane in no team")

	c.Observe(m, nil)
	assert.Equal(t, "70", c.Ctx(ctx("wN:p1", "term_a")), "replaced")
	var none *team.StartClock
	none.Observe(m, entries)
	assert.Equal(t, "70", none.Ctx(ctx("wN:p1", "term_a")))
}

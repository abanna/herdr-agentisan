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

// owned are a pane's tokens as a current build's ctx report leaves them: ctx
// with its ctx_at and the processes that wrote it.
func owned(ctx string, at time.Time, owner ...report.Proc) map[string]string {
	t := stamped(ctx, at)
	t[report.CtxOwnerKey] = report.FormatCtxOwner(at, ctx, owner)
	return t
}

// ownerOf is the ctx_owner of a report of ctx made at at by owner.
func ownerOf(ctx string, at time.Time, owner ...report.Proc) string {
	return report.FormatCtxOwner(at, ctx, owner)
}

// withOwner is tokens with the ctx_owner a report stamped at wrote.
func withOwner(tokens map[string]string, at time.Time, owner ...report.Proc) map[string]string {
	tokens[report.CtxOwnerKey] = report.FormatCtxOwner(at, tokens[report.CtxKey], owner)
	return tokens
}

var (
	agentA = report.Proc{PID: 1002, Start: 1002}
	agentB = report.Proc{PID: 2002, Start: 2002}
)

// TestPaneReadsCtxOwner: a pane carries its ctx_owner only as the report
// writes it, and only while herdr detects an agent in it, as ctx.
func TestPaneReadsCtxOwner(t *testing.T) {
	t.Parallel()
	_, got := modelPanes(
		inTeam(pane("wN:p1", "", "codex", "idle", owned("70", modelAt, agentA, agentB)), "coder-1"),
		inTeam(pane("wN:p2", "", "", "unknown", owned("70", modelAt, agentA)), "coder-1"),
		inTeam(pane("wN:p3", "", "codex", "idle", map[string]string{report.CtxKey: "70", report.CtxOwnerKey: "1002"}), "coder-1"),
		inTeam(pane("wN:p4", "", "codex", "idle", stamped("70", modelAt)), "coder-1"),
		// An older build wrote ctx and ctx_at over the previous owner.
		inTeam(pane("wN:p5", "", "codex", "idle", withOwner(stamped("70", modelAt.Add(time.Minute)), modelAt, agentA)), "coder-1"),
		// The same second, and the same millisecond: an older build wrote B's
		// ctx and ctx_at over A's owner.
		inTeam(pane("wN:p6", "", "codex", "idle", withOwner(stamped("70", modelAt.Add(50*time.Millisecond)), modelAt, agentA)), "coder-1"),
		inTeam(pane("wN:p7", "", "codex", "idle", map[string]string{
			report.CtxKey: "80", report.CtxAtKey: report.FormatModelAt(modelAt), report.CtxOwnerKey: ownerOf("20", modelAt, agentA),
		}), "coder-1"),
	)
	assert.Equal(t, []report.Proc{agentA, agentB}, got["wN:p1"].CtxOwner)
	for id, why := range map[string]string{"wN:p2": "no agent in the pane", "wN:p4": "no ctx_owner"} {
		assert.Empty(t, got[id].CtxOwner, why)
	}
	for id, why := range map[string]string{"wN:p3": "an owner the report never writes", "wN:p5": "an owner written with another ctx_at", "wN:p6": "an owner written 50 ms earlier, in the same second", "wN:p7": "an owner written for another ctx"} {
		assert.Equal(t, []report.Proc{{}}, got[id].CtxOwner, why+": the processes of nobody")
		assert.Empty(t, team.CtxGate{Agent: agentA}.Ctx(got[id]), why)
	}
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

// TestCurrentCtx is A61's rule for a ctx that names no writer, in A56's
// shape: it counts while it is not older than the pane's agent process; one
// reported before the agent started is a previous agent's, and is none. With
// no start known, or a ctx with no ctx_at, it counts, as before.
func TestCurrentCtx(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	for name, tc := range map[string]struct {
		at, start time.Time
		want      string
	}{
		"reported after the agent started": {at: modelAt, start: start, want: "70"},
		"reported as the agent started":    {at: start, start: start, want: "70"},
		"reported before it started":       {at: start.Add(-time.Millisecond), start: start},
		"no start known":                   {at: start.Add(-time.Hour), want: "70"},
		"no ctx_at":                        {start: start, want: "70"},
	} {
		p := team.Pane{Agent: "codex", Ctx: "70", CtxAt: tc.at}
		assert.Equal(t, tc.want, team.CtxGate{Start: tc.start}.Ctx(p), name)
	}
}

// TestCtxGateByOwner: a ctx that names its writers counts only while the
// pane's verified foreground agent is one of them, whatever its stamp says;
// with no verified agent it is unknown.
func TestCtxGateByOwner(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	launcher := report.Proc{PID: 1001, Start: 1001}
	p := team.Pane{Agent: "codex", Ctx: "70", CtxAt: start.Add(-time.Hour), CtxOwner: []report.Proc{agentA, launcher}}
	for name, tc := range map[string]struct {
		gate team.CtxGate
		want string
	}{
		"the writer is in front":        {team.CtxGate{Start: start, Agent: agentA}, "70"},
		"the writer's launcher is":      {team.CtxGate{Start: start, Agent: launcher}, "70"},
		"another agent is in front":     {team.CtxGate{Start: start, Agent: agentB}, ""},
		"the pid was reused":            {team.CtxGate{Start: start, Agent: report.Proc{PID: agentA.PID, Start: agentA.Start + 1}}, ""},
		"no verified agent":             {team.CtxGate{Start: start}, ""},
		"no gate at all":                {team.CtxGate{}, ""},
		"a stamp older than the start":  {team.CtxGate{Start: modelAt, Agent: agentA}, "70"},
		"the stamp is not the evidence": {team.CtxGate{Start: start.Add(2 * time.Hour), Agent: agentA}, "70"},
	} {
		assert.Equal(t, tc.want, tc.gate.Ctx(p), name)
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
		inTeam(pane("wN:p4", "", "codex", "idle", owned("20", start.Add(-time.Hour), agentA)), "coder-1"),
		inTeam(pane("wN:p5", "", "codex", "idle", owned("10", start.Add(-time.Hour), agentA)), "coder-1"),
	)
	team.GateCtx(&m, map[string]team.CtxGate{
		"wJ:p1": {Start: start}, "wN:p1": {Start: start}, "wN:p2": {Start: start},
		"wN:p4": {Start: start, Agent: agentA}, "wN:p5": {Start: start, Agent: agentB},
	})

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
	require.NotNil(t, ctxs["wN:p4"], "its writer is the verified agent")
	assert.Equal(t, 20, *ctxs["wN:p4"])
	assert.Nil(t, ctxs["wN:p5"], "another agent wrote it")
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
	gate := team.CtxGate{Agent: agentB}
	entries := map[string]team.CtxGate{"wJ:p1": {Start: start}, "wN:p1": gate, "w9:p9": gate}
	ctx := func(id, terminal string) team.Pane {
		return team.Pane{ID: id, TerminalID: terminal, Ctx: "70", CtxAt: start.Add(-time.Second), CtxOwner: []report.Proc{agentA}}
	}
	c.Observe(m, entries)
	legacy := func(id, terminal string) team.Pane {
		p := ctx(id, terminal)
		p.CtxOwner = nil
		return p
	}
	assert.Empty(t, c.Ctx(ctx("wJ:p1", "term_boss")), "the boss's: no verified agent")
	assert.Empty(t, c.Ctx(ctx("wN:p1", "term_a")), "another agent is in front")
	assert.Empty(t, c.Ctx(legacy("wJ:p1", "term_boss")), "a ctx older than the boss's agent")
	// Where it holds no gate it has no verified agent: an owned ctx is
	// unknown, one that names no writer is read as before.
	for name, p := range map[string]team.Pane{"another terminal: a reissued id": ctx("wN:p1", "term_b"), "no gate held": ctx("wN:p2", ""), "a pane in no team": ctx("w9:p9", "")} {
		assert.Empty(t, c.Ctx(p), name)
		p.CtxOwner = nil
		assert.Equal(t, "70", c.Ctx(p), name+", ctx of no writer")
	}

	// A reissued id: the gate's agent was verified on the old terminal.
	same := &team.StartClock{}
	same.Observe(m, map[string]team.CtxGate{"wN:p1": {Agent: agentA}})
	assert.Equal(t, "70", same.Ctx(ctx("wN:p1", "term_a")), "its writer is in front of its terminal")
	assert.Empty(t, same.Ctx(ctx("wN:p1", "term_b")), "another terminal's pane")

	c.Observe(m, nil)
	assert.Empty(t, c.Ctx(ctx("wN:p1", "term_a")), "replaced")
	assert.Equal(t, "70", c.Ctx(legacy("wN:p1", "term_a")))
	var none *team.StartClock
	none.Observe(m, entries)
	assert.Empty(t, none.Ctx(ctx("wN:p1", "term_a")))
	assert.Equal(t, "70", none.Ctx(legacy("wN:p1", "term_a")))
}

// Team status reads the gate the poll last held. A pane the poll holds none
// for, as before its first probe after a restart, shows no owned ctx: the
// agent that wrote it is not verified (review of 13d84f7, P1 3).
func TestStartClockHidesAnOwnedCtxItHoldsNoGateFor(t *testing.T) {
	t.Parallel()
	owned := team.Pane{ID: "wN:p1", TerminalID: "term_a", Ctx: "80", CtxOwner: []report.Proc{agentA}}
	legacy := team.Pane{ID: "wN:p1", TerminalID: "term_a", Ctx: "80"}
	m, _ := modelPanes(inTeam(pane("wN:p1", "", "codex", "idle", nil), "coder-1"))
	for name, c := range map[string]*team.StartClock{"no gate held": {}, "a nil clock": nil} {
		assert.Empty(t, c.Ctx(owned), name)
		assert.Equal(t, "80", c.Ctx(legacy), name+": a ctx that names no writer")
	}
	held := &team.StartClock{}
	held.Observe(m, map[string]team.CtxGate{})
	assert.Empty(t, held.Ctx(owned), "a pane the poll holds no gate for")
}

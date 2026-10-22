package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// t0 is the instant the snapshot tests' clock starts at.
var t0 = time.Date(2026, 10, 10, 9, 30, 0, 0, time.UTC)

// layout replaces what th answers pane.list, workspace.list and agent.list
// with.
func (th *teamHerdr) layout(ws []herdr.WorkspaceInfo, panes, agents []map[string]any) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.workspaces, th.panes, th.agents = ws, panes, agents
}

// teamPane is a pane as pane.list answers it: an agent of kind agent (""
// for a plain shell), a label ("" for none) and tokens.
func teamPane(id, ws, agent, status, label string, tokens map[string]string) map[string]any {
	p := map[string]any{"pane_id": id, "workspace_id": ws, "agent_status": status, "focused": false}
	if agent != "" {
		p["agent"] = agent
	}
	if label != "" {
		p["label"] = label
	}
	if tokens != nil {
		p["tokens"] = tokens
	}
	return p
}

// named is an agent.list entry; an empty name is an unnamed agent.
func named(paneID, name string) map[string]any {
	a := map[string]any{"pane_id": paneID, "terminal_id": "t-" + paneID, "agent_status": "idle", "workspace_id": "w", "tab_id": "t", "focused": false, "revision": 1}
	if name != "" {
		a["name"] = name
	}
	return a
}

// fullTeam is a team in several ◆ spaces: the boss, with this dashboard's
// own shell beside it; working, ready, blocked and idle agents, named and
// unnamed, labelled and not; plain shells; a group v12's order does not
// name; an empty group; and the spaces that are not the team's.
func fullTeam() ([]herdr.WorkspaceInfo, []map[string]any, []map[string]any) {
	ws := []herdr.WorkspaceInfo{
		{WorkspaceID: "wJ", Label: "◆ boss"},
		{WorkspaceID: "wR", Label: "◆ research"},
		{WorkspaceID: "wN", Label: "◆ coders"},
		{WorkspaceID: "wQ", Label: "◆ qa"},
		{WorkspaceID: "wZ", Label: "◆ zeta"},
		{WorkspaceID: "wP", Label: "agentisan · precheck"},
		{WorkspaceID: "w12", Label: "◆ herdr"},
		{WorkspaceID: "w2", Label: "notes"},
	}
	panes := []map[string]any{
		teamPane("wJ:p1", "wJ", "claude", "working", "", map[string]string{"ctx": "23", "item": "#867", "stage": "review"}),
		teamPane("wJ:p2", "wJ", "", "unknown", "Agentisan dashboard", nil),
		teamPane("wN:p1", "wN", "claude", "working", "", map[string]string{"ctx": "40", "item": "NERD-5255", "stage": "build_test"}),
		teamPane("wN:p2", "wN", "claude", "done", "", map[string]string{"ctx": "61", "handoff": "⟳ READY TO RESTART"}),
		teamPane("wN:p3", "wN", "claude", "blocked", "", map[string]string{"ctx": "78", "item": "NERD-5247", "stage": "waiting on boss"}),
		teamPane("wN:p4", "wN", "codex", "idle", "scratch", nil),
		teamPane("wN:p5", "wN", "", "unknown", "", nil),
		teamPane("wQ:p1", "wQ", "claude", "idle", "", map[string]string{"ctx": "abc"}),
		teamPane("wZ:p1", "wZ", "", "unknown", "notes", nil),
		teamPane("wP:p1", "wP", "claude", "working", "", map[string]string{"ctx": "100", "stage": "running"}),
		teamPane("w12:p1", "w12", "", "unknown", "", nil),
		teamPane("w2:p1", "w2", "claude", "working", "", nil),
	}
	agents := []map[string]any{
		named("wJ:p1", "boss"), named("wN:p1", "pee01"), named("wN:p2", "pee02"), named("wN:p3", "pee03"),
		named("wN:p4", ""), named("wQ:p1", "qa"), named("wP:p1", ""), named("w2:p1", "elsewhere"),
	}
	return ws, panes, agents
}

//go:fix inline

// wantFullTeam is fullTeam's snapshot at t0, by hand: the groups in v12's
// order, then the rest by name; the rows by pane id; every stage first seen
// at t0, and every agent at rest first seen resting at t0 (A37). The boss
// has no runtime: herdr lists no agent process in its pane (A47).
func wantFullTeam() snapshot.Snapshot {
	return snapshot.Snapshot{
		V: 1, At: t0, Team: "agentisan", Project: &snapshot.Project{Color: "#7c3aed"},
		Boss: &snapshot.Boss{Agent: snapshot.Agent{
			Name: "boss", PaneID: "wJ:p1", Status: "working", Ctx: new(23), Item: "#867", Stage: "review", StageStartedAt: new(t0),
		}},
		Groups: []snapshot.Group{
			{Name: "coders", Agents: []snapshot.Agent{
				{Name: "pee01", PaneID: "wN:p1", Status: "working", Ctx: new(40), Item: "NERD-5255", Stage: "build_test", StageStartedAt: new(t0)},
				{Name: "pee02", PaneID: "wN:p2", Status: "ready", Ctx: new(61), IdleSince: new(t0)},
				{Name: "pee03", PaneID: "wN:p3", Status: "blocked", Ctx: new(78), Item: "NERD-5247", Stage: "waiting on boss", StageStartedAt: new(t0)},
				{Name: "scratch", Unnamed: true, PaneID: "wN:p4", Status: "idle", IdleSince: new(t0)},
				{Name: "wN:p5", Unnamed: true, Shell: true, PaneID: "wN:p5", Status: "unknown"},
			}},
			{Name: "precheck", Agents: []snapshot.Agent{
				{Name: "wP:p1", Unnamed: true, PaneID: "wP:p1", Status: "working", Ctx: new(100), Stage: "running", StageStartedAt: new(t0)},
			}},
			{Name: "qa", Agents: []snapshot.Agent{{Name: "qa", PaneID: "wQ:p1", Status: "idle", IdleSince: new(t0)}}},
			{Name: "research", Agents: []snapshot.Agent{}},
			{Name: "zeta", Agents: []snapshot.Agent{{Name: "notes", Unnamed: true, Shell: true, PaneID: "wZ:p1", Status: "unknown"}}},
		},
	}
}

// projectConfig writes a config dir naming project agentisan, coloured, with
// a repo the daemon cannot (and need not) resolve.
func projectConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "schema_version = 1\n[projects.agentisan]\nrepo = \"~/Development/agentisan\"\ncolor = \"#7c3aed\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(body), 0o600))
	return dir
}

// rawSnapshot asks the daemon socket for the snapshot and returns the
// answer's data, as sent.
func rawSnapshot(t *testing.T, sock string) map[string]any {
	t.Helper()
	return request(t, sock, []byte(`{"v":1,"op":"snapshot"}`+"\n"))
}

// served waits until the daemon at sock serves a snapshot that satisfies
// ok, and returns it. A wait that runs out reports the last answer read.
func served(t *testing.T, sock string, ok func(snapshot.Snapshot) bool) snapshot.Snapshot {
	t.Helper()
	var got snapshot.Snapshot
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		s, err := daemon.Snapshot(t.Context(), sock)
		if !assert.NoError(c, err) {
			return
		}
		got = s
		assert.Truef(c, ok(s), "the daemon never served the snapshot wanted; last: %+v", s)
	}, ready, 5*time.Millisecond)
	return got
}

func anySnapshot(snapshot.Snapshot) bool { return true }

// TestSnapshotOpServesTheTeam is the snapshot op on a herdrtest team: its
// answer's data is one snapshot v1 document that snapshot.Decode accepts as
// sent, with exactly the groups, their order, the rows' names, statuses,
// ctx, item and stage, and the boss the team holds (ADR-001 A18).
func TestSnapshotOpServesTheTeam(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	dir := shortDir(t)
	o := teamOpts(t, dir, th, projectConfig(t))
	o.Now = func() time.Time { return t0 }
	run(t, o)
	sock := socketPath(t, dir, th.path)

	got := served(t, sock, anySnapshot)
	assert.Equal(t, wantFullTeam(), got)

	answer := rawSnapshot(t, sock)
	require.Equal(t, true, answer["ok"])
	data, err := json.Marshal(answer["data"])
	require.NoError(t, err)
	strict, err := snapshot.Decode(bytes.NewReader(data))
	require.NoError(t, err, "the op's data is one strict snapshot v1 document")
	assert.Equal(t, wantFullTeam(), strict)
	assert.NotContains(t, string(data), `"elsewhere"`, "a pane outside the ◆ spaces is not the team's")
	assert.NotContains(t, string(data), `"w12:p1"`, "◆ herdr is not the team's")
	assert.NotContains(t, string(data), `"Agentisan dashboard"`, "a shell in ◆ boss is not the boss")
}

// TestNoCtxWithoutADetectedAgent is A27's rule on a herdrtest team: a pane
// agent.list names but herdr detects no agent in, carrying the ctx a Codex
// left behind, gets no ctx in the snapshot and "ctx  --" in the sidebar,
// while an agent's pane with the same token gets it in both.
func TestNoCtxWithoutADetectedAgent(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, _, _ := fullTeam()
	th.layout(ws, []map[string]any{
		teamPane("wN:p1", "wN", "codex", "idle", "", map[string]string{"ctx": "45"}),
		teamPane("wN:p2", "wN", "", "unknown", "", map[string]string{"ctx": "45"}),
	}, []map[string]any{named("wN:p1", "pee01"), named("wN:p2", "pee02")})
	dir := shortDir(t)
	o := teamOpts(t, dir, th, projectConfig(t))
	o.Now = func() time.Time { return t0 }
	run(t, o)

	got := served(t, socketPath(t, dir, th.path), anySnapshot)
	i := slices.IndexFunc(got.Groups, func(g snapshot.Group) bool { return g.Name == "coders" })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, []snapshot.Agent{
		{Name: "pee01", PaneID: "wN:p1", Status: "idle", Ctx: new(45), IdleSince: new(t0)},
		{Name: "pee02", PaneID: "wN:p2", Status: "unknown"},
	}, got.Groups[i].Agents)

	polled(t, th.srv, 1)
	for pane, want := range map[string]string{"wN:p1": "ctx 45%", "wN:p2": "ctx  --"} {
		params := panePushes(t, th.srv)[pane]
		require.NotEmpty(t, params, pane)
		var push struct {
			Tokens map[string]string `json:"tokens"`
		}
		require.NoError(t, json.Unmarshal([]byte(params[len(params)-1]), &push))
		bands := map[string]string{"ctx_ok": push.Tokens["ctx_ok"], "ctx_warn": push.Tokens["ctx_warn"], "ctx_hot": push.Tokens["ctx_hot"]}
		assert.Equal(t, map[string]string{"ctx_ok": want, "ctx_warn": "", "ctx_hot": ""}, bands, pane)
	}
}

// stagedPane is fullTeam's wN:p1 at status, its stage reported at reported.
func stagedPane(status string, reported time.Time) []map[string]any {
	return []map[string]any{teamPane("wN:p1", "wN", "claude", status, "", map[string]string{
		"item": "NERD-5334", "stage": "qa: reading", "stage_at": strconv.FormatInt(reported.Unix(), 10),
	})}
}

// stagePushed is the stage_now and stage_old of wN:p1's last sidebar push.
func stagePushed(t *testing.T, th *teamHerdr) [2]string {
	t.Helper()
	params := panePushes(t, th.srv)["wN:p1"]
	require.NotEmpty(t, params)
	var push struct {
		Tokens map[string]string `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(params[len(params)-1]), &push))
	return [2]string{push.Tokens[team.StageNowKey], push.Tokens[team.StageOldKey]}
}

// stagedRow is wN:p1's row in s, or the zero Agent.
func stagedRow(s snapshot.Snapshot) snapshot.Agent {
	for _, g := range s.Groups {
		for _, a := range g.Agents {
			if a.PaneID == "wN:p1" {
				return a
			}
		}
	}
	return snapshot.Agent{}
}

// TestRunAgesAStaleStage is A42 on a herdrtest team, over the poll's own
// clock: a stage carried into a turn that reports nothing is current while
// the daemon has seen no turn and while the turn runs; once it ends, the
// sidebar's stage moves from stage_now to stage_old with its age and the
// snapshot marks it stale with its stage_at. Reported again, it is current.
func TestRunAgesAStaleStage(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, _, _ := fullTeam()
	agents := []map[string]any{named("wN:p1", "pee01")}
	reported := t0.Add(-2 * time.Hour)
	th.layout(ws, stagedPane("idle", reported), agents)
	dir := shortDir(t)
	o := teamOpts(t, dir, th, projectConfig(t))
	o.Now = func() time.Time { return t0 }
	run(t, o)
	sock := socketPath(t, dir, th.path)

	polled(t, th.srv, 2)
	assert.Equal(t, [2]string{"qa: reading", ""}, stagePushed(t, th), "no turn seen yet")
	th.layout(ws, stagedPane("working", reported), agents)
	polled(t, th.srv, 2)
	assert.Equal(t, [2]string{"qa: reading", ""}, stagePushed(t, th), "a turn running")
	th.layout(ws, stagedPane("idle", reported), agents)
	polled(t, th.srv, 3)
	assert.Equal(t, [2]string{"", "qa: reading · 2h"}, stagePushed(t, th), "the turn ended with no report")
	got := stagedRow(served(t, sock, func(s snapshot.Snapshot) bool { return stagedRow(s).StageStale }))
	require.NotNil(t, got.StageAt)
	assert.Equal(t, reported, *got.StageAt)

	th.layout(ws, stagedPane("idle", t0), agents)
	polled(t, th.srv, 2)
	assert.Equal(t, [2]string{"qa: reading", ""}, stagePushed(t, th), "reported again")
	got = stagedRow(served(t, sock, func(s snapshot.Snapshot) bool { return !stagedRow(s).StageStale }))
	require.NotNil(t, got.StageAt)
	assert.Equal(t, t0, *got.StageAt)
}

// TestRunGivesTheStopHookAPoll is A42's second poll at rest on the daemon's
// own poll: the first poll that sees a turn end with no report pushes the
// stage current, since the Stop hook may still report, and the second pushes
// it old. Each layout carries a ctx of its own, so every push says which
// layout its poll listed.
func TestRunGivesTheStopHookAPoll(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, _, _ := fullTeam()
	agents := []map[string]any{named("wN:p1", "pee01")}
	reported := t0.Add(-2 * time.Hour)
	pane := func(status, ctx string) []map[string]any {
		p := stagedPane(status, reported)
		p[0]["tokens"].(map[string]string)["ctx"] = ctx
		return p
	}
	th.layout(ws, pane("idle", "40"), agents)
	o := teamOpts(t, shortDir(t), th, projectConfig(t))
	o.Now = func() time.Time { return t0 }
	run(t, o)
	polled(t, th.srv, 2)
	th.layout(ws, pane("working", "41"), agents)
	polled(t, th.srv, 2)
	th.layout(ws, pane("idle", "42"), agents)
	polled(t, th.srv, 3)

	var ended [][2]string
	for _, params := range panePushes(t, th.srv)["wN:p1"] {
		var push struct {
			Tokens map[string]string `json:"tokens"`
		}
		require.NoError(t, json.Unmarshal([]byte(params), &push))
		if push.Tokens["ctx_ok"] == "ctx 42%" {
			ended = append(ended, [2]string{push.Tokens[team.StageNowKey], push.Tokens[team.StageOldKey]})
		}
	}
	require.GreaterOrEqual(t, len(ended), 2, "the polls that saw the turn's end")
	assert.Equal(t, [2]string{"qa: reading", ""}, ended[0], "the first poll at rest: its Stop hook may still report")
	assert.Equal(t, [2]string{"", "qa: reading · 2h"}, ended[1], "the second: the turn ended with no report")
}

// TestRunRestartRaisesNoFalseAlarm: a daemon restarted while a pane is
// mid-turn never saw that turn begin, so a stage reported in it before the
// restart is still current when the turn ends; the next whole turn that
// reports nothing ages it. stage_at, a herdr token, outlives the daemon.
// Every stage_at is at or before the daemon's clock, so none reads as absent.
func TestRunRestartRaisesNoFalseAlarm(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, _, _ := fullTeam()
	agents := []map[string]any{named("wN:p1", "pee01")}
	clk := &clock{now: t0}
	dir := shortDir(t)
	cfg := projectConfig(t)
	current := [2]string{"qa: reading", ""}

	// The first daemon sees the pane rest, then a turn begin, in which the
	// agent reports its stage; it stops before the turn ends.
	o := teamOpts(t, dir, th, cfg)
	o.Now = clk.Now
	th.layout(ws, stagedPane("idle", t0.Add(-2*time.Hour)), agents)
	first := run(t, o)
	polled(t, th.srv, 2)
	th.layout(ws, stagedPane("working", t0.Add(-2*time.Hour)), agents)
	polled(t, th.srv, 2)
	clk.advance(10 * time.Second)
	reported := clk.Now()
	th.layout(ws, stagedPane("working", reported), agents)
	polled(t, th.srv, 2)
	require.Equal(t, current, stagePushed(t, th))
	clk.advance(10 * time.Second)
	first.cancel()
	require.NoError(t, first.wait(t, ready))

	// The restarted daemon first sees the pane at work; the turn ends.
	o = restarted(teamOpts(t, dir, th, cfg))
	o.Now = clk.Now
	run(t, o)
	sock := socketPath(t, dir, th.path)
	polled(t, th.srv, 2)
	assert.Equal(t, current, stagePushed(t, th), "a turn running")
	th.layout(ws, stagedPane("idle", reported), agents)
	polled(t, th.srv, 3)
	assert.Equal(t, current, stagePushed(t, th), "reported in the turn that just ended")
	got := stagedRow(served(t, sock, func(s snapshot.Snapshot) bool { return stagedRow(s).Status == snapshot.StatusIdle }))
	assert.False(t, got.StageStale, "the snapshot agrees")

	// The next whole turn reports nothing: the stage is old.
	clk.advance(10 * time.Second)
	polled(t, th.srv, 2)
	th.layout(ws, stagedPane("working", reported), agents)
	polled(t, th.srv, 2)
	clk.advance(10 * time.Second)
	th.layout(ws, stagedPane("idle", reported), agents)
	polled(t, th.srv, 3)
	assert.Equal(t, [2]string{"", "qa: reading · 0m"}, stagePushed(t, th), "a whole turn with no report")
	got = stagedRow(served(t, sock, func(s snapshot.Snapshot) bool { return stagedRow(s).StageStale }))
	require.NotNil(t, got.StageAt)
	assert.Equal(t, reported, *got.StageAt, "stage_at outlives the daemon")
}

// TestSnapshotOpBeforeTheFirstPoll: until the team poll has built a
// snapshot, the op answers not_ready, which the client maps to ErrNotReady,
// and never waits on herdr. A daemon whose team poll is off is never ready.
func TestSnapshotOpBeforeTheFirstPoll(t *testing.T) {
	t.Parallel()

	t.Run("the first poll is still listing", func(t *testing.T) {
		t.Parallel()
		th := newTeamHerdr(t)
		entered, hold := make(chan struct{}), make(chan struct{})
		var enter, release sync.Once
		th.onCall = func(method string) {
			if method == "workspace.list" {
				enter.Do(func() { close(entered) })
				<-hold
			}
		}
		t.Cleanup(func() { release.Do(func() { close(hold) }) })
		dir := shortDir(t)
		run(t, teamOpts(t, dir, th, ""))
		select {
		case <-entered:
		case <-time.After(ready):
			t.Fatal("the team poll never listed workspaces")
		}
		healthy(t, dir, th.fakeHerdr)
		sock := socketPath(t, dir, th.path)

		_, err := daemon.Snapshot(t.Context(), sock)
		require.ErrorIs(t, err, daemon.ErrNotReady)
		require.ErrorIs(t, err, daemon.ErrRequest)
		answer := rawSnapshot(t, sock)
		assert.Equal(t, false, answer["ok"])
		e, ok := answer["error"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "not_ready", e["code"])
		assert.NotEmpty(t, e["message"])

		release.Do(func() { close(hold) })
		served(t, sock, anySnapshot)
	})
	t.Run("no team poll", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		run(t, opts(t, dir, h))
		healthy(t, dir, h)
		_, err := daemon.Snapshot(t.Context(), socketPath(t, dir, h.path))
		require.ErrorIs(t, err, daemon.ErrNotReady)
	})
}

// TestSnapshotStageTimer: a row's stage_started_at is when the daemon first
// saw the pane's current stage. The same value keeps its start time poll
// after poll; a new value starts at the poll that first sees it.
func TestSnapshotStageTimer(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws := []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}}
	withStage := func(stage string) []map[string]any {
		return []map[string]any{teamPane("wN:p1", "wN", "claude", "working", "", map[string]string{"stage": stage})}
	}
	agents := []map[string]any{named("wN:p1", "pee01")}
	th.layout(ws, withStage("build"), agents)
	c := &clock{now: t0}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, "")
	o.Now = c.Now
	run(t, o)
	sock := socketPath(t, dir, th.path)
	row := func(s snapshot.Snapshot) snapshot.Agent { return s.Groups[0].Agents[0] }

	first := served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t0) })
	require.NotNil(t, row(first).StageStartedAt)
	assert.Equal(t, t0, *row(first).StageStartedAt)

	t1 := t0.Add(3 * time.Second)
	c.advance(3 * time.Second)
	later := served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t1) })
	assert.Equal(t, "build", row(later).Stage)
	require.NotNil(t, row(later).StageStartedAt)
	assert.Equal(t, t0, *row(later).StageStartedAt, "the same value keeps its start")

	t2 := t0.Add(6 * time.Second)
	c.advance(3 * time.Second)
	th.layout(ws, withStage("review"), agents)
	changed := served(t, sock, func(s snapshot.Snapshot) bool { return row(s).Stage == "review" })
	require.NotNil(t, row(changed).StageStartedAt)
	assert.Equal(t, t2, *row(changed).StageStartedAt, "a new value starts at the poll that first sees it")

	t3 := t0.Add(9 * time.Second)
	c.advance(3 * time.Second)
	kept := served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t3) })
	assert.Equal(t, t2, *row(kept).StageStartedAt)
}

// TestSnapshotNamesTheConfiguredProject: the snapshot's team is [spaces]
// project, read once at start, and its project colour that project's; with
// no configuration, or one that does not resolve, the team is the unnamed
// project and no project state is reported.
func TestSnapshotNamesTheConfiguredProject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config      func(t *testing.T) string
		wantTeam    string
		wantProject *snapshot.Project
	}{
		"a configured project and its colour": {config: projectConfig, wantTeam: "agentisan", wantProject: &snapshot.Project{Color: "#7c3aed"}},
		"no configuration":                    {config: func(*testing.T) string { return "" }},
		"no config file":                      {config: func(t *testing.T) string { return t.TempDir() }},
		"a project without a colour": {config: func(t *testing.T) string {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[projects.app]\nrepo = \"/x\"\n"), 0o600))
			return dir
		}, wantTeam: "app"},
		"a file that does not resolve": {config: func(t *testing.T) string {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 2\n"), 0o600))
			return dir
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			dir := shortDir(t)
			run(t, teamOpts(t, dir, th, tc.config(t)))

			got := served(t, socketPath(t, dir, th.path), anySnapshot)
			assert.Equal(t, tc.wantTeam, got.Team)
			assert.Equal(t, tc.wantProject, got.Project)
			assert.NotEmpty(t, got.Groups, "the layout is served either way")
		})
	}
}

// TestSnapshotKeepsTheLastGoodTeam: agent.list failing costs the snapshot
// that poll, never `$team`, which needs no names: the pushes go on every
// poll, the op answers not_ready until a poll has names, and after that the
// last snapshot built with them stays served, its at showing its age.
func TestSnapshotKeepsTheLastGoodTeam(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.agentsErr = true
	c := &clock{now: t0}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, "")
	o.Now = c.Now
	run(t, o)
	sock := socketPath(t, dir, th.path)

	polled(t, th.srv, 2)
	assert.GreaterOrEqual(t, len(pushes(t, th.srv)["wN"]), 2, "$team is pushed without names")
	_, err := daemon.Snapshot(t.Context(), sock)
	require.ErrorIs(t, err, daemon.ErrNotReady)

	th.mu.Lock()
	th.agentsErr = false
	th.mu.Unlock()
	served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t0) })

	th.mu.Lock()
	th.agentsErr = true
	th.mu.Unlock()
	polled(t, th.srv, 1) // a poll in flight may still have had names
	c.advance(time.Minute)
	before := len(pushes(t, th.srv)["wN"])
	polled(t, th.srv, 2)
	assert.Greater(t, len(pushes(t, th.srv)["wN"]), before)
	got, err := daemon.Snapshot(t.Context(), sock)
	require.NoError(t, err)
	assert.True(t, got.At.Equal(t0), "the last good snapshot stays, at %v", got.At)
}

// TestSnapshotTooLargeIsNotServed: a snapshot that would not fit one answer
// line is refused when the poll caches it, and the last one that fit stays.
func TestSnapshotTooLargeIsNotServed(t *testing.T) {
	t.Parallel()

	sized := func(n int) snapshot.Snapshot {
		return snapshot.Snapshot{V: 1, At: t0, Groups: []snapshot.Group{{Name: "g", Agents: []snapshot.Agent{
			{Name: strings.Repeat("x", n), PaneID: "w1:p1", Status: "idle", Unnamed: true},
		}}}}
	}
	base, err := json.Marshal(sized(0))
	require.NoError(t, err)
	atCap := daemon.MaxSnapshot - len(base) // the name length that encodes to exactly the cap
	tests := map[string]struct {
		snap snapshot.Snapshot
		want error
	}{
		"a team":            {snap: sized(10)},
		"exactly the cap":   {snap: sized(atCap)},
		"one byte past it":  {snap: sized(atCap + 1), want: daemon.ErrSnapshotTooLarge},
		"a megabyte":        {snap: sized(1 << 20), want: daemon.ErrSnapshotTooLarge},
		"an empty snapshot": {snap: snapshot.Snapshot{V: 1, At: t0, Groups: []snapshot.Group{}}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := daemon.CacheSnapshot(tc.snap)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Nil(t, raw, "nothing is cached")
				return
			}
			require.NoError(t, err)
			got, err := snapshot.Decode(bytes.NewReader(raw))
			require.NoError(t, err)
			assert.Equal(t, tc.snap.Groups, got.Groups)
		})
	}
}

// serveOnce answers every connection to a fresh socket with reply, after
// reading the request line.
func serveOnce(t *testing.T, reply string) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			_, _ = conn.Write([]byte(reply))
			_ = conn.Close()
		}
	}()
	return sock
}

// silentDaemon accepts connections and never answers, like a wedged daemon.
func silentDaemon(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "w.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				<-t.Context().Done()
				_ = conn.Close()
			}()
		}
	}()
	return sock
}

// TestSnapshotSourceClasses is the dashboard's daemon source: every way the
// daemon socket can answer, or not, is a snapshot or one line the footer
// shows, and every failure is a sentinel the caller can branch on.
func TestSnapshotSourceClasses(t *testing.T) {
	t.Parallel()

	good := `{"v":1,"at":"2026-10-10T09:30:00Z","team":"agentisan","groups":[{"name":"coders","agents":[{"name":"pee01","pane_id":"w2:p1","status":"working","model":""}]}]}`
	tests := map[string]struct {
		sock     func(t *testing.T) string
		want     []error
		contains string
	}{
		"ok": {sock: func(t *testing.T) string { return serveOnce(t, `{"ok":true,"data":`+good+"}\n") }},
		"daemon down: no socket": {
			sock: func(t *testing.T) string { return filepath.Join(shortDir(t), "none.sock") },
			want: []error{daemon.ErrUnavailable}, contains: "daemon not running: ",
		},
		"daemon down: a stale file, not a socket": {
			sock: func(t *testing.T) string {
				p := filepath.Join(shortDir(t), "f")
				require.NoError(t, os.WriteFile(p, nil, 0o600))
				return p
			},
			want: []error{daemon.ErrUnavailable}, contains: "daemon not running: ",
		},
		"closed without an answer": {sock: func(t *testing.T) string { return serveOnce(t, "") }, want: []error{daemon.ErrUnavailable}, contains: "daemon not running: "},
		"garbage":                  {sock: func(t *testing.T) string { return serveOnce(t, "nope\n") }, want: []error{daemon.ErrProtocol}},
		"ok without data":          {sock: func(t *testing.T) string { return serveOnce(t, `{"ok":true}`+"\n") }, want: []error{daemon.ErrProtocol}},
		"data that is no snapshot": {sock: func(t *testing.T) string { return serveOnce(t, `{"ok":true,"data":[1,2]}`+"\n") }, want: []error{daemon.ErrProtocol, snapshot.ErrInvalid}},
		"a snapshot that breaks the contract": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":true,"data":{"v":1,"team":"t","groups":[{"name":"","agents":[]}]}}`+"\n")
			},
			want: []error{daemon.ErrProtocol, snapshot.ErrInvalid},
		},
		"a key the contract lacks": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":true,"data":{"v":1,"team":"t","groups":[],"extra":1}}`+"\n")
			},
			want: []error{daemon.ErrProtocol, snapshot.ErrInvalid},
		},
		"a newer contract": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":true,"data":{"v":2,"team":"t","groups":[]}}`+"\n")
			},
			want: []error{daemon.ErrProtocol, snapshot.ErrUnsupportedVersion},
		},
		"not ready": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":false,"error":{"code":"not_ready","message":"no snapshot yet"}}`+"\n")
			},
			want: []error{daemon.ErrNotReady, daemon.ErrRequest}, contains: "daemon starting",
		},
		"no such team": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":false,"error":{"code":"no_team","message":"workspace w2 is no team's"}}`+"\n")
			},
			want: []error{daemon.ErrNoTeam, daemon.ErrRequest}, contains: "workspace w2 is no team's",
		},
		// A daemon from before the op answers unknown_op.
		"an older daemon": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":false,"error":{"code":"unknown_op","message":"unknown op \"snapshot\""}}`+"\n")
			},
			want: []error{daemon.ErrRequest}, contains: "unknown_op",
		},
		"an answer past the size cap": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":true,"data":"`+strings.Repeat("x", 1<<20)+`"}`+"\n")
			},
			want: []error{daemon.ErrUnavailable},
		},
		// The dial follows a symlink as the kernel does; nothing here
		// checks where it leads, since the path is the daemon's own.
		"a symlink to the socket": {
			sock: func(t *testing.T) string {
				sock := serveOnce(t, `{"ok":true,"data":`+good+"}\n")
				link := filepath.Join(shortDir(t), "l.sock")
				require.NoError(t, os.Symlink(sock, link))
				return link
			},
		},
		"a dangling symlink": {
			sock: func(t *testing.T) string {
				link := filepath.Join(shortDir(t), "l.sock")
				require.NoError(t, os.Symlink(filepath.Join(shortDir(t), "gone.sock"), link))
				return link
			},
			want: []error{daemon.ErrUnavailable}, contains: "daemon not running: ",
		},
		"a FIFO at the socket path": {
			sock: func(t *testing.T) string {
				p := filepath.Join(shortDir(t), "f.sock")
				require.NoError(t, syscall.Mkfifo(p, 0o600))
				return p
			},
			want: []error{daemon.ErrUnavailable}, contains: "daemon not running: ",
		},
		"a CRLF-framed answer": {sock: func(t *testing.T) string { return serveOnce(t, `{"ok":true,"data":`+good+"}\r\n") }},
		"invalid UTF-8 inside the snapshot": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":true,"data":`+strings.Replace(good, `"model":""`, "\"model\":\"op\xffus\"", 1)+"}\n")
			},
		},
		"a byte-order mark before the answer": {
			sock: func(t *testing.T) string { return serveOnce(t, "\xef\xbb\xbf"+`{"ok":true,"data":`+good+"}\n") },
			want: []error{daemon.ErrProtocol},
		},
		"a raw NUL in the answer": {
			sock: func(t *testing.T) string {
				return serveOnce(t, `{"ok":true,"data":`+strings.Replace(good, "pee01", "pee\x0001", 1)+"}\n")
			},
			want: []error{daemon.ErrProtocol},
		},
		"a directory at the socket path": {
			sock: func(t *testing.T) string {
				p := filepath.Join(shortDir(t), "d.sock")
				require.NoError(t, os.Mkdir(p, 0o700))
				return p
			},
			want: []error{daemon.ErrUnavailable}, contains: "daemon not running: ",
		},
		"a socket the user may not open": {
			sock: func(t *testing.T) string {
				sock := serveOnce(t, `{"ok":true,"data":`+good+"}\n")
				require.NoError(t, os.Chmod(sock, 0))
				return sock
			},
			want: []error{daemon.ErrUnavailable, os.ErrPermission}, contains: "daemon not running: ",
		},
		"a socket under a directory the user may not search": {
			sock: func(t *testing.T) string {
				sock := serveOnce(t, `{"ok":true,"data":`+good+"}\n")
				dir := filepath.Dir(sock)
				require.NoError(t, os.Chmod(dir, 0o600))
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				return sock
			},
			want: []error{daemon.ErrUnavailable, os.ErrPermission}, contains: "daemon not running: ",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if slices.Contains(tc.want, os.ErrPermission) && os.Geteuid() == 0 {
				t.Skip("root opens any socket")
			}
			got, err := daemon.SnapshotSource{Socket: tc.sock(t)}.Snapshot(t.Context())
			if tc.want == nil {
				require.NoError(t, err)
				assert.Equal(t, "agentisan", got.Team)
				assert.Equal(t, "pee01", got.Groups[0].Agents[0].Name)
				return
			}
			for _, want := range tc.want {
				require.ErrorIs(t, err, want)
			}
			assert.NotContains(t, err.Error(), "\n", "the footer shows one line")
			if tc.contains != "" {
				assert.Contains(t, err.Error(), tc.contains)
			}
			assert.True(t, strings.HasPrefix(err.Error(), "daemon "), "the line says what failed: %q", err)
		})
	}
}

// TestSnapshotSourceHonoursTheContext: a wedged daemon never holds a poll
// past the caller's deadline, nor past a cancel with no deadline, which the
// dashboard sends when it quits.
func TestSnapshotSourceHonoursTheContext(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		ctx  func(t *testing.T) context.Context
		want error
		line string
	}{
		"a deadline": {
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			want: context.DeadlineExceeded, line: "daemon not answering",
		},
		"a cancel without a deadline": {
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				time.AfterFunc(100*time.Millisecond, cancel)
				return ctx
			},
			want: context.Canceled,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sock := silentDaemon(t)
			start := time.Now()
			_, err := daemon.SnapshotSource{Socket: sock}.Snapshot(tc.ctx(t))
			require.ErrorIs(t, err, tc.want)
			assert.Less(t, time.Since(start), 2*time.Second, "the call ends with its context, not at its own 5 s bound")
			if tc.line != "" {
				assert.Contains(t, err.Error(), tc.line)
			}
		})
	}
	t.Run("an already cancelled context dials nothing", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := daemon.SnapshotSource{Socket: silentDaemon(t)}.Snapshot(ctx)
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	})
}

// TestSnapshotOpServesConcurrentReaders: any number of dashboards may ask at
// once, while the poll replaces the snapshot under them; each gets one whole
// snapshot, never a torn one.
func TestSnapshotOpServesConcurrentReaders(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	dir := shortDir(t)
	run(t, teamOpts(t, dir, th, projectConfig(t)))
	sock := socketPath(t, dir, th.path)
	served(t, sock, anySnapshot)

	var wg sync.WaitGroup
	errs := make(chan error, 8*5)
	for range 8 {
		wg.Go(func() {
			for range 5 {
				s, err := daemon.Snapshot(t.Context(), sock)
				if err == nil && (s.Team != "agentisan" || len(s.Groups) != 5) {
					err = fmt.Errorf("a torn snapshot: %+v", s)
				}
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// everyTeamConfig is the ◆ spaces' project, coloured, and two tabs teams,
// one coloured and one not.
const everyTeamConfig = `[spaces]
project = "agentisan"
[projects.agentisan]
repo = "/x"
color = "#7c3aed"
[projects.alpha]
repo = "/y"
short = "al"
color = "#16a34a"
[projects.beta]
repo = "/z"
short = "be"
`

// tabPane is a pane in a tab, as pane.list answers it.
func tabPane(id, ws, tab, agent, status string, tokens map[string]string) map[string]any {
	p := teamPane(id, ws, agent, status, "", tokens)
	p["tab_id"] = tab
	return p
}

// tabOf is a tab as tab.list answers it.
func tabOf(id, ws, label string) map[string]any {
	return map[string]any{"tab_id": id, "workspace_id": ws, "number": 1, "label": label, "focused": false, "pane_count": 1, "agent_status": "idle"}
}

// everyTeam serves fullTeam's ◆ spaces beside two tabs teams: alpha in w14,
// with its boss, a reviewer, a coder and a tab outside the model; beta in
// w20, with a librarian and a researcher and no boss. Each team's types are
// listed out of their working order, and out of name order too.
func everyTeam(th *teamHerdr) {
	ws, panes, agents := fullTeam()
	ws = append(ws, herdr.WorkspaceInfo{WorkspaceID: "w14", Label: "alpha"}, herdr.WorkspaceInfo{WorkspaceID: "w20", Label: "beta"})
	panes = append(panes,
		tabPane("w14:p1", "w14", "w14:t1", "claude", "working", nil),
		tabPane("w14:p2", "w14", "w14:t2", "claude", "idle", map[string]string{"ctx": "12"}),
		tabPane("w14:p3", "w14", "w14:t3", "claude", "working", map[string]string{"ctx": "50", "item": "NERD-5285", "stage": "build_test"}),
		tabPane("w14:p4", "w14", "w14:t4", "", "unknown", nil),
		tabPane("w20:p1", "w20", "w20:t1", "claude", "blocked", nil),
		tabPane("w20:p2", "w20", "w20:t2", "codex", "working", nil),
	)
	agents = append(agents, named("w14:p1", "al-boss"), named("w14:p2", "al-review-1"), named("w14:p3", "al-coder-1"), named("w20:p2", "be-research-1"))
	th.mu.Lock()
	defer th.mu.Unlock()
	th.workspaces, th.panes, th.agents = ws, panes, agents
	th.tabs = []map[string]any{
		tabOf("w14:t1", "w14", "al-boss"), tabOf("w14:t2", "w14", "al-review-1"),
		tabOf("w14:t3", "w14", "al-coder-1"), tabOf("w14:t4", "w14", "notes"),
		tabOf("w20:t1", "w20", "be-librarian-1"), tabOf("w20:t2", "w20", "be-research-1"),
	}
}

// wantTeams is everyTeam's snapshot of each team at t0, by hand: each team in
// its own resolver's group order and colour, and no boss with a runtime,
// since herdr lists no agent process in any pane (A47).
func wantTeams() map[string]snapshot.Snapshot {
	return map[string]snapshot.Snapshot{
		"agentisan": wantFullTeam(),
		"alpha": {
			V: 1, At: t0, Team: "alpha", Project: &snapshot.Project{Color: "#16a34a"},
			Boss: &snapshot.Boss{Agent: snapshot.Agent{Name: "al-boss", PaneID: "w14:p1", TabID: "w14:t1", Status: "working"}},
			Groups: []snapshot.Group{
				{Name: "coder", Agents: []snapshot.Agent{
					{Name: "al-coder-1", PaneID: "w14:p3", TabID: "w14:t3", Status: "working", Ctx: new(50), Item: "NERD-5285", Stage: "build_test", StageStartedAt: new(t0)},
				}},
				{Name: "review", Agents: []snapshot.Agent{{Name: "al-review-1", PaneID: "w14:p2", TabID: "w14:t2", Status: "idle", Ctx: new(12), IdleSince: new(t0)}}},
			},
		},
		"beta": {
			V: 1, At: t0, Team: "beta",
			Groups: []snapshot.Group{
				{Name: "research", Agents: []snapshot.Agent{{Name: "be-research-1", PaneID: "w20:p2", TabID: "w20:t2", Status: "working"}}},
				{Name: "librarian", Agents: []snapshot.Agent{{Name: "w20:p1", Unnamed: true, PaneID: "w20:p1", TabID: "w20:t1", Status: "blocked"}}},
			},
		},
	}
}

// everyTeamDaemon runs a daemon on everyTeam at t0 and waits until it serves
// all three teams; it returns the daemon's socket.
func everyTeamDaemon(t *testing.T) (*teamHerdr, string) {
	t.Helper()
	th := newTeamHerdr(t)
	everyTeam(th)
	dir := shortDir(t)
	o := teamOpts(t, dir, th, configDir(t, everyTeamConfig))
	o.Now = func() time.Time { return t0 }
	run(t, o)
	sock := socketPath(t, dir, th.path)
	served(t, sock, anySnapshot)
	require.Eventually(t, func() bool {
		_, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: "beta"})
		return err == nil
	}, ready, 5*time.Millisecond, "the daemon never served the tabs teams")
	return th, sock
}

// snapshotOp sends the snapshot op with args, a JSON value or "" for none,
// and returns the answer's data exactly as sent, or its refusal code.
func snapshotOp(t *testing.T, sock, args string) (json.RawMessage, string) {
	t.Helper()
	line := `{"v":1,"op":"snapshot"}`
	if args != "" {
		line = `{"v":1,"op":"snapshot","args":` + args + `}`
	}
	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer conn.Close() //nolint:errcheck // test connection
	_, err = conn.Write([]byte(line + "\n"))
	require.NoError(t, err)
	raw, err := bufio.NewReader(conn).ReadBytes('\n')
	require.NoError(t, err)
	var resp struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &resp))
	if !resp.OK {
		require.NotNil(t, resp.Error)
		return nil, resp.Error.Code
	}
	return resp.Data, ""
}

// TestSnapshotOpServesEveryTeam is A24's daemon: one poll builds a snapshot
// of every team, the ◆ spaces' and each tabs team's, and the op serves each
// byte for byte as the snapshot v1 encoding of that team, in its own group
// order and colour, with only its own panes.
func TestSnapshotOpServesEveryTeam(t *testing.T) {
	t.Parallel()
	_, sock := everyTeamDaemon(t)

	for project, want := range wantTeams() {
		data, code := snapshotOp(t, sock, fmt.Sprintf(`{"project":%q}`, project))
		require.Empty(t, code, project)
		wantRaw, err := json.Marshal(want)
		require.NoError(t, err)
		assert.Equal(t, string(wantRaw), string(data), "team %s, byte for byte", project)
		got, err := snapshot.Decode(bytes.NewReader(data))
		require.NoError(t, err, "a strict snapshot v1 document")
		assert.Equal(t, want, got)
	}
}

// TestSnapshotOpSelectsTheTeam walks the op's args (A24): a project, which
// wins over a workspace; a workspace, through the index of every team's
// workspaces, ◆ boss and ◆ herdr included; neither, which is the ◆ spaces'
// project as before; and what selects no team, which is no_team, matched by
// ErrNoTeam, never another team's snapshot.
func TestSnapshotOpSelectsTheTeam(t *testing.T) {
	t.Parallel()
	_, sock := everyTeamDaemon(t)

	tests := map[string]struct {
		args, team, code string
	}{
		"a tabs team by project":            {args: `{"project":"alpha"}`, team: "alpha"},
		"the spaces by project":             {args: `{"project":"agentisan"}`, team: "agentisan"},
		"a tabs team's workspace":           {args: `{"workspace_id":"w20"}`, team: "beta"},
		"a group space":                     {args: `{"workspace_id":"wN"}`, team: "agentisan"},
		"◆ boss":                            {args: `{"workspace_id":"wJ"}`, team: "agentisan"},
		"◆ herdr, the edit shell":           {args: `{"workspace_id":"w12"}`, team: "agentisan"},
		"a space no group's but the team's": {args: `{"workspace_id":"wZ"}`, team: "agentisan"},
		"both: the project wins":            {args: `{"project":"beta","workspace_id":"w14"}`, team: "beta"},
		"neither":                           {team: "agentisan"},
		"empty args":                        {args: `{}`, team: "agentisan"},
		"null args":                         {args: `null`, team: "agentisan"},
		"empty strings select nothing":      {args: `{"project":"","workspace_id":""}`, team: "agentisan"},
		"args it does not know":             {args: `{"team":"alpha"}`, team: "agentisan"},
		"a workspace no team's":             {args: `{"workspace_id":"w2"}`, code: "no_team"},
		"a workspace herdr does not show":   {args: `{"workspace_id":"w99"}`, code: "no_team"},
		"an unknown project":                {args: `{"project":"gamma"}`, code: "no_team"},
		"an unknown project wins too":       {args: `{"project":"gamma","workspace_id":"w14"}`, code: "no_team"},
		"a project in another case":         {args: `{"project":"Alpha"}`, code: "no_team"},
		"a workspace that is a label":       {args: `{"workspace_id":"alpha"}`, code: "no_team"},
		"a project that is not a string":    {args: `{"project":["alpha"]}`, code: "bad_request"},
		// Names are compared byte for byte: nothing is trimmed, folded or
		// normalized, so a near miss is no team, never another one.
		"a project with a space":        {args: `{"project":" alpha"}`, code: "no_team"},
		"a project spelled with ä":      {args: `{"project":"älpha"}`, code: "no_team"},
		"a workspace with a NUL":        {args: `{"workspace_id":"w14\u0000"}`, code: "no_team"},
		"a workspace with a newline":    {args: `{"workspace_id":"w14\n"}`, code: "no_team"},
		"a lone surrogate in a project": {args: `{"project":"\ud800"}`, code: "no_team"},
		"invalid UTF-8 in a workspace":  {args: "{\"workspace_id\":\"w1\xff4\"}", code: "no_team"},
		"args nested past their shape":  {args: `{"workspace_id":{"id":"w14"}}`, code: "bad_request"},
		"a project that is a number":    {args: `{"project":1e400}`, code: "bad_request"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, code := snapshotOp(t, sock, tc.args)
			assert.Equal(t, tc.code, code)
			if tc.code != "" {
				return
			}
			got, err := snapshot.Decode(bytes.NewReader(data))
			require.NoError(t, err)
			assert.Equal(t, tc.team, got.Team)
		})
	}

	t.Run("the client", func(t *testing.T) {
		t.Parallel()
		got, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{WorkspaceID: "w14"})
		require.NoError(t, err)
		assert.Equal(t, wantTeams()["alpha"], got)
		_, err = daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: "gamma"})
		require.ErrorIs(t, err, daemon.ErrNoTeam)
		require.ErrorIs(t, err, daemon.ErrRequest)
		assert.NotErrorIs(t, err, daemon.ErrNotReady)
		got, err = daemon.SnapshotSource{Socket: sock, Selector: daemon.Selector{Project: "beta"}}.Snapshot(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "beta", got.Team)
	})
}

// TestSnapshotOpBeforeAnyPollSelectsNothing: before the first poll with
// names, every selector is not_ready, a team the daemon cannot know yet
// included, never no_team.
func TestSnapshotOpBeforeAnyPollSelectsNothing(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.agentsErr = true
	dir := shortDir(t)
	run(t, teamOpts(t, dir, th, configDir(t, everyTeamConfig)))
	polled(t, th.srv, 1)
	sock := socketPath(t, dir, th.path)
	for _, sel := range []daemon.Selector{{}, {Project: "alpha"}, {Project: "gamma"}, {WorkspaceID: "wN"}, {WorkspaceID: "w99"}} {
		_, err := daemon.SnapshotOf(t.Context(), sock, sel)
		require.ErrorIs(t, err, daemon.ErrNotReady, "%+v", sel)
		assert.NotErrorIs(t, err, daemon.ErrNoTeam)
	}
}

// TestSnapshotOpDropsAGoneTeam: a team gone from the layout answers no_team,
// by project and by its old workspace alike, from the next poll on; the
// others are served as before. A18's "the last one stays" is a failed
// agent.list's alone.
func TestSnapshotOpDropsAGoneTeam(t *testing.T) {
	t.Parallel()
	th, sock := everyTeamDaemon(t)

	th.relabel("w14", "alpha (old)")
	require.Eventually(t, func() bool {
		_, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: "alpha"})
		return errors.Is(err, daemon.ErrNoTeam)
	}, ready, 5*time.Millisecond, "alpha is still served")
	_, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{WorkspaceID: "w14"})
	require.ErrorIs(t, err, daemon.ErrNoTeam)
	for _, project := range []string{"agentisan", "beta"} {
		got, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: project})
		require.NoError(t, err)
		assert.Equal(t, wantTeams()[project], got)
	}

	th.relabel("w14", "alpha")
	got := served(t, sock, func(snapshot.Snapshot) bool {
		_, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: "alpha"})
		return err == nil
	})
	assert.Equal(t, "agentisan", got.Team, "a team back in the layout is served again")
}

// workerNamed is a model team, project, in workspace ws, with one worker
// whose name is name.
func workerNamed(project, ws, name string) team.Project {
	return team.Project{Name: project, Workspaces: []string{ws}, Groups: []team.Group{{Name: "g", Workers: []team.Worker{{
		ID: ws + ":p1", Pane: team.Pane{ID: ws + ":p1", Name: name, Agent: "claude", Status: team.StatusIdle},
	}}}}}
}

// TestSnapshotCacheKeepsATeamTooLarge: a team whose snapshot would not fit
// one answer line keeps the last one that did, while every other team is
// replaced as usual; one that has never fit is not_ready, not no_team, and
// costs no other team its snapshot.
func TestSnapshotCacheKeepsATeamTooLarge(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("x", daemon.MaxSnapshot)
	load := func(t *testing.T, c *daemon.TeamCache, sel daemon.Selector) snapshot.Snapshot {
		t.Helper()
		raw, err := c.Load(sel)
		require.NoError(t, err)
		s, err := snapshot.Decode(bytes.NewReader(raw))
		require.NoError(t, err)
		return s
	}
	name := func(s snapshot.Snapshot) string { return s.Groups[0].Agents[0].Name }

	var c daemon.TeamCache
	t1 := t0.Add(3 * time.Second)
	require.NoError(t, c.Store(team.Model{Projects: []team.Project{workerNamed("a", "wa", "a1"), workerNamed("b", "wb", "b1")}}, "a", t0))
	before, err := c.Load(daemon.Selector{Project: "a"})
	require.NoError(t, err)

	err = c.Store(team.Model{Projects: []team.Project{workerNamed("a", "wa", huge), workerNamed("b", "wb", "b2")}}, "a", t1)
	require.ErrorIs(t, err, daemon.ErrSnapshotTooLarge)
	assert.Contains(t, err.Error(), `team "a"`)
	after, err := c.Load(daemon.Selector{Project: "a"})
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a keeps its last snapshot, byte for byte")
	assert.Equal(t, "a1", name(load(t, &c, daemon.Selector{WorkspaceID: "wa"})))
	b := load(t, &c, daemon.Selector{WorkspaceID: "wb"})
	assert.Equal(t, "b2", name(b), "b is replaced as usual")
	assert.True(t, b.At.Equal(t1))

	var fresh daemon.TeamCache
	err = fresh.Store(team.Model{Projects: []team.Project{workerNamed("a", "wa", huge), workerNamed("b", "wb", "b1")}}, "a", t0)
	require.ErrorIs(t, err, daemon.ErrSnapshotTooLarge)
	_, err = fresh.Load(daemon.Selector{})
	require.ErrorIs(t, err, daemon.ErrNotReady, "a team that has never fit")
	assert.NotErrorIs(t, err, daemon.ErrNoTeam)
	_, err = fresh.Load(daemon.Selector{WorkspaceID: "wa"})
	require.ErrorIs(t, err, daemon.ErrNotReady)
	assert.Equal(t, "b1", name(load(t, &fresh, daemon.Selector{Project: "b"})))

	require.NoError(t, fresh.Store(team.Model{Projects: []team.Project{workerNamed("b", "wb", "b3")}}, "a", t1))
	_, err = fresh.Load(daemon.Selector{Project: "a"})
	require.ErrorIs(t, err, daemon.ErrNoTeam, "gone from the model, a team too large to serve is gone too")
	_, err = fresh.Load(daemon.Selector{})
	require.ErrorIs(t, err, daemon.ErrNoTeam, "the spaces' project, with no team in the model")
}

// TestSnapshotCacheNeverPairsTwoStores is the one swap (A24) in process, with
// no socket in the way: stores alternate which team holds w1 and w2 while
// readers ask by workspace, and every answer must hold the asked workspace's
// pane. An index from one store with snapshots from another would serve the
// team that held the workspace in the other store, without it. Every access
// is locked, so the race detector cannot see such a split; only reading at
// in-process speed between stores can.
func TestSnapshotCacheNeverPairsTwoStores(t *testing.T) {
	t.Parallel()
	layout := func(first, second string) team.Model {
		return team.Model{Projects: []team.Project{workerNamed(first, "w1", first+"1"), workerNamed(second, "w2", second+"1")}}
	}
	models := []team.Model{layout("alpha", "beta"), layout("beta", "alpha")}
	var c daemon.TeamCache
	require.NoError(t, c.Store(models[0], "alpha", t0))

	var stop atomic.Bool
	var stores atomic.Int64
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 1; !stop.Load(); i++ {
			_ = c.Store(models[i%2], "alpha", t0)
			stores.Add(1)
		}
	})
	var readers sync.WaitGroup
	var reads, bad atomic.Int64
	var first atomic.Value
	deadline := time.Now().Add(2 * time.Second)
	for i := range 8 {
		ws := []string{"w1", "w2"}[i%2]
		pane := []byte(`"pane_id":"` + ws + `:p1"`)
		readers.Go(func() {
			for time.Now().Before(deadline) && stores.Load() < 2000 && bad.Load() == 0 {
				raw, err := c.Load(daemon.Selector{WorkspaceID: ws})
				reads.Add(1)
				if err != nil || !bytes.Contains(raw, pane) {
					bad.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("%s (%v)", raw, err))
				}
			}
		})
	}
	readers.Wait()
	stop.Store(true)
	writer.Wait()
	t.Logf("%d reads over %d stores", reads.Load(), stores.Load())
	assert.Zero(t, bad.Load(), "an answer without its workspace's pane: %v", first.Load())
	assert.Greater(t, stores.Load(), int64(100), "the stores ran under the readers")
}

// TestSnapshotOpNeverPairsTwoPolls is the one swap (A24) end to end: every
// poll moves two teams between two workspaces and names each pane after the
// poll, while dashboards ask by workspace over the socket. Each answer must be
// the team its workspace belonged to in the very poll that built the
// snapshot, holding that workspace's pane. A split swap leaves a window of
// about a microsecond, which socket round trips rarely hit:
// TestSnapshotCacheNeverPairsTwoStores is the test that catches one.
func TestSnapshotOpNeverPairsTwoPolls(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	var gen int
	flip := func() {
		th.mu.Lock()
		defer th.mu.Unlock()
		gen++
		first, second := "alpha", "beta"
		if gen%2 == 1 {
			first, second = second, first
		}
		short := map[string]string{"alpha": "al", "beta": "be"}
		th.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "w1", Label: first}, {WorkspaceID: "w2", Label: second}}
		th.tabs = []map[string]any{tabOf("w1:t1", "w1", short[first]+"-coder-1"), tabOf("w2:t1", "w2", short[second]+"-coder-1")}
		th.agents = []map[string]any{named("w1:p1", fmt.Sprintf("g%d-w1", gen)), named("w2:p1", fmt.Sprintf("g%d-w2", gen))}
	}
	th.panes = []map[string]any{
		tabPane("w1:p1", "w1", "w1:t1", "claude", "working", nil),
		tabPane("w2:p1", "w2", "w2:t1", "claude", "working", nil),
	}
	flip()
	// Each poll lists the panes, which never change, then the workspaces:
	// the layout moves there, before the tabs and the names are listed.
	th.onCall = func(method string) {
		if method == "workspace.list" {
			flip()
		}
	}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, configDir(t, "[projects.alpha]\nrepo = \"/y\"\nshort = \"al\"\n[projects.beta]\nrepo = \"/z\"\nshort = \"be\"\n"))
	o.Poll = 5 * time.Millisecond
	run(t, o)
	sock := socketPath(t, dir, th.path)
	require.Eventually(t, func() bool {
		_, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{WorkspaceID: "w1"})
		return err == nil
	}, ready, 5*time.Millisecond)

	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	deadline := time.Now().Add(ready)
	for i := range 8 {
		ws := []string{"w1", "w2"}[i%2]
		wg.Go(func() {
			for n := 0; ; n++ {
				mu.Lock()
				enough := len(seen) >= 10 && n >= 20
				mu.Unlock()
				if enough || time.Now().After(deadline) {
					errs <- nil
					return
				}
				s, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{WorkspaceID: ws})
				if err != nil {
					errs <- err
					return
				}
				g, err := pollOf(s, ws)
				if err != nil {
					errs <- err
					return
				}
				want := "alpha"
				if (g%2 == 1) == (ws == "w1") {
					want = "beta"
				}
				if s.Team != want {
					errs <- fmt.Errorf("%s in poll %d is %s's, but %s was served", ws, g, want, s.Team)
					return
				}
				mu.Lock()
				seen[g] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.GreaterOrEqual(t, len(seen), 10, "the readers saw the polls change under them")
}

// pollOf is the poll that named ws's pane in s, which must hold it.
func pollOf(s snapshot.Snapshot, ws string) (int, error) {
	for _, g := range s.Groups {
		for _, a := range g.Agents {
			if a.PaneID != ws+":p1" {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(a.Name, "g%d-"+ws, &n); err != nil {
				return 0, fmt.Errorf("pane %s named %q: %w", a.PaneID, a.Name, err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("team %s was served for %s without its pane: %+v", s.Team, ws, s.Groups)
}

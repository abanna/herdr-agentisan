package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// agentProc is a foreground process as pane.process_info answers it.
func agentProc(pid int, argv ...string) map[string]any {
	return map[string]any{"pid": pid, "name": "x", "argv": argv}
}

// processAsks counts srv's pane.process_info requests, by pane.
func processAsks(srv *herdrtest.Server) map[string]int {
	out := map[string]int{}
	for _, r := range srv.Requests() {
		if r.Method != "pane.process_info" {
			continue
		}
		var p struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(r.Params, &p)
		out[p.PaneID]++
	}
	return out
}

// withModels is s with each row's model, the boss's too, set from byPane.
func withModels(s snapshot.Snapshot, byPane map[string]string) snapshot.Snapshot {
	if s.Boss != nil {
		b := *s.Boss
		b.Agent.Model = byPane[b.Agent.PaneID]
		s.Boss = &b
	}
	groups := make([]snapshot.Group, len(s.Groups))
	for i, g := range s.Groups {
		agents := make([]snapshot.Agent, len(g.Agents))
		for j, a := range g.Agents {
			a.Model = byPane[a.PaneID]
			agents[j] = a
		}
		g.Agents = agents
		groups[i] = g
	}
	s.Groups = groups
	return s
}

// withBossStart is s with its boss's started_at set from byPane, in UTC, or
// none.
func withBossStart(s snapshot.Snapshot, byPane map[string]time.Time) snapshot.Snapshot {
	if s.Boss == nil {
		return s
	}
	b := *s.Boss
	b.StartedAt = nil
	if at, ok := byPane[b.PaneID]; ok {
		at = at.UTC()
		b.StartedAt = &at
	}
	s.Boss = &b
	return s
}

// TestSnapshotCarriesLaunchModels: every team's snapshot, the spaces' and
// both tabs teams', carries each row's launch model, the boss's included
// (A39), each boss its agent process's start (A47), and the op's data still
// decodes strictly. Shells and panes outside every team are never asked,
// and a poll asks nothing for a pane that has not changed.
func TestSnapshotCarriesLaunchModels(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	everyTeam(th)
	th.mu.Lock()
	th.procs = map[string][]map[string]any{
		"wJ:p1":  {agentProc(1001, "/home/u/.local/bin/claude", "--model", "opus", "--name", "boss")},
		"wN:p1":  {agentProc(1002, "claude", "--model", "claude-opus-5-5", "--name", "pee01")},
		"wN:p2":  {agentProc(1003, "claude")},
		"wN:p3":  {agentProc(1004, "/bin/zsh"), agentProc(1005, "claude", "--model=sonnet")},
		"wN:p4":  {agentProc(1006, "node", "/x/bin/codex", "-m", "o3"), agentProc(1007, "/x/vendor/codex", "-m", "o3")},
		"wQ:p1":  {agentProc(1008, "claude", "--model", "haiku")},
		"wP:p1":  {agentProc(1009, "claude", "--model", "opus")},
		"w14:p1": {agentProc(1010, "claude", "--model", "claude-fable-5-1")},
		"w14:p3": {agentProc(1011, "claude", "--model", "claude-opus-5-5")},
		"w20:p1": {agentProc(1012, "claude", "--model", "\x1b[31mopus")},
		"w20:p2": {agentProc(1013, "codex", "--no-daemon")},
		"w2:p1":  {agentProc(1014, "claude", "--model", "elsewhere")},
		"wJ:p2":  {agentProc(1015, "claude", "--model", "a shell")},
	}
	th.mu.Unlock()
	pt := &procs{start: map[int]uint64{}}
	for pid := 1001; pid <= 1015; pid++ {
		pt.set(pid, uint64(pid))
	}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, configDir(t, everyTeamConfig))
	pt.set(os.Getpid(), 77)
	o.Hooks.StartTime = pt.startTime
	o.Now = func() time.Time { return t0 }
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)

	models := map[string]string{
		"wJ:p1": "opus", "wN:p1": "claude-opus-5-5", "wN:p3": "sonnet", "wN:p4": "o3", "wQ:p1": "haiku", "wP:p1": "opus",
		"w14:p1": "claude-fable-5-1", "w14:p3": "claude-opus-5-5",
	}
	// The bosses' processes, by ticksSinceBoot: 1001 and 1010 ticks.
	starts := map[string]time.Time{"wJ:p1": boot.Add(10010 * time.Millisecond), "w14:p1": boot.Add(10100 * time.Millisecond)}
	want := wantTeams()
	served(t, sock, func(s snapshot.Snapshot) bool { return s.Boss != nil && s.Boss.Agent.Model != "" })
	for _, project := range []string{"agentisan", "alpha", "beta"} {
		answer := request(t, sock, []byte(`{"v":1,"op":"snapshot","args":{"project":"`+project+`"}}`+"\n"))
		require.Equal(t, true, answer["ok"], project)
		data, err := json.Marshal(answer["data"])
		require.NoError(t, err)
		got, err := snapshot.Decode(bytes.NewReader(data))
		require.NoError(t, err, "%s: one strict snapshot v1 document", project)
		assert.Equal(t, withBossStart(withModels(want[project], models), starts), got, project)
	}

	asked := processAsks(th.srv)
	polled(t, th.srv, 3)
	assert.Equal(t, asked, processAsks(th.srv), "an unchanged pane is not asked again")
	for _, p := range []string{"wJ:p1", "wN:p1", "wN:p2", "wN:p3", "wN:p4", "wQ:p1", "wP:p1", "w14:p1", "w14:p2", "w14:p3", "w20:p1", "w20:p2"} {
		assert.Equal(t, 1, asked[p], "%s, a team's agent pane, is asked once", p)
	}
	for _, p := range []string{"w2:p1", "wJ:p2", "wN:p5", "w14:p4", "w12:p1", "wZ:p1"} {
		assert.Zero(t, asked[p], "%s, outside every team or a shell, is never asked", p)
	}
}

// procHerdr answers pane.process_info alone, from procs, with an error code
// for a pane in codes, after a delay for a pane in slow.
type procHerdr struct {
	mu    sync.Mutex
	procs map[string][]map[string]any
	codes map[string]string
	slow  map[string]time.Duration
	srv   *herdrtest.Server
}

func newProcHerdr(t *testing.T) *procHerdr {
	t.Helper()
	ph := &procHerdr{procs: map[string][]map[string]any{}, codes: map[string]string{}, slow: map[string]time.Duration{}}
	ph.srv = herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
		var p struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(r.Params, &p)
		ph.mu.Lock()
		procs, code, slow := ph.procs[p.PaneID], ph.codes[p.PaneID], ph.slow[p.PaneID]
		ph.mu.Unlock()
		time.Sleep(slow)
		if code != "" {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: "refused"}}
		}
		if procs == nil {
			procs = []map[string]any{}
		}
		return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": p.PaneID, "shell_pid": 1, "foreground_process_group_id": 1, "foreground_processes": procs,
		}}}
	})
	return ph
}

func (ph *procHerdr) set(pane string, procs ...map[string]any) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	ph.procs[pane] = procs
}

func (ph *procHerdr) refuse(pane, code string) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	ph.codes[pane] = code
}

func (ph *procHerdr) delay(pane string, d time.Duration) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	ph.slow[pane] = d
}

// exit removes pid from p, as when its process exits.
func exit(p *procs, pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.start, pid)
}

func agentPane(id, terminal, agent string) herdr.PaneInfo {
	return herdr.PaneInfo{PaneID: id, TerminalID: terminal, Agent: agent}
}

// TestLaunchModelsAskOnlyWhenAPaneChanged walks one cache poll by poll
// (A39): a pane is asked again only when its terminal, its agent or its
// agent process changed, a miss only ModelRetry after it was asked, and a
// pane no longer listed is forgotten.
func TestLaunchModelsAskOnlyWhenAPaneChanged(t *testing.T) {
	t.Parallel()
	ph := newProcHerdr(t)
	c := herdr.Client{SocketPath: ph.srv.Path}
	pt := &procs{start: map[int]uint64{101: 1, 200: 1, 201: 5, 301: 7, 401: 1}}
	lm := daemon.NewLaunchModels()
	var logs lockedLog
	log := zerolog.New(&logs)
	now := t0
	ph.set("w1:p1", agentProc(101, "/home/u/.local/bin/claude", "--model", "claude-opus-5-5", "--append-system-prompt", "SECRET-PROMPT"))
	ph.set("w1:p2", agentProc(200, "/bin/zsh"), agentProc(201, "node", "/x/bin/codex", "-m", "o3"))
	ph.set("w1:p3", agentProc(301, "claude"))
	ph.set("w1:p4", agentProc(401, "/bin/sh", "wrapper.sh"))
	panes := []herdr.PaneInfo{agentPane("w1:p1", "t1", "claude"), agentPane("w1:p2", "t2", "codex"), agentPane("w1:p3", "t3", "claude"), agentPane("w1:p4", "t4", "claude")}
	poll := func(want map[string]string, msg string) {
		t.Helper()
		got, err := lm.Refresh(t.Context(), c, panes, pt.startTime, now, log)
		require.NoError(t, err)
		assert.Equal(t, want, got, msg)
	}
	asks := func(want map[string]int, msg string) {
		t.Helper()
		assert.Equal(t, want, processAsks(ph.srv), msg)
	}

	poll(map[string]string{"w1:p1": "claude-opus-5-5", "w1:p2": "o3"}, "a hit has a model; one with none, and a miss, have none")
	asks(map[string]int{"w1:p1": 1, "w1:p2": 1, "w1:p3": 1, "w1:p4": 1}, "the first poll asks each pane")
	now = now.Add(3 * time.Second)
	poll(map[string]string{"w1:p1": "claude-opus-5-5", "w1:p2": "o3"}, "the same models")
	asks(map[string]int{"w1:p1": 1, "w1:p2": 1, "w1:p3": 1, "w1:p4": 1}, "an unchanged pane is not asked again")

	ph.set("w1:p1", agentProc(101, "claude", "--model", "sonnet"))
	pt.set(101, 2)
	poll(map[string]string{"w1:p1": "sonnet", "w1:p2": "o3"}, "the pid reused by a later process")
	exit(pt, 101)
	pt.set(102, 3)
	ph.set("w1:p1", agentProc(102, "claude", "--model=opus"))
	poll(map[string]string{"w1:p1": "opus", "w1:p2": "o3"}, "the agent exited and another started")
	panes[1].TerminalID = "t2b"
	ph.set("w1:p2", agentProc(201, "node", "/x/bin/codex", "-m", "o4"))
	poll(map[string]string{"w1:p1": "opus", "w1:p2": "o4"}, "a new terminal, as after a live handoff, is asked again")
	panes[2].Agent = "codex"
	pt.set(302, 9)
	ph.set("w1:p3", agentProc(302, "codex", "-mgpt-5.5"))
	poll(map[string]string{"w1:p1": "opus", "w1:p2": "o4", "w1:p3": "gpt-5.5"}, "another agent in the pane")
	asks(map[string]int{"w1:p1": 3, "w1:p2": 2, "w1:p3": 2, "w1:p4": 1}, "each change costs one ask")

	pt.set(402, 4)
	ph.set("w1:p4", agentProc(402, "claude", "--model", "haiku"))
	now = t0.Add(daemon.ModelRetry - time.Second)
	poll(map[string]string{"w1:p1": "opus", "w1:p2": "o4", "w1:p3": "gpt-5.5"}, "a miss is not asked again within ModelRetry")
	asks(map[string]int{"w1:p1": 3, "w1:p2": 2, "w1:p3": 2, "w1:p4": 1}, "nor is anything else")
	now = t0.Add(daemon.ModelRetry)
	poll(map[string]string{"w1:p1": "opus", "w1:p2": "o4", "w1:p3": "gpt-5.5", "w1:p4": "haiku"}, "a miss is asked again ModelRetry later")
	asks(map[string]int{"w1:p1": 3, "w1:p2": 2, "w1:p3": 2, "w1:p4": 2}, "once")

	all := panes
	panes = panes[:1]
	poll(map[string]string{"w1:p1": "opus"}, "a pane no longer listed has no model")
	panes = all
	poll(map[string]string{"w1:p1": "opus", "w1:p2": "o4", "w1:p3": "gpt-5.5", "w1:p4": "haiku"}, "and is asked again when it comes back")
	asks(map[string]int{"w1:p1": 3, "w1:p2": 3, "w1:p3": 3, "w1:p4": 3}, "its entry was dropped")

	// Each of these is a miss: no model, and not asked again within
	// ModelRetry.
	pt.set(501, 1)
	ph.set("w2:p1", map[string]any{"pid": 501, "name": "claude"})
	ph.set("w2:p2", agentProc(0, "claude", "--model", "opus"))
	ph.set("w2:p3", agentProc(601, "claude", "--model", "opus"))
	panes = append(panes, agentPane("w2:p1", "t", "claude"), agentPane("w2:p2", "t", "claude"), agentPane("w2:p3", "t", "claude"))
	wantAll := map[string]string{"w1:p1": "opus", "w1:p2": "o4", "w1:p3": "gpt-5.5", "w1:p4": "haiku"}
	poll(wantAll, "argv herdr could not read; a pid herdr reports as null; an agent gone before its start time is read")
	now = now.Add(3 * time.Second)
	poll(wantAll, "not asked again")
	got := processAsks(ph.srv)
	for _, p := range []string{"w2:p1", "w2:p2", "w2:p3"} {
		assert.Equal(t, 1, got[p], p)
	}
	assert.Empty(t, logs.String(), "nothing here is logged")
}

// TestLaunchModelsWithoutAStartTime: off Linux and darwin there is no start
// time to read, so an entry holds until its pane's terminal or agent
// changes.
func TestLaunchModelsWithoutAStartTime(t *testing.T) {
	t.Parallel()
	ph := newProcHerdr(t)
	c := herdr.Client{SocketPath: ph.srv.Path}
	lm := daemon.NewLaunchModels()
	ph.set("w1:p1", agentProc(101, "claude", "--model", "opus"))
	panes := []herdr.PaneInfo{agentPane("w1:p1", "t1", "claude")}
	poll := func(want string) {
		t.Helper()
		got, err := lm.Refresh(t.Context(), c, panes, nil, t0, zerolog.Nop())
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"w1:p1": want}, got)
	}
	poll("opus")
	ph.set("w1:p1", agentProc(102, "claude", "--model", "sonnet"))
	poll("opus")
	assert.Equal(t, 1, processAsks(ph.srv)["w1:p1"], "the process is not checked")
	panes[0].TerminalID = "t2"
	poll("sonnet")
	panes[0].Agent = "codex"
	ph.set("w1:p1", agentProc(103, "codex", "-m", "o3"))
	poll("o3")
	assert.Equal(t, 3, processAsks(ph.srv)["w1:p1"])
}

// TestLaunchModelsStarted: each agent process found has its start on the
// wall clock (A47), put there once while its entry holds, so the boss's
// runtime never moves from poll to poll; a start the clock cannot place is
// tried again at the next poll, and a new process is placed afresh. A miss,
// a pane with no start time read, and a daemon with no clock have none.
func TestLaunchModelsStarted(t *testing.T) {
	t.Parallel()
	ph := newProcHerdr(t)
	c := herdr.Client{SocketPath: ph.srv.Path}
	pt := &procs{start: map[int]uint64{101: 100, 201: 200, 301: 300}}
	lm := daemon.NewLaunchModels()
	ph.set("w1:p1", agentProc(101, "claude"))
	ph.set("w1:p2", agentProc(201, "codex", "-m", "o3"))
	ph.set("w1:p3", agentProc(301, "claude"))
	ph.set("w1:p4", agentProc(401, "/bin/zsh"))
	panes := []herdr.PaneInfo{agentPane("w1:p1", "t1", "claude"), agentPane("w1:p2", "t2", "codex"), agentPane("w1:p3", "t3", "claude"), agentPane("w1:p4", "t4", "claude")}
	refresh := func() {
		t.Helper()
		_, err := lm.Refresh(t.Context(), c, panes, pt.startTime, t0, zerolog.Nop())
		require.NoError(t, err)
	}
	// wall places every start but w1:p3's first, counting each call by
	// start.
	var mu sync.Mutex
	calls := map[uint64]int{}
	wall := func(start uint64) (time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[start]++
		if start == 300 && calls[start] == 1 {
			return time.Time{}, errors.New("no boot time")
		}
		return ticksSinceBoot(start)
	}
	at := func(ticks int) time.Time { return boot.Add(time.Duration(ticks) * 10 * time.Millisecond) }

	refresh()
	assert.Empty(t, lm.Started(nil), "no clock: no starts")
	assert.Equal(t, map[string]time.Time{"w1:p1": at(100), "w1:p2": at(200)}, lm.Started(wall),
		"each agent's start; w1:p3's is not placed yet, and w1:p4 runs no agent")
	refresh()
	assert.Equal(t, map[string]time.Time{"w1:p1": at(100), "w1:p2": at(200), "w1:p3": at(300)}, lm.Started(wall),
		"w1:p3 placed at the next poll")
	assert.Equal(t, map[uint64]int{100: 1, 200: 1, 300: 2}, calls, "a start once placed is not placed again")

	// The same pid, a later process: the same model, so only the start
	// differs (QA #81, S1).
	pt.set(101, 9000)
	refresh()
	assert.Equal(t, at(9000), lm.Started(wall)["w1:p1"], "a reused pid is a new process: its own start")

	exit(pt, 101)
	pt.set(102, 150)
	ph.set("w1:p1", agentProc(102, "claude"))
	refresh()
	assert.Equal(t, at(150), lm.Started(wall)["w1:p1"], "a new agent process in the pane starts again")

	exit(pt, 201)
	ph.set("w1:p2", agentProc(201, "/bin/zsh"))
	refresh()
	got := lm.Started(wall)
	assert.NotContains(t, got, "w1:p2", "its agent gone, it has no start: never a guess")
	assert.Equal(t, at(300), got["w1:p3"])

	zero := daemon.NewLaunchModels()
	_, err := zero.Refresh(t.Context(), c, panes[:1], pt.startTime, t0, zerolog.Nop())
	require.NoError(t, err)
	assert.Empty(t, zero.Started(func(uint64) (time.Time, error) { return time.Time{}, nil }), "a zero time is not a start")

	untimed := daemon.NewLaunchModels()
	_, err = untimed.Refresh(t.Context(), c, panes, nil, t0, zerolog.Nop())
	require.NoError(t, err)
	assert.Empty(t, untimed.Started(wall), "no start time read, as off Linux and darwin: none")
}

// TestLaunchModelsFailures: a failed ask leaves its pane no model and is
// asked again at the next poll, logged once until an ask succeeds; a pane
// closed since the list is not logged; ErrHerdrGone stops the asks; a
// stopping daemon asks nothing and logs nothing; and no log holds argv.
func TestLaunchModelsFailures(t *testing.T) {
	t.Parallel()
	ph := newProcHerdr(t)
	c := herdr.Client{SocketPath: ph.srv.Path}
	pt := &procs{start: map[int]uint64{101: 1, 201: 1}}
	lm := daemon.NewLaunchModels()
	var logs lockedLog
	log := zerolog.New(&logs)
	ph.set("w1:p1", agentProc(101, "claude", "--model", "opus", "--system-prompt", "SECRET-PROMPT"))
	ph.set("w1:p2", agentProc(201, "claude", "--model", "sonnet"))
	ph.refuse("w1:p1", "server_error")
	ph.refuse("w1:p2", "pane_not_found")
	panes := []herdr.PaneInfo{agentPane("w1:p1", "t1", "claude"), agentPane("w1:p2", "t2", "claude")}
	poll := func(want map[string]string) {
		t.Helper()
		got, err := lm.Refresh(t.Context(), c, panes, pt.startTime, t0, log)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	failed := func() int { return strings.Count(logs.String(), "read a pane's processes") }

	for range 3 {
		poll(map[string]string{})
	}
	assert.Equal(t, map[string]int{"w1:p1": 3, "w1:p2": 3}, processAsks(ph.srv), "a failed ask is asked again each poll")
	assert.Equal(t, 1, failed(), "logged once")
	assert.NotContains(t, logs.String(), "w1:p2", "a pane closed since the list is not logged")

	ph.refuse("w1:p1", "")
	poll(map[string]string{"w1:p1": "opus"})
	ph.set("w1:p1", agentProc(102, "claude"))
	exit(pt, 101)
	ph.refuse("w1:p1", "server_error")
	poll(map[string]string{})
	poll(map[string]string{})
	assert.Equal(t, 2, failed(), "logged again after an ask succeeded")
	assert.NotContains(t, logs.String(), "SECRET")
	assert.NotContains(t, logs.String(), "--model")

	var dials int
	gone := herdr.Client{SocketPath: ph.srv.Path, Dialed: func() error { dials++; return daemon.ErrHerdrGone }}
	_, err := daemon.NewLaunchModels().Refresh(t.Context(), gone, panes, pt.startTime, t0, log)
	require.ErrorIs(t, err, daemon.ErrHerdrGone)
	assert.Equal(t, 1, dials, "nothing more is asked")

	before, logged := len(ph.srv.Requests()), logs.String()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := daemon.NewLaunchModels().Refresh(ctx, c, panes, pt.startTime, t0, log)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Len(t, ph.srv.Requests(), before, "a stopping daemon asks nothing")
	assert.Equal(t, logged, logs.String(), "and logs nothing")
}

// TestLaunchModelsBudget: a poll's asks share ModelBudget, so a slow herdr
// costs it about that much and no more; the panes not asked in time are
// asked at a later poll, and the overrun is logged once until a poll asks
// every pane.
func TestLaunchModelsBudget(t *testing.T) {
	t.Parallel()
	ph := newProcHerdr(t)
	c := herdr.Client{SocketPath: ph.srv.Path}
	pt := &procs{start: map[int]uint64{101: 1, 201: 1, 301: 1}}
	lm := daemon.NewLaunchModels()
	var logs lockedLog
	log := zerolog.New(&logs)
	ph.set("w1:p1", agentProc(101, "claude", "--model", "opus"))
	ph.set("w1:p2", agentProc(201, "claude", "--model", "sonnet"))
	ph.set("w1:p3", agentProc(301, "claude", "--model", "haiku"))
	panes := []herdr.PaneInfo{agentPane("w1:p1", "t1", "claude"), agentPane("w1:p2", "t2", "claude"), agentPane("w1:p3", "t3", "claude")}
	poll := func(want map[string]string) {
		t.Helper()
		start := time.Now()
		got, err := lm.Refresh(t.Context(), c, panes, pt.startTime, t0, log)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		assert.Less(t, time.Since(start), daemon.ModelBudget+herdr.CallTimeout/2, "the poll is not held past the budget")
	}
	slow := func() int { return strings.Count(logs.String(), "herdr is slow") }

	ph.delay("w1:p1", daemon.ModelBudget+300*time.Millisecond)
	poll(map[string]string{})
	assert.Equal(t, map[string]int{"w1:p1": 1}, processAsks(ph.srv), "the panes after the slow one wait")
	poll(map[string]string{})
	assert.Equal(t, 1, slow(), "logged once")
	ph.delay("w1:p1", 0)
	poll(map[string]string{"w1:p1": "opus", "w1:p2": "sonnet", "w1:p3": "haiku"})
	assert.Equal(t, 1, slow())
	ph.delay("w1:p2", daemon.ModelBudget+300*time.Millisecond)
	panes[1].TerminalID = "t2b"
	poll(map[string]string{"w1:p1": "opus", "w1:p3": "haiku"})
	assert.Equal(t, 2, slow(), "logged again after a poll that asked every pane")
	assert.NotContains(t, logs.String(), "read a pane's processes", "running out of time is not a failed ask")
}

// TestAgentPanes: a team's agent panes, workers and bosses, as pane.list
// lists them; never a shell, nor a pane in no team.
func TestAgentPanes(t *testing.T) {
	t.Parallel()
	m := team.Model{Projects: []team.Project{
		{Name: "a", Boss: []team.Worker{{Pane: team.Pane{ID: "w1:p1"}}, {Pane: team.Pane{ID: "w1:p2"}}}, Groups: []team.Group{
			{Name: "coder", Workers: []team.Worker{{Pane: team.Pane{ID: "w1:p3"}}, {Pane: team.Pane{ID: "w1:p4"}}}},
		}},
		{Name: "b", Groups: []team.Group{{Name: "qa", Workers: []team.Worker{{Pane: team.Pane{ID: "w2:p1"}}}}, {Name: "none"}}},
	}}
	panes := []herdr.PaneInfo{
		{PaneID: "w1:p1", Agent: "claude", TerminalID: "t1"},
		{PaneID: "w1:p2"},
		{PaneID: "w1:p3", Agent: "codex"},
		{PaneID: "w1:p4"},
		{PaneID: "w2:p1", Agent: "gemini"},
		{PaneID: "w9:p1", Agent: "claude"},
	}
	assert.Equal(t, []herdr.PaneInfo{panes[0], panes[2], panes[4]}, daemon.AgentPanes(m, panes))
	assert.Empty(t, daemon.AgentPanes(team.Model{}, panes))
}

// TestOneProcessInfoAskPerPanePerPoll: on a poll where a worker's shell and
// its agent's model are both new, as after a spawn or a live handoff, the
// reconcile's shell read (A35) and the model (A39) share the pane's one
// pane.process_info reply, and later polls ask it nothing.
func TestOneProcessInfoAskPerPanePerPoll(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	dir := shortDir(t)
	seedRow(t, dir, th, "ha-coder-1", "w14:p2", "term_w14:p2", 0, 0)
	th.setShell("w14:p2", 4242, false)
	th.mu.Lock()
	th.procs = map[string][]map[string]any{
		"w14:p1": {agentProc(1001, "claude", "--model", "opus")},
		"w14:p2": {agentProc(1002, "claude", "--model", "claude-opus-5-5")},
	}
	th.mu.Unlock()
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 77, 4242: 9, 1001: 1, 1002: 2}}).startTime
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)

	eventually(t, dir, th, "ha-coder-1", workerRow{Pane: "w14:p2", Terminal: "term_w14:p2", Status: "active", ShellPID: 4242, ShellStart: 9})
	servedTeam(t, sock, "ha", func(s snapshot.Snapshot) bool {
		for _, g := range s.Groups {
			for _, a := range g.Agents {
				if a.PaneID == "w14:p2" {
					return a.Model == "claude-opus-5-5"
				}
			}
		}
		return false
	})
	polled(t, th.srv, 3)
	asks := processAsks(th.srv)
	assert.Equal(t, 1, asks["w14:p2"], "the coder's pane is asked once for its shell and its model")
	for pane, n := range asks {
		assert.Equal(t, 1, n, "pane %s: one ask, then none while nothing changes", pane)
	}
}

// TestPaneProcessesAskOncePerPoll: within a poll a pane is asked once, a
// failed answer included, and a socket that names another server is never
// held, so each caller sees it and the poll ends.
func TestPaneProcessesAskOncePerPoll(t *testing.T) {
	t.Parallel()
	ph := newProcHerdr(t)
	c := herdr.Client{SocketPath: ph.srv.Path}
	ph.set("w1:p1", agentProc(101, "claude", "--model", "opus"))
	ph.refuse("w1:p2", "server_error")
	pp := daemon.NewPaneProcesses()

	for range 2 {
		info, err := pp.Ask(t.Context(), c, "w1:p1")
		require.NoError(t, err)
		require.Len(t, info.ForegroundProcesses, 1)
		assert.Equal(t, []string{"claude", "--model", "opus"}, info.ForegroundProcesses[0].Argv)
		_, err = pp.Ask(t.Context(), c, "w1:p2")
		require.ErrorIs(t, err, herdr.ErrAPI)
		assert.Contains(t, err.Error(), "w1:p2")
	}
	assert.Equal(t, map[string]int{"w1:p1": 1, "w1:p2": 1}, processAsks(ph.srv), "one ask each, the failed one too")
	_, err := daemon.NewPaneProcesses().Ask(t.Context(), c, "w1:p1")
	require.NoError(t, err)
	assert.Equal(t, 2, processAsks(ph.srv)["w1:p1"], "the next poll asks again")

	dials := 0
	gone := herdr.Client{SocketPath: ph.srv.Path, Dialed: func() error {
		dials++
		return daemon.ErrHerdrGone
	}}
	for range 2 {
		_, err := pp.Ask(t.Context(), gone, "w1:p3")
		require.ErrorIs(t, err, daemon.ErrHerdrGone)
	}
	assert.Equal(t, 2, dials, "a reply that ends the poll is not held")
}

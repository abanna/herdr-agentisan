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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
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
// order, then the rest by name; the rows by pane id; every stage, and the
// boss, first seen at t0.
func wantFullTeam() snapshot.Snapshot {
	return snapshot.Snapshot{
		V: 1, At: t0, Team: "agentisan", Project: &snapshot.Project{Color: "#7c3aed"},
		Boss: &snapshot.Boss{Agent: snapshot.Agent{
			Name: "boss", PaneID: "wJ:p1", Status: "working", Ctx: new(23), Item: "#867", Stage: "review", StageStartedAt: new(t0),
		}, StartedAt: new(t0)},
		Groups: []snapshot.Group{
			{Name: "coders", Agents: []snapshot.Agent{
				{Name: "pee01", PaneID: "wN:p1", Status: "working", Ctx: new(40), Item: "NERD-5255", Stage: "build_test", StageStartedAt: new(t0)},
				{Name: "pee02", PaneID: "wN:p2", Status: "ready", Ctx: new(61)},
				{Name: "pee03", PaneID: "wN:p3", Status: "blocked", Ctx: new(78), Item: "NERD-5247", Stage: "waiting on boss", StageStartedAt: new(t0)},
				{Name: "scratch", Unnamed: true, PaneID: "wN:p4", Status: "idle"},
				{Name: "wN:p5", Unnamed: true, PaneID: "wN:p5", Status: "unknown"},
			}},
			{Name: "precheck", Agents: []snapshot.Agent{
				{Name: "wP:p1", Unnamed: true, PaneID: "wP:p1", Status: "working", Ctx: new(100), Stage: "running", StageStartedAt: new(t0)},
			}},
			{Name: "qa", Agents: []snapshot.Agent{{Name: "qa", PaneID: "wQ:p1", Status: "idle"}}},
			{Name: "research", Agents: []snapshot.Agent{}},
			{Name: "zeta", Agents: []snapshot.Agent{{Name: "notes", Unnamed: true, PaneID: "wZ:p1", Status: "unknown"}}},
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
// ok, and returns it.
func served(t *testing.T, sock string, ok func(snapshot.Snapshot) bool) snapshot.Snapshot {
	t.Helper()
	var got snapshot.Snapshot
	require.Eventually(t, func() bool {
		s, err := daemon.Snapshot(t.Context(), sock)
		if err != nil {
			return false
		}
		got = s
		return ok(s)
	}, ready, 5*time.Millisecond, "the daemon never served the snapshot wanted; last: %+v", got)
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

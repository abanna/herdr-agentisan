package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
)

const bossSnapshotOp = "boss.snapshot"

func shiftHerdr(t *testing.T) *teamHerdr {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("daemon peer credentials require Linux or Darwin")
	}
	h := newTeamHerdr(t)
	everyTeam(h)
	h.mu.Lock()
	dash := tabPane("w14:pD", "w14", "w14:t1", "", "", nil)
	dash["label"] = "al-dashboard"
	h.panes = append(h.panes, tabPane("w14:p0", "w14", "w14:t1", "claude", "working", nil),
		dash, tabPane("w20:p0", "w20", "w20:t0", "claude", "working", nil))
	h.tabs = append(h.tabs, tabOf("w20:t0", "w20", "be-boss"))
	h.agents = append(h.agents, named("w14:p0", "al-candidate"), named("w20:p0", "be-boss"))
	h.procs = map[string][]map[string]any{"w14:pD": {{"pid": 0, "argv": []string{"/bin/herdr-agentisan", "dashboard"}}}}
	h.mu.Unlock()
	return h
}

func shiftCaller(h *teamHerdr, pane string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shells = map[string]uint32{pane: uint32(os.Getpid())} //nolint:gosec // test PID
}

func shiftOpts(t *testing.T, h *teamHerdr) daemon.Options {
	t.Helper()
	return teamOpts(t, shortDir(t), h, configDir(t, everyTeamConfig))
}

func startShift(t *testing.T, h *teamHerdr, o daemon.Options) string {
	t.Helper()
	serving(t, o, h.fakeHerdr)
	return socketPath(t, o.StateDir, h.path)
}

func shiftRead(t *testing.T, sock string) snapshot.BossSnapshot {
	t.Helper()
	got, err := daemon.BossSnapshotOf(t.Context(), sock)
	require.NoError(t, err)
	return got
}

// NERD-5748: the candidate reads the canonical boss and its own team's rows.
func TestBossSnapshotPlacesCandidateInOwnTeam(t *testing.T) {
	t.Parallel()
	h := shiftHerdr(t)
	shiftCaller(h, "w14:p0")
	sock := startShift(t, h, shiftOpts(t, h))
	served(t, sock, anySnapshot)
	polled(t, h.srv, 1)

	got := shiftRead(t, sock)
	assert.Equal(t, "alpha", got.Team)
	require.NotNil(t, got.Boss)
	assert.Equal(t, "al-boss", got.Boss.Name)
	require.Len(t, got.Groups, 2)
	assert.Equal(t, "coder", got.Groups[0].Name)
	assert.Equal(t, "review", got.Groups[1].Name)
	for _, pane := range []string{"w14:p3", "w14:pD", ""} {
		shiftCaller(h, pane)
		_, err := daemon.Workforce(t.Context(), sock, bossSnapshotOp, struct{}{})
		assert.Equal(t, "not_allowed", refusedWith(t, err), "pane %q", pane)
	}
	shiftCaller(h, "w20:p0")
	assert.Equal(t, "beta", shiftRead(t, sock).Team)
	for _, args := range []any{map[string]string{"project": "alpha"}, map[string]string{"workspace_id": "w14"}, []string{}, nil} {
		_, err := daemon.Workforce(t.Context(), sock, bossSnapshotOp, args)
		assert.Equal(t, "bad_request", refusedWith(t, err), "args %#v", args)
	}
	h.mu.Lock()
	h.infoFails["w20:p0"] = true
	h.mu.Unlock()
	_, err := daemon.Workforce(t.Context(), sock, bossSnapshotOp, struct{}{})
	assert.Equal(t, "herdr_failed", refusedWith(t, err))
	assert.NotContains(t, err.Error(), "busy")
}

func TestBossSnapshotSharesSlowBoardPlacement(t *testing.T) {
	t.Parallel()
	h := shiftHerdr(t)
	// Beta is probed after the two alpha boss panes and the agentisan boss.
	shiftCaller(h, "w20:p0")
	sock := startShift(t, h, shiftOpts(t, h))
	served(t, sock, anySnapshot)
	h.mu.Lock()
	h.onCall = func(method string) {
		if method == "pane.process_info" {
			time.Sleep(1500 * time.Millisecond)
		}
	}
	h.mu.Unlock()
	_, err := daemon.Workforce(t.Context(), sock, daemon.OpBoardRead, daemon.BoardReadRequest{})
	require.NoError(t, err)
	assert.Equal(t, "beta", shiftRead(t, sock).Team)
}

func TestBossSnapshotAsOfPrecedesStatusFlip(t *testing.T) {
	t.Parallel()
	h := shiftHerdr(t)
	shiftCaller(h, "w14:p0")
	o := shiftOpts(t, h)
	o.Poll = time.Hour
	var clock atomic.Int64
	o.Now = func() time.Time { return boardNow.Add(time.Duration(clock.Load())) }
	h.mu.Lock()
	h.onCall = func(method string) {
		if method == "agent.list" {
			h.mu.Lock()
			h.panes = slices.Clone(h.panes)
			i := slices.IndexFunc(h.panes, func(p map[string]any) bool { return p["pane_id"] == "w14:p3" })
			h.panes[i] = tabPane("w14:p3", "w14", "w14:t3", "claude", "idle", nil)
			h.mu.Unlock()
			clock.Store(int64(time.Second))
			time.Sleep(100 * time.Millisecond)
		}
	}
	h.mu.Unlock()
	sock := startShift(t, h, o)
	raw, err := daemon.Workforce(t.Context(), sock, bossSnapshotOp, struct{}{})
	require.NoError(t, err)
	var got snapshot.BossSnapshot
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, snapshot.StatusWorking, got.Groups[0].Workers[0].Status)
	assert.Equal(t, &boardNow, got.TeamPoll.AsOf, "poll as_of must precede the status flip")
	assert.Equal(t, new(boardNow.Add(time.Second)), got.Board.AsOf, "board as_of comes from its own read")
}

func TestBossSnapshotPreservesMarksAndWaits(t *testing.T) {
	t.Parallel()
	h := shiftHerdr(t)
	shiftCaller(h, "w14:p0")
	o := shiftOpts(t, h)
	o.Now = func() time.Time { return boardNow }
	state := paths(t, o.StateDir, h.fakeHerdr)
	require.NoError(t, os.MkdirAll(state.Dir, 0o700))
	st, err := store.Open(t.Context(), state.DB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	p := boardPost(t, st, "alpha", "private board body")
	boardPost(t, st, "beta", "private other body")
	_, err = st.MarkSeen(t.Context(), "alpha", store.ReaderUser, p.Seq)
	require.NoError(t, err)
	sock := startShift(t, h, o)
	served(t, sock, anySnapshot)
	polled(t, h.srv, 1)

	type result struct {
		data json.RawMessage
		err  error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() {
			data, err := daemon.Workforce(t.Context(), sock, daemon.OpBoardWait, daemon.BoardWaitRequest{After: &p.Seq, Timeout: 2 * time.Second})
			done <- result{data, err}
		}()
	}
	// A superseded wait proves the surviving wait has joined the registry.
	select {
	case a := <-done:
		require.Equal(t, "superseded", refusedWith(t, a.err))
	case <-time.After(ready):
		t.Fatal("no wait reached the registry")
	}

	raw, err := daemon.Workforce(t.Context(), sock, bossSnapshotOp, struct{}{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "private")
	got := shiftRead(t, sock).Board
	assert.Equal(t, snapshot.Fresh, got.State)
	assert.Equal(t, new(int64(0)), got.Mark)
	assert.Equal(t, &p.Seq, got.Newest)
	assert.Equal(t, new(1), got.Unread)
	require.NotNil(t, got.AsOf)
	boss, err := st.BoardStatus(t.Context(), "alpha", store.ReaderBoss)
	require.NoError(t, err)
	assert.Zero(t, boss.Mark)
	user, err := st.BoardStatus(t.Context(), "alpha", store.ReaderUser)
	require.NoError(t, err)
	assert.Equal(t, p.Seq, user.Mark)
	select {
	case a := <-done:
		require.NoError(t, a.err)
		assert.Contains(t, string(a.data), `"timed_out":true`)
	case <-time.After(ready):
		t.Fatal("wait did not finish naturally")
	}

	db, err := sql.Open("sqlite", state.DB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), "DROP TABLE board_marks")
	require.NoError(t, err)
	got = shiftRead(t, sock).Board
	assert.Equal(t, snapshot.Unavailable, got.State)
	assert.Nil(t, got.AsOf)
	assert.Nil(t, got.Mark)
	assert.Nil(t, got.Newest)
	assert.Nil(t, got.Unread)
}

func TestBossSnapshotFreshnessAndProjection(t *testing.T) {
	t.Parallel()
	h := shiftHerdr(t)
	shiftCaller(h, "w14:p0")
	o := shiftOpts(t, h)
	var clock atomic.Int64
	o.Now = func() time.Time { return boardNow.Add(time.Duration(clock.Load())) }
	state := paths(t, o.StateDir, h.fakeHerdr)
	require.NoError(t, os.MkdirAll(state.Dir, 0o700))
	st, err := store.Open(t.Context(), state.DB)
	require.NoError(t, err)
	terminal := "term_w14:p3"
	row := store.Worker{Project: "alpha", Group: "coder", LogicalID: "al-coder-1", PaneID: "w14:p3", TerminalID: terminal, Status: store.StatusActive, Provider: store.ProviderClaude}
	gen, err := st.BindWorker(t.Context(), row, boardNow)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	h.mu.Lock()
	h.agentsErr = true
	for _, p := range h.panes {
		if p["pane_id"] == row.PaneID {
			p["terminal_id"] = terminal
		}
	}
	for i := range 129 {
		pid := fmt.Sprintf("w14:px%03d", i)
		h.panes = append(h.panes, tabPane(pid, "w14", "w14:t2", "", "", nil))
	}
	h.procs["w14:p3"] = []map[string]any{{"pid": 101, "name": "claude", "argv": []string{"claude", "private argv"}}}
	h.procs["w14:p1"] = []map[string]any{{"pid": 101, "name": "claude", "argv": []string{"claude"}}}
	h.mu.Unlock()
	h.setToken("w14:p3", report.CtxKey, "0")
	h.setToken("w14:p3", report.CtxAtKey, report.FormatModelAt(boardNow))
	h.setToken("w14:p2", report.CtxAtKey, report.FormatModelAt(boardNow))
	h.setToken("w14:p1", report.CtxKey, "99")
	o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 77, 101: 100}}).startTime
	sock := startShift(t, h, o)
	cold, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	_, err = daemon.Workforce(cold, sock, bossSnapshotOp, struct{}{})
	assert.Equal(t, "not_ready", refusedWith(t, err))
	h.mu.Lock()
	h.agentsErr = false
	h.mu.Unlock()
	served(t, sock, anySnapshot)
	polled(t, h.srv, 1)

	raw, err := daemon.Workforce(t.Context(), sock, bossSnapshotOp, struct{}{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "private argv")
	got := shiftRead(t, sock)
	assert.Equal(t, snapshot.Fresh, got.TeamPoll.State)
	assert.Equal(t, &boardNow, got.TeamPoll.AsOf)
	assert.Equal(t, snapshot.Fresh, got.Board.State)
	assert.Equal(t, new(0), got.Board.Unread)
	require.NotNil(t, got.Boss)
	assert.Equal(t, new(boot.Add(time.Second)), got.Boss.StartedAt)
	assert.Nil(t, got.Boss.Ctx, "no ctx_at")
	require.Len(t, got.Groups, 2)
	coder := got.Groups[0].Workers
	require.Len(t, coder, 1)
	assert.Equal(t, &row.LogicalID, coder[0].ID)
	assert.Equal(t, &gen, coder[0].Generation)
	assert.Equal(t, new(0), coder[0].Ctx)
	review := got.Groups[1].Workers
	require.Len(t, review, 127)
	assert.Nil(t, review[0].ID)
	assert.Nil(t, review[0].Generation)
	assert.Nil(t, review[0].Ctx, "no verified start")
	assert.Equal(t, 3, got.WorkersOmitted)
	h.setToken("w14:p3", report.CtxAtKey, report.FormatModelAt(boot))
	polled(t, h.srv, 2)
	assert.Nil(t, shiftRead(t, sock).Groups[0].Workers[0].Ctx, "predates this agent")
	h.setToken("w14:p3", report.CtxAtKey, report.FormatModelAt(boardNow.Add(time.Second)))
	polled(t, h.srv, 2)
	assert.Nil(t, shiftRead(t, sock).Groups[0].Workers[0].Ctx, "report is in the future")

	h.mu.Lock()
	h.agentsErr = true
	h.mu.Unlock()
	polled(t, h.srv, 2)
	clock.Store(int64(3 * o.Poll))
	assert.Equal(t, snapshot.Fresh, shiftRead(t, sock).TeamPoll.State)
	clock.Add(1)
	got = shiftRead(t, sock)
	assert.Equal(t, snapshot.Stale, got.TeamPoll.State)
	assert.Equal(t, &boardNow, got.TeamPoll.AsOf)
	assert.Equal(t, snapshot.Fresh, got.Board.State)
	clock.Store(-1)
	assert.Equal(t, snapshot.Stale, shiftRead(t, sock).TeamPoll.State, "clock moved back")

	h.mu.Lock()
	h.agents = append(h.agents, named("w14:p0", "al-boss"))
	h.agentsErr = false
	h.mu.Unlock()
	polled(t, h.srv, 2)
	assert.Nil(t, shiftRead(t, sock).Boss, "two panes claim the canonical name")

	h.mu.Lock()
	h.panes = slices.DeleteFunc(slices.Clone(h.panes), func(p map[string]any) bool { return p["pane_id"] == "w14:p1" })
	h.agents = slices.DeleteFunc(slices.Clone(h.agents), func(a map[string]any) bool { return a["name"] == "al-boss" })
	h.mu.Unlock()
	polled(t, h.srv, 2)
	got = shiftRead(t, sock)
	assert.Nil(t, got.Boss, "the sole candidate is not the canonical boss")
	var out strings.Builder
	require.NoError(t, got.WriteText(&out))
	assert.Equal(t, "Boss unknown (no agent named al-boss)", strings.SplitN(out.String(), "\n", 5)[3])

	db, err := sql.Open("sqlite", state.DB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), "ALTER TABLE workers RENAME TO unavailable_workers")
	require.NoError(t, err)
	clock.Add(1)
	polled(t, h.srv, 2)
	assert.Equal(t, got.TeamPoll.AsOf, shiftRead(t, sock).TeamPoll.AsOf, "failed worker read keeps the last successful poll")
}

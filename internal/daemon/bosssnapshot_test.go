package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
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
	shiftCaller(h, "w14:p1") // the boss pane the team lists first
	assert.Equal(t, "alpha", shiftRead(t, sock).Team)
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
		if method == "workspace.list" || method == "agent.list" {
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
	assert.Equal(t, new(boardNow.Add(time.Second-time.Millisecond)), got.Board.AsOf, "board as_of comes from its own read, less the clock slack")
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

	var seen, thaw sync.Once
	block, blocked := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { thaw.Do(func() { close(block) }) })
	h.mu.Lock()
	h.onCall = func(method string) { // the poll waits in agent.list, so the state below stays the last complete poll's
		if method == "agent.list" {
			seen.Do(func() { close(blocked) })
			<-block
		}
	}
	h.mu.Unlock()
	<-blocked
	clock.Store(int64(3 * o.Poll))
	assert.Equal(t, snapshot.Fresh, shiftRead(t, sock).TeamPoll.State)
	clock.Add(1)
	got = shiftRead(t, sock)
	assert.Equal(t, snapshot.Stale, got.TeamPoll.State)
	assert.Equal(t, &boardNow, got.TeamPoll.AsOf)
	assert.Equal(t, snapshot.Fresh, got.Board.State)
	thaw.Do(func() { close(block) })

	h.mu.Lock()
	h.agents = append(h.agents, named("w14:p0", "al-boss"))
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

// NERD-5748: a post committed as the clock is read is counted or later than as_of.
func TestBossSnapshotBoardAsOfPrecedesItsRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		sampled, at time.Duration
		fresh       bool
	}{{"clock steps forward", 0, time.Second, true}, {"clock steps back", time.Second, 0, false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := shiftHerdr(t)
			shiftCaller(h, "w14:p0")
			o := shiftOpts(t, h)
			o.Poll = time.Hour
			state := paths(t, o.StateDir, h.fakeHerdr)
			require.NoError(t, os.MkdirAll(state.Dir, 0o700))
			st, err := store.Open(t.Context(), state.DB)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, st.Close()) })
			var armed atomic.Bool
			o.Now = func() time.Time {
				if strings.Contains(string(debug.Stack()), "bossSnapshotOp") && armed.CompareAndSwap(true, false) {
					boardPost(t, st, "alpha", "committed as the clock is read")
					return boardNow.Add(c.sampled)
				}
				return boardNow.Add(c.at)
			}
			sock := startShift(t, h, o)
			shiftRead(t, sock)
			armed.Store(true)
			got := shiftRead(t, sock).Board
			assert.Equal(t, c.fresh, got.State == snapshot.Fresh, "fresh only if as_of is not after the reply's own time")
			if c.fresh {
				assert.Equal(t, new(1), got.Unread, "counted: sampled before the read")
			}
		})
	}
}

func swapBoss(h *teamHerdr) {
	h.tabs = append(slices.DeleteFunc(slices.Clone(h.tabs), func(tab map[string]any) bool { return tab["tab_id"] == "w14:t1" || tab["tab_id"] == "w14:t4" }),
		tabOf("w14:t1", "w14", "al-coder-2"), tabOf("w14:t4", "w14", "al-boss"))
}

// NERD-5748: a request paused after placement is refused once a poll moved on.
func TestBossSnapshotRefusesFormerBossAfterPlacement(t *testing.T) {
	t.Parallel()
	for name, moved := range map[string]func(*teamHerdr){
		"complete poll":            swapBoss,
		"degraded poll":            func(h *teamHerdr) { h.agentsErr = true; swapBoss(h) },
		"poll that built no model": func(h *teamHerdr) { h.workspacesErr = true },
		"pane id reissued": func(h *teamHerdr) { // the caller's terminal left, the replacement's took its id
			h.panes = slices.Clone(h.panes)
			i := slices.IndexFunc(h.panes, func(p map[string]any) bool { return p["pane_id"] == "w14:p0" })
			h.panes[i] = maps.Clone(h.panes[i])
			h.panes[i]["terminal_id"] = "term_replacement"
			h.shells = map[string]uint32{"w14:p3": uint32(os.Getpid())} //nolint:gosec // test PID
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := shiftHerdr(t)
			shiftCaller(h, "w14:p0")
			o := shiftOpts(t, h)
			var armed atomic.Bool
			var release sync.Once
			paused, resume := make(chan struct{}), make(chan struct{})
			o.Now = func() time.Time {
				if strings.Contains(string(debug.Stack()), "bossSnapshotOp") && armed.CompareAndSwap(true, false) {
					close(paused)
					<-resume
				}
				return boardNow
			}
			sock := startShift(t, h, o)
			t.Cleanup(func() { release.Do(func() { close(resume) }) }) // before the daemon's stop, which waits for the handler
			shiftRead(t, sock)
			armed.Store(true)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			got := make(chan error, 1)
			go func() { _, err := daemon.Workforce(ctx, sock, bossSnapshotOp, struct{}{}); got <- err }()
			select {
			case <-paused:
			case err := <-got:
				t.Fatalf("answered before the hook: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("the hook never ran")
			}
			h.mu.Lock()
			moved(h)
			h.mu.Unlock()
			require.Eventually(t, func() bool { // placement has moved on
				asked, stop := context.WithTimeout(t.Context(), 300*time.Millisecond) // not_ready is asked again
				defer stop()
				_, err := daemon.Workforce(asked, sock, daemon.OpBoardRead, daemon.BoardReadRequest{})
				return err != nil
			}, ready, 10*time.Millisecond)
			polled(t, h.srv, 2) // and the poll has published it
			release.Do(func() { close(resume) })
			assert.Contains(t, []string{"not_ready", "not_allowed"}, refusedWith(t, <-got), "never served")
		})
	}
}

// NERD-5748: a degraded poll serves the incoming boss stale rows naming the old boss.
func TestBossSnapshotIncomingBossDuringDegradedPoll(t *testing.T) {
	t.Parallel()
	h := shiftHerdr(t)
	shiftCaller(h, "w14:p0")
	o := shiftOpts(t, h)
	o.Now = func() time.Time { return boardNow } // no age: only the degraded poll can make the rows stale
	sock := startShift(t, h, o)
	before := shiftRead(t, sock)
	h.mu.Lock()
	h.agentsErr = true
	swapBoss(h)
	h.mu.Unlock()
	shiftCaller(h, "w14:p4")
	polled(t, h.srv, 2)
	got := shiftRead(t, sock)
	assert.Equal(t, snapshot.Stale, got.TeamPoll.State)
	assert.Equal(t, before.Boss, got.Boss, "the rows name the old boss")
}

// NERD-5748: a pane id reissued, or its pane moved, before any poll saw it is not the boss's.
func TestBossSnapshotRefusesPaneChangedBeforePoll(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"terminal_id", "tab_id"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			h := shiftHerdr(t)
			shiftCaller(h, "w14:p0")
			o := shiftOpts(t, h)
			o.Poll = time.Hour
			sock := startShift(t, h, o)
			shiftRead(t, sock)
			h.mu.Lock()
			h.panes = slices.Clone(h.panes)
			i := slices.IndexFunc(h.panes, func(p map[string]any) bool { return p["pane_id"] == "w14:p0" })
			h.panes[i] = maps.Clone(h.panes[i])
			h.panes[i][key] = "other"
			h.mu.Unlock()
			_, err := daemon.Workforce(t.Context(), sock, bossSnapshotOp, struct{}{})
			assert.Equal(t, "not_ready", refusedWith(t, err))
		})
	}
}

// NERD-5748: only the monotonic clock ages rows and tells that the wall clock stepped back.
func TestBossSnapshotIgnoresWallClockSteps(t *testing.T) {
	t.Parallel()
	h := shiftHerdr(t)
	shiftCaller(h, "w14:p0")
	o := shiftOpts(t, h)
	o.Poll = time.Hour
	state := paths(t, o.StateDir, h.fakeHerdr)
	require.NoError(t, os.MkdirAll(state.Dir, 0o700))
	st, err := store.Open(t.Context(), state.DB)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	var wall, mono atomic.Int64
	var armed atomic.Bool
	o.Mono = func() time.Duration { return time.Duration(mono.Load()) }
	o.Now = func() time.Time {
		if wall.Load() == 0 && armed.Load() && strings.Contains(string(debug.Stack()), "bossSnapshotOp") {
			wall.Store(int64(100 * time.Millisecond)) // as_of is sampled here, then the clock steps back 50 ms and the read ends 10 ms on
			return boardNow.Add(100 * time.Millisecond)
		} else if wall.Load() == int64(100*time.Millisecond) {
			wall.Store(int64(110 * time.Millisecond))
			mono.Store(int64(60 * time.Millisecond))
			_, err := st.Post(t.Context(), store.BoardPost{Project: "alpha", Channel: store.ChannelTeam, Author: store.Address{Kind: store.KindWorker, ID: "ha-coder-1"}, Kind: "post", Body: "after the read", At: boardNow.Add(80 * time.Millisecond)})
			require.NoError(t, err)
		}
		return boardNow.Add(time.Duration(wall.Load()))
	}
	sock := startShift(t, h, o)
	shiftRead(t, sock)
	armed.Store(true)
	assert.Equal(t, snapshot.Unavailable, shiftRead(t, sock).Board.State, "a post stamped before as_of was committed after the read")
	wall.Store(int64(4 * o.Poll))
	mono.Store(int64(4 * o.Poll))
	assert.Equal(t, snapshot.Stale, shiftRead(t, sock).TeamPoll.State)
	wall.Store(int64(o.Poll)) // a correction of three intervals back: the rows are no younger
	assert.Equal(t, snapshot.Stale, shiftRead(t, sock).TeamPoll.State)
}

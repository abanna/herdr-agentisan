package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

var boardNow = time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC)

func boardStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func boardPost(t *testing.T, st *store.Store, project, body string) store.BoardPost {
	t.Helper()
	p, err := st.Post(t.Context(), store.BoardPost{Project: project, Channel: store.ChannelTeam, Author: store.Address{Kind: store.KindWorker, ID: "ha-coder-1"}, Kind: "post", Body: body, At: boardNow})
	require.NoError(t, err)
	return p
}

// as places every caller as who.
func as(who daemon.BoardCaller) func(context.Context, daemon.Peer) (daemon.BoardCaller, error) {
	return func(context.Context, daemon.Peer) (daemon.BoardCaller, error) { return who, nil }
}

var (
	theBoss = daemon.BoardCaller{Project: "ha", Reader: store.ReaderBoss, Pane: "w1:pB"}
	theUser = daemon.BoardCaller{Project: "ha", Reader: store.ReaderUser, Pane: "w1:pD"}
)

func boardOp[T any](t *testing.T, socket, op string, args any) (T, error) {
	t.Helper()
	var out T
	data, err := daemon.Workforce(t.Context(), socket, op, args)
	if err != nil {
		return out, err
	}
	require.NoError(t, json.Unmarshal(data, &out))
	return out, nil
}

func refusedWith(t *testing.T, err error) string {
	t.Helper()
	var re *daemon.RequestError
	require.ErrorAs(t, err, &re)
	return re.Code
}

// TestBoardOpsPlaceTheCallerByTheKernel (A60): a board op is told its caller
// as the kernel knows it, the process that connected, never by its request.
func TestBoardOpsPlaceTheCallerByTheKernel(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no reader of a connection's peer on " + runtime.GOOS)
	}
	var (
		mu   sync.Mutex
		seen []daemon.Peer
	)
	socket, _, _ := daemon.ServeBoard(t, shortDir(t), boardStore(t), func(_ context.Context, p daemon.Peer) (daemon.BoardCaller, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, p)
		return theBoss, nil
	})
	_, err := boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, daemon.BoardReadRequest{})
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 1)
	assert.Equal(t, daemon.Peer{PID: os.Getpid(), UID: uint32(os.Getuid()), Known: true}, seen[0]) //nolint:gosec // G115: a test's uid fits
}

// TestBoardRead (A60): the boss's read is its unread posts, oldest first,
// moving its mark; --all is the newest, newest first, moving none; a caller
// placed nowhere is refused, and a bad limit too.
func TestBoardRead(t *testing.T) {
	t.Parallel()
	st := boardStore(t)
	for _, b := range []string{"one", "two", "three"} {
		boardPost(t, st, "ha", b)
	}
	boardPost(t, st, "web", "not ha's")
	socket, _, _ := daemon.ServeBoard(t, shortDir(t), st, as(theBoss))

	got, err := boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, daemon.BoardReadRequest{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, "ha", got.Project)
	assert.Equal(t, store.ReaderBoss, got.Reader)
	require.Len(t, got.Posts, 2)
	assert.Equal(t, "one", got.Posts[0].Body)
	assert.Equal(t, daemon.BoardPostView{Seq: got.Posts[1].Seq, ID: got.Posts[1].ID, Channel: "team", AuthorKind: "worker", Author: "ha-coder-1", Kind: "post", Body: "two", At: boardNow}, got.Posts[1])
	assert.Equal(t, 1, got.More)
	assert.Equal(t, got.Posts[1].Seq, got.Mark)
	assert.Equal(t, got.Posts[1].Seq+1, got.Newest)

	got, err = boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, daemon.BoardReadRequest{})
	require.NoError(t, err)
	assert.Len(t, got.Posts, 1, "the default limit takes the rest")
	assert.Equal(t, got.Newest, got.Mark)

	all, err := boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, daemon.BoardReadRequest{All: true, Limit: 2})
	require.NoError(t, err)
	require.Len(t, all.Posts, 2)
	assert.Equal(t, []string{"three", "two"}, []string{all.Posts[0].Body, all.Posts[1].Body}, "newest first")
	user, err := st.BoardStatus(t.Context(), "ha", store.ReaderUser)
	require.NoError(t, err)
	assert.Equal(t, 3, user.Unread, "the boss's reads leave the user's mark")

	_, err = boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, daemon.BoardReadRequest{Limit: store.MaxBoardRead + 1})
	assert.Equal(t, "bad_request", refusedWith(t, err))
	_, err = boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, map[string]any{"reader": "boss"})
	assert.Equal(t, "bad_request", refusedWith(t, err), "the request cannot say who reads")

	refused, _, _ := daemon.ServeBoard(t, shortDir(t), st, func(context.Context, daemon.Peer) (daemon.BoardCaller, error) {
		return daemon.BoardCaller{}, daemon.ErrNotAllowed
	})
	_, err = boardOp[daemon.BoardReadResult](t, refused, daemon.OpBoardRead, daemon.BoardReadRequest{})
	assert.Equal(t, "not_allowed", refusedWith(t, err))
	cannot, _, _ := daemon.ServeBoard(t, shortDir(t), st, func(context.Context, daemon.Peer) (daemon.BoardCaller, error) {
		return daemon.BoardCaller{}, daemon.ErrCannotPlace
	})
	_, err = boardOp[daemon.BoardReadResult](t, cannot, daemon.OpBoardRead, daemon.BoardReadRequest{})
	assert.Equal(t, "herdr_failed", refusedWith(t, err))
}

// TestBoardWait (A60): a wait answers at once when a post is past its mark,
// else at the next post, whoever makes it through the store; at its
// timeout it says so; a later wait from the same pane ends it; a bad
// timeout or after is refused.
func TestBoardWait(t *testing.T) {
	t.Parallel()
	st := boardStore(t)
	socket, _, waiting := daemon.ServeBoard(t, shortDir(t), st, as(theBoss))
	inForce := func() bool { return waiting(theBoss.Pane) }

	type answer struct {
		res daemon.BoardStateResult
		err error
	}
	waitIn := func(req daemon.BoardWaitRequest) <-chan answer {
		ch := make(chan answer, 1)
		go func() {
			var a answer
			a.res, a.err = boardOp[daemon.BoardStateResult](t, socket, daemon.OpBoardWait, req)
			ch <- a
		}()
		return ch
	}

	blocked := waitIn(daemon.BoardWaitRequest{Timeout: time.Minute})
	require.Eventually(t, inForce, 10*time.Second, 5*time.Millisecond, "the wait is in force")
	select {
	case a := <-blocked:
		t.Fatalf("an empty board answered a wait: %+v", a)
	case <-time.After(200 * time.Millisecond):
	}
	boardPost(t, st, "web", "another board's")
	select {
	case a := <-blocked:
		t.Fatalf("another team's post answered the wait: %+v", a)
	case <-time.After(200 * time.Millisecond):
	}
	p := boardPost(t, st, "ha", "wake")
	select {
	case a := <-blocked:
		require.NoError(t, a.err)
		assert.Equal(t, daemon.BoardStateResult{Project: "ha", Reader: store.ReaderBoss, Newest: p.Seq, Unread: 1}, a.res)
	case <-time.After(10 * time.Second):
		t.Fatal("a post did not wake the wait")
	}

	got, err := boardOp[daemon.BoardStateResult](t, socket, daemon.OpBoardWait, daemon.BoardWaitRequest{Timeout: time.Minute})
	require.NoError(t, err, "an unread post answers at once")
	assert.Equal(t, 1, got.Unread)
	_, err = st.ReadBoard(t.Context(), "ha", store.ReaderBoss, 10)
	require.NoError(t, err)
	got, err = boardOp[daemon.BoardStateResult](t, socket, daemon.OpBoardWait, daemon.BoardWaitRequest{Timeout: 50 * time.Millisecond})
	require.NoError(t, err)
	assert.True(t, got.TimedOut, "nothing past the mark: the timeout")
	zero := int64(0)
	got, err = boardOp[daemon.BoardStateResult](t, socket, daemon.OpBoardWait, daemon.BoardWaitRequest{After: &zero, Timeout: time.Minute})
	require.NoError(t, err, "past an after behind the mark: at once")
	assert.False(t, got.TimedOut)

	require.Eventually(t, func() bool { return !inForce() }, 10*time.Second, 5*time.Millisecond, "every earlier wait has answered")
	first := waitIn(daemon.BoardWaitRequest{Timeout: time.Minute})
	require.Eventually(t, inForce, 10*time.Second, 5*time.Millisecond, "the first wait is in force before the second starts")
	second := waitIn(daemon.BoardWaitRequest{Timeout: time.Minute})
	select {
	case a := <-first:
		assert.Equal(t, "superseded", refusedWith(t, a.err))
	case <-time.After(10 * time.Second):
		t.Fatal("a second wait from the pane did not end the first")
	}
	boardPost(t, st, "ha", "for the second")
	select {
	case a := <-second:
		require.NoError(t, a.err)
	case <-time.After(10 * time.Second):
		t.Fatal("the second wait was not woken")
	}

	for _, req := range []daemon.BoardWaitRequest{{Timeout: daemon.MaxBoardWait + time.Second}, {Timeout: -time.Second}, {After: new(int64(-1))}} {
		_, err = boardOp[daemon.BoardStateResult](t, socket, daemon.OpBoardWait, req)
		assert.Equal(t, "bad_request", refusedWith(t, err), "%+v", req)
	}
}

// TestBoardWaitEndsWhenTheDaemonStops (A60): a stop ends a blocked wait at
// once, told daemon_gone, and the stop does not wait out its timeout.
func TestBoardWaitEndsWhenTheDaemonStops(t *testing.T) {
	t.Parallel()
	socket, stop, waiting := daemon.ServeBoard(t, shortDir(t), boardStore(t), as(theBoss))
	done := make(chan error, 1)
	go func() {
		_, err := daemon.Workforce(context.Background(), socket, daemon.OpBoardWait, daemon.BoardWaitRequest{Timeout: daemon.MaxBoardWait})
		done <- err
	}()
	require.Eventually(t, func() bool { return waiting(theBoss.Pane) }, 10*time.Second, 5*time.Millisecond, "the wait is in force before the stop")
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop waited on the board wait")
	}
	select {
	case err := <-done:
		assert.Equal(t, "daemon_gone", refusedWith(t, err))
	case <-time.After(10 * time.Second):
		t.Fatal("the wait was not ended")
	}
}

// TestBoardBudgets: each board op's budget, which both the server and its
// client read; a wait gets its timeout and an exchange.
func TestBoardBudgets(t *testing.T) {
	t.Parallel()
	assert.Equal(t, daemon.BoardBudget, daemon.OpBudget(daemon.OpBoardRead, nil))
	assert.Equal(t, daemon.DefaultBoardWait+daemon.IOTimeout, daemon.OpBudget(daemon.OpBoardWait, nil))
	assert.Equal(t, 90*time.Second+daemon.IOTimeout, daemon.OpBudget(daemon.OpBoardWait, json.RawMessage(`{"timeout":90000000000}`)))
	assert.Equal(t, daemon.DefaultBoardWait+daemon.IOTimeout, daemon.OpBudget(daemon.OpBoardWait, json.RawMessage(`{"timeout":-1}`)), "a refused timeout gets the default's")
}

// paneOf is pane.list's entry for a pane in tab.
func paneOf(id, tab string) map[string]any {
	return map[string]any{"pane_id": id, "workspace_id": "w1", "tab_id": tab, "terminal_id": "term_" + id}
}

// placeHerdr is a herdr whose panes run the processes procs gives them: a
// shell pid and foreground argv by pane. failing fails every process read.
func placeHerdr(t *testing.T, failing bool) herdr.Client {
	t.Helper()
	panes := []map[string]any{paneOf("w1:pB", "t1"), paneOf("w1:pD", "t1"), paneOf("w1:pE", "t1"), paneOf("w1:pF", "t1"), paneOf("w1:p2", "t2"), paneOf("w1:pX", "t2"), paneOf("w1:pS", "t3")}
	shells := map[string]uint32{"w1:pB": 100, "w1:pD": 200, "w1:pE": 500, "w1:pF": 600, "w1:p2": 300, "w1:pX": 400, "w1:pS": 700}
	dashboard := []string{"bin/herdr-agentisan", "dashboard"}
	// pE runs another of the plugin's commands, pF another program's
	// dashboard: neither is the plugin's dashboard.
	argv := map[string][]string{"w1:pD": dashboard, "w1:pE": {"/opt/bin/herdr-agentisan", "board", "read"}, "w1:pF": {"other-tool", "dashboard"}, "w1:pX": dashboard, "w1:pS": dashboard}
	srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
		switch r.Method {
		case "pane.list":
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": panes}}
		case "pane.process_info":
			if failing {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
			}
			var params struct {
				PaneID string `json:"pane_id"`
			}
			_ = json.Unmarshal(r.Params, &params)
			procs := []map[string]any{}
			if a, ok := argv[params.PaneID]; ok {
				procs = append(procs, map[string]any{"pid": shells[params.PaneID], "name": a[0], "argv": a})
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{
				"pane_id": params.PaneID, "shell_pid": shells[params.PaneID], "foreground_process_group_id": 0, "foreground_processes": procs,
			}}}
		}
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "unknown_method", Message: r.Method}}
	})
	return herdr.Client{SocketPath: srv.Path}
}

// boardProcs is a process table: each pid's parent and start, a parent always
// starting first.
var boardProcs = map[int]report.Stat{
	100: {PPID: 1, Start: 10}, 101: {PPID: 100, Start: 50},
	200: {PPID: 1, Start: 20},
	300: {PPID: 1, Start: 30}, 301: {PPID: 300, Start: 70},
	400: {PPID: 1, Start: 40}, 401: {PPID: 400, Start: 80},
	500: {PPID: 1, Start: 45}, 501: {PPID: 500, Start: 85},
	600: {PPID: 1, Start: 46}, 601: {PPID: 600, Start: 86},
	700: {PPID: 1, Start: 47},
	999: {PPID: 1, Start: 90},
}

func boardStat(pid int) (report.Stat, error) {
	s, ok := boardProcs[pid]
	if !ok {
		return report.Stat{}, errors.New("no such process")
	}
	return s, nil
}

// TestPlaceBoardCaller (A60): a caller whose process runs in its team's
// boss place reads as the boss; one in the dashboard in the boss's tab as
// the user; any other is not allowed, a worker, an orphan, a dashboard in a
// worker's tab, another program in the boss's tab, another user, or one the
// kernel told nothing of, and the check fails closed.
func TestPlaceBoardCaller(t *testing.T) {
	t.Parallel()
	model := &team.Model{Projects: []team.Project{{
		Name: "ha",
		// A ◆ boss space holds its dashboard among the boss's panes: w1:pS,
		// in a tab of its own.
		Boss: []team.Worker{
			{ID: "ha-boss", Pane: team.Pane{ID: "w1:pB", TabID: "t1", Name: "ha-boss"}},
			{ID: "w1:pS", Pane: team.Pane{ID: "w1:pS", TabID: "t3"}},
		},
		Groups: []team.Group{{Name: "coder", Workers: []team.Worker{{ID: "w1:p2", Pane: team.Pane{ID: "w1:p2", TabID: "t2"}}}}},
	}}}
	view := func() (*team.Model, map[string]string) { return model, map[string]string{"ha": "ha-boss"} }
	uid := os.Getuid()
	place := daemon.PlaceBoardCaller(view, placeHerdr(t, false), boardStat, uid)
	me := func(pid int) daemon.Peer { return daemon.Peer{PID: pid, UID: uint32(uid), Known: true} } //nolint:gosec // G115: a test's uid fits

	tests := map[string]struct {
		peer daemon.Peer
		want daemon.BoardCaller
	}{
		"a command the boss runs":       {me(101), theBoss},
		"the dashboard in the boss tab": {me(200), daemon.BoardCaller{Project: "ha", Reader: store.ReaderUser, Pane: "w1:pD"}},
		"a dashboard in the boss place": {me(700), daemon.BoardCaller{Project: "ha", Reader: store.ReaderUser, Pane: "w1:pS"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := place(t.Context(), tc.peer)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	refused := map[string]daemon.Peer{
		"a worker":                      me(301),
		"a dashboard in a worker's tab": me(401),
		"another of the plugin's commands in the boss's tab": me(501),
		"another program's dashboard in the boss's tab":      me(601),
		"an orphan":                      me(999),
		"a process that is gone":         me(12345),
		"another user":                   {PID: 101, UID: uint32(uid) + 1, Known: true}, //nolint:gosec // G115: a test's uid fits
		"one the kernel told nothing of": {PID: 101, UID: uint32(uid)},                  //nolint:gosec // G115: a test's uid fits
	}
	for name, peer := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := place(t.Context(), peer)
			require.ErrorIs(t, err, daemon.ErrNotAllowed)
		})
	}

	_, err := daemon.PlaceBoardCaller(view, placeHerdr(t, false), nil, uid)(t.Context(), me(101))
	require.ErrorIs(t, err, daemon.ErrNotAllowed, "no process table to read")
	_, err = daemon.PlaceBoardCaller(func() (*team.Model, map[string]string) { return nil, nil }, placeHerdr(t, false), boardStat, uid)(t.Context(), me(101))
	require.ErrorIs(t, err, daemon.ErrNotReady, "before the first poll's model")
	_, err = daemon.PlaceBoardCaller(view, placeHerdr(t, true), boardStat, uid)(t.Context(), me(101))
	require.ErrorIs(t, err, daemon.ErrCannotPlace, "herdr failing is not a refusal of the caller")
}

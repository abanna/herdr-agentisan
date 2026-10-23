package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/preflight"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// workforceConfig is a config dir with one tabs team, ha, whose repo carries
// the Agentisan bundle.
func workforceConfig(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	marker := filepath.Join(repo, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
	return configDir(t, fmt.Sprintf("[projects.ha]\nrepo = %q\nshort = \"ha\"\nlayout = \"tabs\"\n[projects.ha.groups.coder]\nworkers = 1\nslots = 2\n", repo)+quietNotices)
}

// withTeam adds the ha team's workspace, with its boss's tab and one coder,
// to th's layout.
func withTeam(th *teamHerdr) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.workspaces = append(th.workspaces, herdr.WorkspaceInfo{WorkspaceID: "w14", Label: "ha"})
	th.tabs = append(th.tabs,
		map[string]any{"tab_id": "w14:t1", "workspace_id": "w14", "label": "ha-boss"},
		map[string]any{"tab_id": "w14:t2", "workspace_id": "w14", "label": "ha-coder-1"})
	th.panes = append(th.panes,
		map[string]any{"pane_id": "w14:p1", "terminal_id": "term_w14:p1", "workspace_id": "w14", "tab_id": "w14:t1", "agent": "claude", "agent_status": "working"},
		map[string]any{"pane_id": "w14:p2", "terminal_id": "term_w14:p2", "workspace_id": "w14", "tab_id": "w14:t2", "agent": "claude", "agent_status": "idle", "tokens": map[string]string{"ctx": "31"}})
}

// TestWorkforceOpsOverTheSocket: the workforce ops answer over the daemon
// socket with their result as data, and each refusal with its code.
func TestWorkforceOpsOverTheSocket(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	stateDir := shortDir(t)
	r := run(t, teamOpts(t, stateDir, th, workforceConfig(t)))
	healthy(t, stateDir, th.fakeHerdr)
	sock := socketPath(t, stateDir, th.path)

	data, err := daemon.Workforce(t.Context(), sock, daemon.OpTeamStatus, workforce.StatusRequest{Selector: workforce.Selector{Project: "ha"}})
	require.NoError(t, err)
	var st workforce.StatusResult
	require.NoError(t, json.Unmarshal(data, &st))
	assert.Equal(t, "ha", st.Project)
	require.Len(t, st.Groups, 1)
	assert.Equal(t, "coder", st.Groups[0].Name)
	assert.Equal(t, 1, st.Groups[0].Occupied)
	require.Len(t, st.Groups[0].Members, 1)
	assert.Equal(t, "ha-coder-1", st.Groups[0].Members[0].ID)
	require.NotNil(t, st.Groups[0].Members[0].Ctx)
	assert.Equal(t, 31, *st.Groups[0].Members[0].Ctx)
	require.Eventually(t, func() bool {
		data, err := daemon.Workforce(t.Context(), sock, daemon.OpTeamStatus, workforce.StatusRequest{Selector: workforce.Selector{Project: "ha"}})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &st))
		return st.Groups[0].Members[0].IdleS != nil
	}, ready, 20*time.Millisecond, "the team poll times the idle coder's rest, and status reads it")

	data, err = daemon.CallWith(t.Context(), sock, daemon.OpTeamStatus, nil, 10*time.Second)
	require.NoError(t, err, "a request with no args asks with no fields: the only tabs team's status")
	require.NoError(t, json.Unmarshal(data, &st))
	assert.Equal(t, "ha", st.Project)

	tests := map[string]struct {
		op   string
		args any
		code string
	}{
		"an unknown project":          {op: daemon.OpTeamStatus, args: map[string]any{"project": "nope"}, code: "unknown_project"},
		"an unknown arg":              {op: daemon.OpTeamStatus, args: map[string]any{"projekt": "ha"}, code: "bad_request"},
		"args that are no obj":        {op: daemon.OpTeamStatus, args: []int{1}, code: "bad_request"},
		"a caller pane not a string":  {op: daemon.OpTeamStatus, args: map[string]any{"caller_pane": 7}, code: "bad_request"},
		"a count below 1":             {op: daemon.OpWorkerSpawn, args: map[string]any{"group": "coder"}, code: "bad_request"},
		"a count past int64":          {op: daemon.OpWorkerSpawn, args: json.RawMessage(`{"group":"coder","count":99999999999999999999}`), code: "bad_request"},
		"past the slot cap":           {op: daemon.OpWorkerSpawn, args: map[string]any{"group": "coder", "count": 2}, code: "slot_cap"},
		"an unknown group":            {op: daemon.OpWorkerSpawn, args: map[string]any{"group": "qa", "count": 1}, code: "unknown_group"},
		"spawn takes no --set":        {op: daemon.OpWorkerSpawn, args: map[string]any{"group": "coder", "count": 1, "set": []string{"routing.bindings.claude_mid.model_id=claude-fable-5-1"}}, code: "bad_request"},
		"spawn args not UTF-8":        {op: daemon.OpWorkerSpawn, args: json.RawMessage("{\"group\":\"coder\",\"count\":1,\"task\":{\"task_lookup_status\":\"found\",\"worker_assignment\":\"\xff\"}}"), code: "bad_request"},
		"spawn args with a surrogate": {op: daemon.OpWorkerSpawn, args: json.RawMessage("{\"group\":\"coder\",\"count\":1,\"task\":{\"task_lookup_status\":\"found\",\"worker_assignment\":\"\xed\xa0\x80\"}}"), code: "bad_request"},
		"team up, an unknown project": {op: daemon.OpTeamUp, args: map[string]any{"project": "nope"}, code: "unknown_project"},
		"team up, an unknown arg":     {op: daemon.OpTeamUp, args: map[string]any{"group": "coder"}, code: "bad_request"},
		"retire with no name":         {op: daemon.OpWorkerRetire, args: map[string]any{}, code: "bad_request"},
		"retire the boss":             {op: daemon.OpWorkerRetire, args: map[string]any{"target": "w14:p1"}, code: "not_a_worker"},
		"retire nobody":               {op: daemon.OpWorkerRetire, args: map[string]any{"target": "ha-coder-9"}, code: "unknown_worker"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := daemon.Workforce(t.Context(), sock, tc.op, tc.args)
			var re *daemon.RequestError
			require.ErrorAs(t, err, &re)
			assert.Equal(t, tc.code, re.Code)
			assert.NotEmpty(t, re.Message)
		})
	}
	assert.True(t, r.alive())
}

// TestHandoffReadyOverTheSocket: handoff ready marks a worker's agent pane
// over the daemon socket, and each mark it changes, set or cleared, is the
// worker's handoff event; a mark that changes nothing is written again and
// is no event. Its refusals answer with their codes.
func TestHandoffReadyOverTheSocket(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	events, rec := telemetrytest.NewEvents()
	o := teamOpts(t, shortDir(t), th, workforceConfig(t))
	o.Team.Events = events
	r := run(t, o)
	healthy(t, o.StateDir, th.fakeHerdr)
	sock := socketPath(t, o.StateDir, th.path)
	mark := func(req workforce.HandoffReadyRequest) workforce.HandoffReadyResult {
		t.Helper()
		data, err := daemon.Workforce(t.Context(), sock, daemon.OpHandoffReady, req)
		require.NoError(t, err)
		var res workforce.HandoffReadyResult
		require.NoError(t, json.Unmarshal(data, &res))
		return res
	}

	coder := workforce.HandoffReadyResult{ID: "w14:p2", Project: "ha", Group: "coder", PaneID: "w14:p2", Ready: true, Changed: true}
	assert.Equal(t, coder, mark(workforce.HandoffReadyRequest{Target: "ha-coder-1"}))
	th.mu.Lock()
	for _, p := range th.panes {
		if p["pane_id"] == "w14:p2" {
			p["tokens"] = map[string]string{"ctx": "31", "handoff": "⟳ READY TO RESTART"}
		}
	}
	th.mu.Unlock()
	coder.Changed = false
	assert.Equal(t, coder, mark(workforce.HandoffReadyRequest{Target: "w14:p2"}), "the pane shows the mark already")
	coder.Ready, coder.Changed = false, true
	assert.Equal(t, coder, mark(workforce.HandoffReadyRequest{Target: "w14:p2", Clear: true}))

	handoffs := func() []telemetrytest.Event {
		var out []telemetrytest.Event
		for _, e := range rec.Events() {
			if e.Name == telemetry.EventHandoff {
				out = append(out, e)
			}
		}
		return out
	}
	attrs := func(step string) map[string]any {
		return map[string]any{
			"agentisan.worker.id": "w14:p2", "agentisan.worker.bound": false, "agentisan.project": "ha",
			"agentisan.group": "coder", "herdr.pane.id": "w14:p2", "agentisan.handoff.step": step,
		}
	}
	assert.Equal(t, []telemetrytest.Event{
		{Name: telemetry.EventHandoff, Attrs: attrs("ready")},
		{Name: telemetry.EventHandoff, Attrs: attrs("cleared")},
	}, handoffs(), "one event per changed mark, none for the repeat")
	var marks []string
	for _, req := range th.srv.Requests() {
		var p struct {
			PaneID string            `json:"pane_id"`
			Tokens map[string]string `json:"tokens"`
		}
		require.NoError(t, json.Unmarshal(req.Params, &p))
		if v, ok := p.Tokens["handoff"]; req.Method == "pane.report_metadata" && ok {
			marks = append(marks, p.PaneID+"="+v)
		}
	}
	assert.Equal(t, []string{"w14:p2=⟳ READY TO RESTART", "w14:p2=⟳ READY TO RESTART", "w14:p2="}, marks)

	for name, tc := range map[string]struct {
		args any
		code string
	}{
		"the boss":                {map[string]any{"target": "w14:p1"}, "not_a_worker"},
		"nobody":                  {map[string]any{"target": "ha-coder-9"}, "unknown_worker"},
		"no target and no pane":   {map[string]any{}, "bad_request"},
		"a target not a string":   {map[string]any{"target": 7}, "bad_request"},
		"an arg it does not know": {map[string]any{"target": "ha-coder-1", "project": "ha"}, "bad_request"},
	} {
		_, err := daemon.Workforce(t.Context(), sock, daemon.OpHandoffReady, tc.args)
		var re *daemon.RequestError
		require.ErrorAs(t, err, &re, name)
		assert.Equal(t, tc.code, re.Code, name)
		assert.NotEmpty(t, re.Message, name)
	}
	assert.Len(t, handoffs(), 2, "refusals add no event")
	assert.True(t, r.alive())
}

// TestTheIdleClockRunsWithoutTheAgentList: a pane's rest is pane.list's
// status, so the poll times it while agent.list fails, and status, asked
// once agent.list answers again, reports the whole rest.
func TestTheIdleClockRunsWithoutTheAgentList(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	th.mu.Lock()
	th.agentsErr = true
	th.mu.Unlock()
	t0 := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	o := teamOpts(t, shortDir(t), th, workforceConfig(t))
	o.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	run(t, o)
	healthy(t, o.StateDir, th.fakeHerdr)
	polled(t, th.srv, 2)

	clock.Store(t0.Add(90 * time.Second).UnixNano())
	th.mu.Lock()
	th.agentsErr = false
	th.mu.Unlock()
	data, err := daemon.Workforce(t.Context(), socketPath(t, o.StateDir, th.path), daemon.OpTeamStatus, workforce.StatusRequest{})
	require.NoError(t, err)
	var st workforce.StatusResult
	require.NoError(t, json.Unmarshal(data, &st))
	require.Len(t, st.Groups, 1)
	require.Len(t, st.Groups[0].Members, 1)
	require.NotNil(t, st.Groups[0].Members[0].IdleS, "timed while agent.list failed")
	assert.Equal(t, int64(90), *st.Groups[0].Members[0].IdleS)
}

// TestWorkforceOpsNeedTheTeamPoll: a daemon run without team options has no
// workforce ops.
func TestWorkforceOpsNeedTheTeamPoll(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	stateDir := shortDir(t)
	run(t, opts(t, stateDir, h))
	healthy(t, stateDir, h)

	_, err := daemon.Workforce(t.Context(), socketPath(t, stateDir, h.path), daemon.OpTeamStatus, workforce.StatusRequest{})
	var re *daemon.RequestError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "unknown_op", re.Code)
}

// TestTheDaemonsWorkforce: the workforce service a daemon builds runs on its
// configuration, idle clock and clock, and dials only the herdr server the
// daemon belongs to: once the socket names another, every call is
// ErrHerdrGone.
func TestTheDaemonsWorkforce(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	o := teamOpts(t, shortDir(t), th, "/config")
	at := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return at }
	ident, err := daemon.HerdrIdentity(th.path)
	require.NoError(t, err)

	svc := daemon.WorkforceOf(o, ident)
	assert.Equal(t, "/config", svc.ConfigDir)
	assert.NotNil(t, svc.Idle)
	require.NotNil(t, svc.Now)
	assert.Equal(t, at, svc.Now())
	_, err = svc.Herdr.ListPanes(t.Context())
	require.NoError(t, err, "its own server")

	th.restart()
	_, err = svc.Herdr.ListPanes(t.Context())
	require.ErrorIs(t, err, daemon.ErrHerdrGone, "the socket's replacement is not its server")
}

// TestAnOpGetsItsBudget: an op longer than the 5 s exchange answers within
// its own budget, and still finishes when its client has gone.
func TestAnOpGetsItsBudget(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortDir(t), "d.sock")
	stop, err := daemon.ServeOp(t.Context(), sock, "slow", "failed", 8*time.Second, func(ctx context.Context) (any, error) {
		select {
		case <-time.After(5500 * time.Millisecond):
			return map[string]string{"done": "yes"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	require.NoError(t, err)
	t.Cleanup(stop)

	data, err := daemon.CallWith(t.Context(), sock, "slow", json.RawMessage(`{}`), 10*time.Second)
	require.NoError(t, err)
	assert.JSONEq(t, `{"done":"yes"}`, string(data))

	short := filepath.Join(shortDir(t), "e.sock")
	ended := make(chan error, 1)
	stopShort, err := daemon.ServeOp(t.Context(), short, "slow", "failed", 3*time.Second, func(ctx context.Context) (any, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			ended <- nil
			return nil, nil
		case <-ctx.Done():
			ended <- ctx.Err()
			return nil, ctx.Err()
		}
	})
	require.NoError(t, err)
	t.Cleanup(stopShort)
	_, err = daemon.CallWith(t.Context(), short, "slow", json.RawMessage(`{}`), 50*time.Millisecond)
	require.ErrorIs(t, err, daemon.ErrUnavailable, "the client gave up")
	select {
	case err := <-ended:
		require.NoError(t, err, "the op ran to its end, not cancelled with its client")
	case <-time.After(ready):
		t.Fatal("the op did not finish after its client went")
	}
}

// TestDaemonStopEndsAHandoffWait: a retire waiting out a handoff does not
// hold the daemon's stop, whether it is stopped or its herdr crashed (a
// crashed herdr leaves the wait polling a socket nothing answers): the op
// ends, refused, and Run returns.
func TestDaemonStopEndsAHandoffWait(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		stop    func(*running, *teamHerdr)
		wantErr error
	}{
		"the daemon is stopped": {stop: func(r *running, _ *teamHerdr) { r.cancel() }},
		"its herdr crashes":     {stop: func(_ *running, th *teamHerdr) { th.crash() }, wantErr: daemon.ErrHerdrGone},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			withTeam(th)
			stateDir := shortDir(t)
			r := run(t, teamOpts(t, stateDir, th, workforceConfig(t)))
			healthy(t, stateDir, th.fakeHerdr)
			poll := freezePoll(t, th)

			done := make(chan error, 1)
			go func() {
				_, err := daemon.Workforce(context.Background(), socketPath(t, stateDir, th.path), daemon.OpWorkerRetire, workforce.RetireRequest{Target: "ha-coder-1", AfterHandoff: true})
				done <- err
			}()
			// The op reads herdr's layout, agent.list last, then state.db's
			// workers rows, and then lists the panes: its handoff wait's
			// first look. A stop before that list fails a read (herdr_failed,
			// store_failed) rather than ending the wait, which has 1,500 s.
			require.Eventually(t, func() bool { return poll.askedAfter("agent.list", "pane.list") }, ready, 5*time.Millisecond,
				"the retire never reached its handoff wait")
			select {
			case err := <-done:
				t.Fatalf("the retire ended before the stop: %v", err)
			default:
			}
			poll.release() // a crash waits for every handler, a held push's too
			tc.stop(r, th)
			select {
			case err := <-r.done:
				r.done <- err // for run's cleanup, which waits for it too
				if tc.wantErr == nil {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, tc.wantErr)
				}
			case <-time.After(ready):
				t.Fatal("the daemon's stop waited for the handoff")
			}
			// Either stop answers handoff_timeout, and the answer always
			// lands: the wait polls an unreachable herdr again until the op's
			// context ends, and the server's close waits for every handler,
			// which writes its answer before it closes. An early stop answers
			// a read's herdr_failed or store_failed instead.
			select {
			case err := <-done:
				re, ok := errors.AsType[*daemon.RequestError](err)
				require.True(t, ok, "the client got no answer: %v", err)
				assert.Equal(t, "handoff_timeout", re.Code)
			case <-time.After(ready):
				t.Fatal("the client never heard back")
			}
		})
	}
}

// frozenPoll holds th's team poll at its pushes, which only the poll makes,
// so every herdr call made meanwhile is another caller's.
type frozenPoll struct {
	frozen chan struct{}
	// release lets the held pushes answer; the poll goes on.
	release func()
	mu      sync.Mutex
	asked   []string // the methods herdr was asked while the poll was held
}

// freezePoll holds th's team poll at its next push until release, and
// returns once it is held. A push follows the poll's reconcile, which opens
// the workforce ops, so they are open by then. Each held push holds the poll
// until its call's timeout, and a poll pushes to every team and agent pane,
// so it stays held for many seconds even unreleased. When the hook lands on
// a poll's last push, the poll goes on after one CallTimeout (5 s), and its
// own pane.list and agent.list are recorded from then: a test whose other
// caller takes longer than that to ask can read the poll's calls as its own.
func freezePoll(t *testing.T, th *teamHerdr) *frozenPoll {
	t.Helper()
	held := make(chan struct{})
	f := &frozenPoll{frozen: make(chan struct{}), release: sync.OnceFunc(func() { close(held) })}
	freeze := sync.OnceFunc(func() { close(f.frozen) })
	th.mu.Lock()
	th.onCall = func(method string) {
		if method == "workspace.report_metadata" || method == "pane.report_metadata" {
			freeze()
			<-held
			return
		}
		select {
		case <-f.frozen:
			f.mu.Lock()
			f.asked = append(f.asked, method)
			f.mu.Unlock()
		default:
		}
	}
	th.mu.Unlock()
	// After the fake herdr's own cleanup, so it runs first: herdrtest's Close
	// waits for every handler, a held push's too.
	t.Cleanup(f.release)
	select {
	case <-f.frozen:
	case <-time.After(ready):
		t.Fatal("the team poll never pushed")
	}
	return f
}

// askedAfter reports whether herdr was asked then after first while the
// poll was held.
func (f *frozenPoll) askedAfter(first, then string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.asked, first)
	return i >= 0 && slices.Contains(f.asked[i+1:], then)
}

// TestAClientWaitsOutItsOpsBudget: a client waits an op's own budget, past
// the 5 s exchange: a status whose four lists herdr answers slowly, each
// within its own call's 5 s, still gets its answer, and the daemon answers
// another request meanwhile.
func TestAClientWaitsOutItsOpsBudget(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	stateDir := shortDir(t)
	run(t, teamOpts(t, stateDir, th, workforceConfig(t)))
	healthy(t, stateDir, th.fakeHerdr)
	// The first reconcile opens the ops: a status before it is not_ready
	// for as long as that poll takes, which this test is not about (QA #77,
	// F-5).
	polled(t, th.srv, 1)
	var slow atomic.Bool
	th.mu.Lock()
	th.onCall = func(method string) {
		if strings.HasSuffix(method, ".list") && slow.Load() {
			time.Sleep(1500 * time.Millisecond)
		}
	}
	th.mu.Unlock()
	slow.Store(true)

	sock := socketPath(t, stateDir, th.path)
	type answer struct {
		data json.RawMessage
		err  error
	}
	status := make(chan answer, 1)
	began := time.Now()
	go func() {
		data, err := daemon.Workforce(t.Context(), sock, daemon.OpTeamStatus, workforce.StatusRequest{})
		status <- answer{data, err}
	}()
	time.Sleep(500 * time.Millisecond) // the status is waiting on herdr
	_, err := daemon.Health(t.Context(), sock)
	require.NoError(t, err, "another request is answered meanwhile")
	select {
	case <-status:
		t.Fatal("the status answered before herdr did")
	default:
	}

	got := <-status
	slow.Store(false)
	require.NoError(t, got.err)
	assert.Greater(t, time.Since(began), 5*time.Second, "past the exchange")
	var st workforce.StatusResult
	require.NoError(t, json.Unmarshal(got.data, &st))
	assert.Equal(t, "ha", st.Project)
}

// TestAnOpPastItsBudgetIsATimeout: an op whose own budget ends while a call
// fails answers timeout, saying what it waited on; a code that says more
// than that a call failed stands, and a deadline of an inner call, the
// budget still running, keeps the op's own code.
func TestAnOpPastItsBudgetIsATimeout(t *testing.T) {
	t.Parallel()
	waitThenFail := func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("list panes: %w", ctx.Err())
	}
	for name, tc := range map[string]struct {
		code   string
		budget time.Duration
		run    func(context.Context) (any, error)
		want   string
	}{
		"a call that failed":    {code: "herdr_failed", budget: 200 * time.Millisecond, run: waitThenFail, want: "timeout"},
		"an error with no code": {code: "failed", budget: 200 * time.Millisecond, run: waitThenFail, want: "timeout"},
		"a handoff not written": {code: "handoff_timeout", budget: 200 * time.Millisecond, run: waitThenFail, want: "handoff_timeout"},
		"a replaced herdr":      {code: "herdr_gone", budget: 200 * time.Millisecond, run: waitThenFail, want: "herdr_gone"},
		"an inner call's deadline": {code: "herdr_failed", budget: time.Minute, run: func(context.Context) (any, error) {
			return nil, fmt.Errorf("list panes: %w", context.DeadlineExceeded)
		}, want: "herdr_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sock := filepath.Join(shortDir(t), "d.sock")
			stop, err := daemon.ServeOp(t.Context(), sock, "op", tc.code, tc.budget, tc.run)
			require.NoError(t, err)
			t.Cleanup(stop)

			_, err = daemon.CallWith(t.Context(), sock, "op", json.RawMessage(`{}`), ready)
			var re *daemon.RequestError
			require.ErrorAs(t, err, &re)
			assert.Equal(t, tc.want, re.Code)
			assert.Contains(t, re.Message, "list panes")
			if tc.want == "timeout" {
				assert.Contains(t, re.Message, "op ran past its 200ms budget")
			}
		})
	}
}

// TestAnOpEndsWithTheDaemon: an op runs under the context the daemon's
// socket was given, so it ends when that ends, whatever its own budget.
func TestAnOpEndsWithTheDaemon(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortDir(t), "d.sock")
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	// Only the cancel can end the op in time; its budget, longer than the
	// wait below, still bounds a regression, which then fails, not hangs.
	stop, err := daemon.ServeOp(ctx, sock, "wait", "failed", 2*ready, func(ctx context.Context) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	require.NoError(t, err)
	t.Cleanup(stop)

	answered := make(chan error, 1)
	go func() {
		_, err := daemon.CallWith(t.Context(), sock, "wait", json.RawMessage(`{}`), ready)
		answered <- err
	}()
	<-started
	cancel()
	select {
	case err := <-answered:
		var re *daemon.RequestError
		require.ErrorAs(t, err, &re, "the op answers, refused")
		assert.Contains(t, re.Message, "context canceled")
	case <-time.After(ready):
		t.Fatal("the op outlived the daemon's context")
	}
}

// TestTheWorkforceGetsThePreflightHook: Hooks.Preflight is the check the
// daemon's spawns run (A34); with none, the service runs its default. The
// team's dashboard pane and Codex hooks check are the TeamOptions' own.
func TestTheWorkforceGetsThePreflightHook(t *testing.T) {
	t.Parallel()
	var checked string
	dash := workforce.DashboardPane{PluginID: "p", Entrypoint: "dashboard", Title: "Dashboard"}
	turnErr := errors.New("turn hooks checked")
	o := daemon.Options{Team: &daemon.TeamOptions{Dashboard: dash, CodexHooks: func() (bool, error) { return true, nil }, CodexTurnHooks: func() (bool, error) { return false, turnErr }}, Hooks: daemon.Hooks{Preflight: func(_ context.Context, repo string) error {
		checked = repo
		return nil
	}}}
	svc := daemon.WorkforceOf(o, daemon.Identity{})
	require.NoError(t, svc.Preflight(t.Context(), "/repo"))
	assert.Equal(t, "/repo", checked)
	assert.Equal(t, dash, svc.Dashboard)
	require.NotNil(t, svc.CodexHooks)
	installed, err := svc.CodexHooks()
	require.NoError(t, err)
	assert.True(t, installed)
	require.NotNil(t, svc.CodexTurnHooks)
	_, err = svc.CodexTurnHooks()
	require.ErrorIs(t, err, turnErr)
	assert.Nil(t, daemon.WorkforceOf(daemon.Options{Team: &daemon.TeamOptions{}}, daemon.Identity{}).Preflight)
}

// TestTheWorkforceGetsTheDebuggerLaunch: a debugger worker's launch names
// the plugin's checkout and binary `daemon run` was given, and the daemon's
// own directory for its herdr server, which holds daemon.log (A32).
func TestTheWorkforceGetsTheDebuggerLaunch(t *testing.T) {
	t.Parallel()
	o := daemon.Options{StateDir: "/state", HerdrSocket: "/run/herdr.sock", Team: &daemon.TeamOptions{PluginRoot: "/plugin", Binary: "/plugin/bin/herdr-agentisan"}}
	paths, err := daemon.PathsFor(o.StateDir, o.HerdrSocket)
	require.NoError(t, err)
	assert.Equal(t, debugger.Launch{Root: "/plugin", Bin: "/plugin/bin/herdr-agentisan", ServerDir: paths.Dir}, daemon.WorkforceOf(o, daemon.Identity{}).Debugger)
}

// TestRunGivesTheDebuggerItsServerDir: a running daemon's workforce gives a
// debugger the daemon's own directory for its herdr server, never the whole
// state dir, which holds state.db, nor none (A32). Its env file is put in
// that directory, which the launch refuses to fence, so the refusal names the
// directory the workforce was given.
func TestRunGivesTheDebuggerItsServerDir(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	stateDir := shortDir(t)
	paths, err := daemon.PathsFor(stateDir, th.path)
	require.NoError(t, err)
	envFile := filepath.Join(paths.Dir, "elastic.env")
	repo := t.TempDir()
	marker := filepath.Join(repo, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
	cfg := configDir(t, fmt.Sprintf("[debugger]\nenv_file = %q\n[projects.ha]\nrepo = %q\nshort = \"ha\"\nlayout = \"tabs\"\n[projects.ha.groups.debugger]\nworkers = 0\nslots = 1\n", envFile, repo))
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "roles"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, debugger.RoleFile), []byte("# debugger\n"), 0o600))
	o := teamOpts(t, stateDir, th, cfg)
	o.Team.PluginRoot, o.Team.Binary = root, filepath.Join(root, "bin", "herdr-agentisan")
	run(t, o)
	healthy(t, stateDir, th.fakeHerdr)
	sock := socketPath(t, stateDir, th.path)

	_, err = daemon.Workforce(t.Context(), sock, daemon.OpWorkerSpawn, workforce.SpawnRequest{Group: settings.DebuggerType, Count: 1})
	require.ErrorContains(t, err, "the server dir "+paths.Dir+" holds the trail's env file")
}

// TestTheWorkforceRecordsItsRouting: the daemon's service hands each routing
// decision, a spawn's or a team up group's, to the workflow events, one per
// worker, and to daemon.log whole, named for its op; a refusal while routing
// is off goes to daemon.log alone (A50).
func TestTheWorkforceRecordsItsRouting(t *testing.T) {
	t.Parallel()
	events, rec := telemetrytest.NewEvents()
	var logs strings.Builder
	o := daemon.Options{Team: &daemon.TeamOptions{Events: events}, Logger: zerolog.New(&logs)}
	svc := daemon.WorkforceOf(o, daemon.Identity{})
	require.NotNil(t, svc.Routed)
	d := routing.Decision{Role: "coder", Provider: "claude", SelectedBinding: "claude_mid", ModelID: "claude-sonnet-5-5", Effort: "high", Applied: true}

	svc.Routed(t.Context(), workforce.Routed{Op: "worker spawn", Project: "ha", Group: "coder", IDs: []string{"ha-coder-1", "ha-coder-2"}, Decision: &d})
	got := rec.Events()
	require.Len(t, got, 2)
	for i, id := range []string{"ha-coder-1", "ha-coder-2"} {
		assert.Equal(t, "agentisan.worker.routed", got[i].Name)
		assert.Equal(t, id, got[i].Attrs["agentisan.worker.id"])
		assert.Equal(t, "claude_mid", got[i].Attrs["agentisan.routing.selected_binding"])
	}
	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(logs.String()), &line))
	assert.Equal(t, "worker spawn routed", line["message"])
	assert.Equal(t, []any{"ha-coder-1", "ha-coder-2"}, line["workers"])
	routed, _ := line["routing"].(map[string]any)
	assert.Equal(t, "claude-sonnet-5-5", routed["model_id"])
	assert.Equal(t, true, routed["applied"])

	logs.Reset()
	svc.Routed(t.Context(), workforce.Routed{Op: "team up", Project: "ha", Group: "qa", IDs: []string{"ha-qa-1"}, Decision: &d})
	require.Len(t, rec.Events(), 3)
	require.NoError(t, json.Unmarshal([]byte(logs.String()), &line))
	assert.Equal(t, "team up routed", line["message"])

	logs.Reset()
	svc.Routed(t.Context(), workforce.Routed{Op: "worker spawn", Project: "ha", Group: "ops", IDs: []string{"ha-ops-1"}, Refused: routing.ErrUnknownRole})
	assert.Len(t, rec.Events(), 3, "no event without a decision")
	assert.Contains(t, logs.String(), "worker spawn: routing is off; when on, it would refuse this")
	assert.Contains(t, logs.String(), routing.ErrUnknownRole.Error())

	logs.Reset()
	svc.Routed(t.Context(), workforce.Routed{Op: "team up", Project: "ha", Group: "ops", IDs: []string{"ha-ops-1"}, Refused: routing.ErrUnknownRole})
	assert.Len(t, rec.Events(), 3, "no event without a decision")
	assert.Contains(t, logs.String(), "team up: routing is off; when on, it would refuse this")
	assert.NotContains(t, logs.String(), "worker spawn", "a team up refusal is named for its own op")
}

// TestWorkforceCodes: every workforce refusal has a code of its own, and
// D12 is told apart from any other config error.
func TestWorkforceCodes(t *testing.T) {
	t.Parallel()
	for err, code := range map[error]string{
		fmt.Errorf("%w: not reconciled", daemon.ErrNotReady):                     "not_ready",
		workforce.ErrBadRequest:                                                  "bad_request",
		workforce.ErrUnknownProject:                                              "unknown_project",
		fmt.Errorf("%w: %w", workforce.ErrConfig, settings.ErrBypassesAgentisan): "bypasses_agentisan",
		workforce.ErrConfig:                                                      "config_invalid",
		workforce.ErrUnknownWorker:                                               "unknown_worker",
		workforce.ErrNotAWorker:                                                  "not_a_worker",
		workforce.ErrPaneChanged:                                                 "pane_changed",
		workforce.ErrHandoffPending:                                              "handoff_pending",
		workforce.ErrHandoffTimeout:                                              "handoff_timeout",
		workforce.ErrNoTarget:                                                    "no_target",
		workforce.ErrUnknownGroup:                                                "unknown_group",
		routing.ErrUnknownRole:                                                   "unknown_role",
		routing.ErrProviderMismatch:                                              "provider_mismatch",
		routing.ErrOverrideRefused:                                               "override_refused",
		routing.ErrBlocked:                                                       "routing_blocked",
		fmt.Errorf("%w: %w", workforce.ErrBadRequest, routing.ErrInvalid):        "bad_request",
		workforce.ErrSlotCap:                                                     "slot_cap",
		workforce.ErrInvalidID:                                                   "invalid_id",
		fmt.Errorf("%w: %w", workforce.ErrPreflight, preflight.ErrTdaddyMissing): "preflight_failed",
		fmt.Errorf("%w: %w", workforce.ErrSpawnFailed, workforce.ErrHerdr):       "spawn_failed",
		fmt.Errorf("%w: %w", workforce.ErrHerdr, daemon.ErrHerdrGone):            "herdr_gone",
		workforce.ErrHerdr:          "herdr_failed",
		store.ErrStore:              "store_failed",
		errors.New("anything else"): "failed",
	} {
		assert.Equal(t, code, daemon.WorkforceCode(err), err.Error())
	}
}

// TestOpBudgets: team status, retire, send and handoff ready get 10 s, spawn
// 100 s, team up a spawn's, whatever that is, and retire --after-handoff
// 1,500 s, both in the client's wait and in the budget the daemon's op map
// enforces; any other op none of its own.
func TestOpBudgets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   string
		args string
		want time.Duration
	}{
		{daemon.OpTeamStatus, ``, 10 * time.Second},
		{daemon.OpWorkerSpawn, `{"group":"coder","count":1}`, 100 * time.Second},
		{daemon.OpTeamUp, ``, daemon.OpBudget(daemon.OpWorkerSpawn, nil)},
		{daemon.OpWorkerRetire, `{"target":"x"}`, 10 * time.Second},
		{daemon.OpWorkerRetire, `{"target":"x","after_handoff":true}`, 1500 * time.Second},
		{daemon.OpWorkerRetire, `not json`, 10 * time.Second},
		{daemon.OpSend, `{"to":"boss","body":"hi"}`, 10 * time.Second},
		{daemon.OpHandoffReady, `{"target":"x"}`, 10 * time.Second},
	} {
		args := json.RawMessage(tc.args)
		assert.Equal(t, tc.want, daemon.OpBudget(tc.op, args), "the client's wait: %s %s", tc.op, tc.args)
		assert.Equal(t, tc.want, daemon.ServedBudget(tc.op, args), "the budget the daemon enforces: %s %s", tc.op, tc.args)
	}
	assert.Zero(t, daemon.OpBudget("health", nil))
	assert.Equal(t, []string{daemon.OpHandoffReady, daemon.OpSend, daemon.OpTeamStatus, daemon.OpTeamUp, daemon.OpWorkerRetire, daemon.OpWorkerSpawn}, daemon.WorkforceOpNames())
	for _, op := range daemon.WorkforceOpNames() {
		assert.Positive(t, daemon.ServedBudget(op, nil), "%s has a budget in the table, not a nil one", op)
	}
}

// TestAWorkforceCommandFitsTheWrapper: the longest a workforce command
// waits, the not_ready window, its op's budget and an exchange (A52), is at
// most 115 s, 5 s inside the skill wrapper's 120, for every op the daemon
// serves, so an op added to the table is held to it too. A retire after a
// handoff has the wrapper's 1,800 s instead, and the same 5 s to spare.
func TestAWorkforceCommandFitsTheWrapper(t *testing.T) {
	t.Parallel()
	longest := func(op, args string) time.Duration {
		return daemon.NotReadyWindow + daemon.OpBudget(op, json.RawMessage(args)) + daemon.IOTimeout
	}
	assert.Equal(t, 10*time.Second, daemon.NotReadyWindow)
	assert.Equal(t, 5*time.Second, daemon.IOTimeout)
	for _, op := range daemon.WorkforceOpNames() {
		assert.LessOrEqual(t, longest(op, ""), 115*time.Second, "%s's window, budget and exchange", op)
	}
	assert.Equal(t, 115*time.Second, longest(daemon.OpWorkerSpawn, `{"group":"coder","count":1}`), "a spawn takes all of it")
	assert.Equal(t, 115*time.Second, longest(daemon.OpTeamUp, ""), "team up, with a spawn's budget, takes all of it too")
	assert.LessOrEqual(t, longest(daemon.OpWorkerRetire, `{"target":"x","after_handoff":true}`), 1795*time.Second, "a retire after a handoff")
}

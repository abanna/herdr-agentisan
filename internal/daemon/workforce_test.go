package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
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
	return configDir(t, fmt.Sprintf("[projects.ha]\nrepo = %q\nshort = \"ha\"\nlayout = \"tabs\"\n[projects.ha.groups.coder]\nworkers = 1\nslots = 2\n", repo))
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
		"an unknown project":         {op: daemon.OpTeamStatus, args: map[string]any{"project": "nope"}, code: "unknown_project"},
		"an unknown arg":             {op: daemon.OpTeamStatus, args: map[string]any{"projekt": "ha"}, code: "bad_request"},
		"args that are no obj":       {op: daemon.OpTeamStatus, args: []int{1}, code: "bad_request"},
		"a caller pane not a string": {op: daemon.OpTeamStatus, args: map[string]any{"caller_pane": 7}, code: "bad_request"},
		"retire with no name":        {op: daemon.OpWorkerRetire, args: map[string]any{}, code: "bad_request"},
		"retire the boss":            {op: daemon.OpWorkerRetire, args: map[string]any{"target": "w14:p1"}, code: "not_a_worker"},
		"retire nobody":              {op: daemon.OpWorkerRetire, args: map[string]any{"target": "ha-coder-9"}, code: "unknown_worker"},
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

			done := make(chan error, 1)
			go func() {
				_, err := daemon.Workforce(context.Background(), socketPath(t, stateDir, th.path), daemon.OpWorkerRetire, workforce.RetireRequest{Target: "ha-coder-1", AfterHandoff: true})
				done <- err
			}()
			// The op reads the config, herdr and state.db first; a second is
			// ample for that, and the handoff wait itself has 1,500 s.
			time.Sleep(time.Second)
			select {
			case err := <-done:
				t.Fatalf("the retire ended before the stop: %v", err)
			default:
			}
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
			select {
			case err := <-done:
				if re, ok := errors.AsType[*daemon.RequestError](err); ok {
					assert.Equal(t, "handoff_timeout", re.Code)
				} else {
					require.ErrorIs(t, err, daemon.ErrUnavailable)
				}
			case <-time.After(ready):
				t.Fatal("the client never heard back")
			}
		})
	}
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
		fmt.Errorf("%w: %w", workforce.ErrHerdr, daemon.ErrHerdrGone):            "herdr_gone",
		workforce.ErrHerdr:                                                       "herdr_failed",
		store.ErrStore:                                                           "store_failed",
		errors.New("anything else"):                                              "failed",
	} {
		assert.Equal(t, code, daemon.WorkforceCode(err), err.Error())
	}
}

// TestOpBudgets: team status, retire and send get 10 s, and retire
// --after-handoff 1,500 s, both in the client's wait and in the budget the
// daemon's op map enforces; any other op none of its own.
func TestOpBudgets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   string
		args string
		want time.Duration
	}{
		{daemon.OpTeamStatus, ``, 10 * time.Second},
		{daemon.OpWorkerRetire, `{"target":"x"}`, 10 * time.Second},
		{daemon.OpWorkerRetire, `{"target":"x","after_handoff":true}`, 1500 * time.Second},
		{daemon.OpWorkerRetire, `not json`, 10 * time.Second},
		{daemon.OpSend, `{"to":"boss","body":"hi"}`, 10 * time.Second},
	} {
		args := json.RawMessage(tc.args)
		assert.Equal(t, tc.want, daemon.OpBudget(tc.op, args), "the client's wait: %s %s", tc.op, tc.args)
		assert.Equal(t, tc.want, daemon.ServedBudget(tc.op, args), "the budget the daemon enforces: %s %s", tc.op, tc.args)
	}
	assert.Zero(t, daemon.OpBudget("health", nil))
	assert.Equal(t, []string{daemon.OpSend, daemon.OpTeamStatus, daemon.OpWorkerRetire}, daemon.WorkforceOpNames())
	for _, op := range daemon.WorkforceOpNames() {
		assert.Positive(t, daemon.ServedBudget(op, nil), "%s has a budget in the table, not a nil one", op)
	}
}

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
		map[string]any{"pane_id": "w14:p1", "workspace_id": "w14", "tab_id": "w14:t1", "agent": "claude", "agent_status": "working"},
		map[string]any{"pane_id": "w14:p2", "workspace_id": "w14", "tab_id": "w14:t2", "agent": "claude", "agent_status": "idle", "tokens": map[string]string{"ctx": "31"}})
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
	finished := make(chan struct{})
	stop, err := daemon.ServeOp(t.Context(), sock, "slow", 8*time.Second, func(ctx context.Context) (any, error) {
		defer close(finished)
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
	stopShort, err := daemon.ServeOp(t.Context(), short, "slow", 3*time.Second, func(ctx context.Context) (any, error) {
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
	<-finished
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

// TestAnOpPastItsBudgetIsATimeout: an op whose own budget ends answers
// timeout, whatever its own code for the error, and says what it waited on;
// a deadline of an inner call, the op's budget still running, keeps the op's
// own code.
func TestAnOpPastItsBudgetIsATimeout(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortDir(t), "d.sock")
	stop, err := daemon.ServeOp(t.Context(), sock, "wait", 200*time.Millisecond, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("list panes: %w", ctx.Err())
	})
	require.NoError(t, err)
	t.Cleanup(stop)
	_, err = daemon.CallWith(t.Context(), sock, "wait", json.RawMessage(`{}`), ready)
	var re *daemon.RequestError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "timeout", re.Code)
	assert.Contains(t, re.Message, "wait ran past its 200ms budget")
	assert.Contains(t, re.Message, "list panes")

	inner := filepath.Join(shortDir(t), "e.sock")
	stopInner, err := daemon.ServeOp(t.Context(), inner, "call", time.Minute, func(context.Context) (any, error) {
		return nil, fmt.Errorf("list panes: %w", context.DeadlineExceeded)
	})
	require.NoError(t, err)
	t.Cleanup(stopInner)
	_, err = daemon.CallWith(t.Context(), inner, "call", json.RawMessage(`{}`), ready)
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "failed", re.Code, "the op's own code: its budget had not ended")
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
	stop, err := daemon.ServeOp(ctx, sock, "wait", 2*ready, func(ctx context.Context) (any, error) {
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
		workforce.ErrBadRequest:     "bad_request",
		workforce.ErrUnknownProject: "unknown_project",
		fmt.Errorf("%w: %w", workforce.ErrConfig, settings.ErrBypassesAgentisan): "bypasses_agentisan",
		workforce.ErrConfig: "config_invalid",
		fmt.Errorf("%w: %w", workforce.ErrHerdr, daemon.ErrHerdrGone): "herdr_gone",
		workforce.ErrHerdr:          "herdr_failed",
		store.ErrStore:              "store_failed",
		errors.New("anything else"): "failed",
	} {
		assert.Equal(t, code, daemon.WorkforceCode(err), err.Error())
	}
}

// TestOpBudgets: team status gets 10 s; any other op none of its own.
func TestOpBudgets(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 10*time.Second, daemon.OpBudget(daemon.OpTeamStatus, nil))
	assert.Equal(t, 10*time.Second, daemon.OpBudget(daemon.OpTeamStatus, json.RawMessage(`not json`)))
	assert.Zero(t, daemon.OpBudget("health", nil))
}

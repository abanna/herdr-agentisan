package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// rawDB opens the state database of th's daemon in stateDir beside the
// daemon's own connection, waiting out its locks as the store does.
func rawDB(t *testing.T, stateDir string, th *teamHerdr) *sql.DB {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", store.BusyTimeout.Milliseconds()))
	u := url.URL{Scheme: "file", OmitHost: true, Path: paths(t, stateDir, th.fakeHerdr).DB, RawQuery: q.Encode()}
	raw, err := sql.Open("sqlite", u.String())
	require.NoError(t, err)
	return raw
}

// execDB runs query on the state database of th's daemon in stateDir,
// creating it first if no daemon has yet.
func execDB(t *testing.T, stateDir string, th *teamHerdr, query string, args ...any) {
	t.Helper()
	p := paths(t, stateDir, th.fakeHerdr)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	st, err := store.Open(t.Context(), p.DB)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	raw := rawDB(t, stateDir, th)
	defer raw.Close() //nolint:errcheck // test cleanup
	_, err = raw.ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

// seedRow writes a live row of the ha team binding id to pane and terminal,
// with the shell pid and start, NULL when pid is 0.
func seedRow(t *testing.T, stateDir string, th *teamHerdr, id, pane, terminal string, pid, start int64) {
	t.Helper()
	var shellPID, shellStart any
	if pid != 0 {
		shellPID, shellStart = pid, start
	}
	execDB(t, stateDir, th, `INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, terminal_id, shell_pid, shell_start)
VALUES (?, 'ha', 'coder', ?, 0, 'active', 0, ?, ?, ?)`, id, pane, terminal, shellPID, shellStart)
}

// workerRow is a workers row as the daemon left it.
type workerRow struct {
	Pane, Terminal, Status string
	ShellPID, ShellStart   int64
}

// rowOf reads id's row, or the error reading it.
func rowOf(t *testing.T, stateDir string, th *teamHerdr, id string) (workerRow, error) {
	t.Helper()
	raw := rawDB(t, stateDir, th)
	defer raw.Close() //nolint:errcheck // test cleanup
	var r workerRow
	err := raw.QueryRowContext(t.Context(),
		"SELECT pane_id, terminal_id, status, coalesce(shell_pid, 0), coalesce(shell_start, 0) FROM workers WHERE logical_id = ?", id).
		Scan(&r.Pane, &r.Terminal, &r.Status, &r.ShellPID, &r.ShellStart)
	return r, err //nolint:wrapcheck // test helper
}

// row is id's row now.
func row(t *testing.T, stateDir string, th *teamHerdr, id string) workerRow {
	t.Helper()
	r, err := rowOf(t, stateDir, th, id)
	require.NoError(t, err)
	return r
}

// eventually waits until id's row is want.
func eventually(t *testing.T, stateDir string, th *teamHerdr, id string, want workerRow) {
	t.Helper()
	var last workerRow
	var lastErr error
	require.Eventually(t, func() bool {
		last, lastErr = rowOf(t, stateDir, th, id)
		return lastErr == nil && last == want
	}, ready, 10*time.Millisecond, "%s never became %+v; last %+v, %v", id, want, &last, &lastErr)
}

// setPane sets key on the pane th lists as id, copying what it changes: the
// server encodes a reply after the lock is released.
func (th *teamHerdr) setPane(id, key string, value any) {
	th.mu.Lock()
	defer th.mu.Unlock()
	panes := make([]map[string]any, 0, len(th.panes))
	for _, p := range th.panes {
		if p["pane_id"] == id {
			p = maps.Clone(p)
			p[key] = value
		}
		panes = append(panes, p)
	}
	th.panes = panes
}

// renewTerminals gives every pane a new terminal id and keeps its pane id,
// as a live handoff does.
func (th *teamHerdr) renewTerminals() {
	th.mu.Lock()
	defer th.mu.Unlock()
	panes := make([]map[string]any, 0, len(th.panes))
	for _, p := range th.panes {
		p = maps.Clone(p)
		p["terminal_id"] = "renewed_" + p["pane_id"].(string)
		panes = append(panes, p)
	}
	th.panes = panes
}

func (th *teamHerdr) setShell(pane string, pid uint32, fails bool) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.shells[pane] = pid
	th.infoFails[pane] = fails
}

// haStatus is the ha team's status from the daemon at sock.
func haStatus(t *testing.T, sock string) workforce.StatusResult {
	t.Helper()
	data, err := daemon.Workforce(t.Context(), sock, daemon.OpTeamStatus, workforce.StatusRequest{Selector: workforce.Selector{Project: "ha"}})
	require.NoError(t, err)
	var st workforce.StatusResult
	require.NoError(t, json.Unmarshal(data, &st))
	return st
}

// TestTheTeamPollFollowsAMove is A35 within one server's life: the first
// poll that sees a row bound by its terminal reads the pane's shell into it,
// and a pane moved away and back, under a new pane id with the same
// terminal, is rebound within a poll, so team status finds the worker where
// it is now.
func TestTheTeamPollFollowsAMove(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	dir := shortDir(t)
	seedRow(t, dir, th, "ha-coder-1", "w14:p2", "term_w14:p2", 0, 0)
	th.setShell("w14:p2", 4242, false)
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 77, 4242: 9}}).startTime
	run(t, o)

	eventually(t, dir, th, "ha-coder-1", workerRow{Pane: "w14:p2", Terminal: "term_w14:p2", Status: "active", ShellPID: 4242, ShellStart: 9})
	th.setPane("w14:p2", "pane_id", "w14:p9")
	th.setShell("w14:p9", 4242, false)
	eventually(t, dir, th, "ha-coder-1", workerRow{Pane: "w14:p9", Terminal: "term_w14:p2", Status: "active", ShellPID: 4242, ShellStart: 9})

	st := haStatus(t, socketPath(t, dir, th.path))
	require.Len(t, st.Groups, 1)
	require.Len(t, st.Groups[0].Members, 1)
	assert.Equal(t, "w14:p9", st.Groups[0].Members[0].PaneID)
	assert.True(t, st.Groups[0].Members[0].Recorded)
	assert.Empty(t, st.Unbound)
}

// TestTheTeamPollReKeysAcrossALiveHandoff is A35's simulated live handoff:
// every terminal is new and every pane id kept, as when the daemon restarts
// after one. A row re-keys only to a candidate whose shell has the row's pid
// and start time, past a hint pane that runs another's. A reused pid with another start, a stranger's shell at the
// row's pane id, a shell that has exited, a row with no candidate pane left,
// and a row with no shell to prove are lost, binding nothing;
// a pane herdr cannot describe leaves its row unverified, never lost, until
// it can; and a row with a handoff pending is never lost.
func TestTheTeamPollReKeysAcrossALiveHandoff(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	dir := shortDir(t)
	seedRow(t, dir, th, "ha-coder-1", "w14:p2", "term_w14:p2", 4242, 9)
	seedRow(t, dir, th, "ha-reused", "wN:p1", "term_wN:p1", 5555, 1)
	seedRow(t, dir, th, "ha-stranger", "wN:p2", "term_wN:p2", 6666, 1)
	seedRow(t, dir, th, "ha-unread", "wN:p3", "term_wN:p3", 0, 0)
	seedRow(t, dir, th, "ha-unanswered", "wY:p1", "term_wY:p1", 7777, 1)
	seedRow(t, dir, th, "ha-handing-off", "w2:p1", "term_w2:p1", 0, 0)
	seedRow(t, dir, th, "ha-gone", "w99:p1", "term_w99:p1", 8888, 1)
	seedRow(t, dir, th, "ha-exited", "wN:p4", "term_wN:p4", 9999, 1)
	execDB(t, dir, th, `INSERT INTO handoffs (logical_id, old_pane, new_pane, generation, new_terminal_id, created_at, updated_at)
VALUES ('ha-handing-off', 'w2:p1', 'w2:p9', 0, 'term_w2:p9', 1, 1)`)
	// ha-coder-2's hint pane runs the boss's shell; its own is in the tab its
	// id labels.
	seedRow(t, dir, th, "ha-coder-2", "wJ:p1", "term_wJ:p1", 3333, 5)
	th.mu.Lock()
	th.tabs = append(slices.Clone(th.tabs), map[string]any{"tab_id": "w14:t3", "workspace_id": "w14", "label": "ha-coder-2"})
	th.panes = append(slices.Clone(th.panes), map[string]any{"pane_id": "w14:p3", "terminal_id": "term_w14:p3", "workspace_id": "w14", "tab_id": "w14:t3", "agent": "claude", "agent_status": "working"})
	th.mu.Unlock()
	th.setShell("wJ:p1", 1111, false)
	th.setShell("w14:p3", 3333, false)
	th.renewTerminals()
	th.setShell("w14:p2", 4242, false)
	th.setShell("wN:p1", 5555, false)
	th.setShell("wN:p2", 6060, false)
	th.setShell("wY:p1", 7777, true)
	th.setShell("wN:p4", 9999, false) // exited: its start time cannot be read
	starts := &procs{start: map[int]uint64{os.Getpid(): 77, 4242: 9, 5555: 2, 6060: 1, 7777: 1, 1111: 5, 3333: 5}}
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Hooks.StartTime = starts.startTime
	run(t, o)

	eventually(t, dir, th, "ha-coder-1", workerRow{Pane: "w14:p2", Terminal: "renewed_w14:p2", Status: "active", ShellPID: 4242, ShellStart: 9})
	eventually(t, dir, th, "ha-coder-2", workerRow{Pane: "w14:p3", Terminal: "renewed_w14:p3", Status: "active", ShellPID: 3333, ShellStart: 5})
	eventually(t, dir, th, "ha-reused", workerRow{Pane: "wN:p1", Terminal: "term_wN:p1", Status: "lost", ShellPID: 5555, ShellStart: 1})
	eventually(t, dir, th, "ha-stranger", workerRow{Pane: "wN:p2", Terminal: "term_wN:p2", Status: "lost", ShellPID: 6666, ShellStart: 1})
	eventually(t, dir, th, "ha-unread", workerRow{Pane: "wN:p3", Terminal: "term_wN:p3", Status: "lost"})
	eventually(t, dir, th, "ha-gone", workerRow{Pane: "w99:p1", Terminal: "term_w99:p1", Status: "lost", ShellPID: 8888, ShellStart: 1})
	eventually(t, dir, th, "ha-exited", workerRow{Pane: "wN:p4", Terminal: "term_wN:p4", Status: "lost", ShellPID: 9999, ShellStart: 1})
	sock := socketPath(t, dir, th.path)
	st := haStatus(t, sock)
	require.Len(t, st.Groups, 1)
	var members [][]any
	for _, m := range st.Groups[0].Members {
		members = append(members, []any{m.ID, m.Recorded, m.PaneID})
	}
	assert.Equal(t, [][]any{{"ha-coder-1", true, "w14:p2"}, {"ha-coder-2", true, "w14:p3"}}, members, "each where its shell is")
	assert.Equal(t, []workforce.UnboundWorker{
		{ID: "ha-exited", Group: "coder", PaneID: "wN:p4", Status: "lost"},
		{ID: "ha-gone", Group: "coder", PaneID: "w99:p1", Status: "lost"},
		{ID: "ha-handing-off", Group: "coder", PaneID: "w2:p1", Status: "active"},
		{ID: "ha-reused", Group: "coder", PaneID: "wN:p1", Status: "lost"},
		{ID: "ha-stranger", Group: "coder", PaneID: "wN:p2", Status: "lost"},
		{ID: "ha-unanswered", Group: "coder", PaneID: "wY:p1", Status: "active"},
		{ID: "ha-unread", Group: "coder", PaneID: "wN:p3", Status: "lost"},
	}, st.Unbound)

	polled(t, th.srv, 2)
	assert.Equal(t, "active", row(t, dir, th, "ha-unanswered").Status, "unverified while herdr cannot say")
	assert.Equal(t, "active", row(t, dir, th, "ha-handing-off").Status, "the handoff's to settle")
	th.setShell("wY:p1", 7777, false)
	eventually(t, dir, th, "ha-unanswered", workerRow{Pane: "wY:p1", Terminal: "renewed_wY:p1", Status: "active", ShellPID: 7777, ShellStart: 1})
	th.setShell("wN:p1", 5555, false)
	starts.set(5555, 1)
	polled(t, th.srv, 2)
	assert.Equal(t, "lost", row(t, dir, th, "ha-reused").Status, "lost is sticky: the boss's to retire or reuse")
}

// TestARowBoundMidPollIsNotLost: the poll reads the rows before it lists the
// panes. A spawn opens its pane, then writes its row; when both land after
// the poll's pane.list, a poll that read the rows last would find a row
// whose pane it had not listed, with no shell to prove, and lose it. Read
// first, the row is the next poll's, which lists its pane.
func TestARowBoundMidPollIsNotLost(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	dir := shortDir(t)
	execDB(t, dir, th, "SELECT 1") // creates the database
	var bound atomic.Bool
	th.mu.Lock()
	th.onCall = func(method string) {
		// workspace.list is the poll's next call after pane.list.
		if method != "workspace.list" || bound.Swap(true) {
			return
		}
		th.mu.Lock()
		th.panes = append(slices.Clone(th.panes), map[string]any{"pane_id": "w14:p3", "terminal_id": "term_w14:p3", "workspace_id": "w14", "tab_id": "w14:t3", "agent_status": "unknown"})
		th.mu.Unlock()
		seedRow(t, dir, th, "ha-coder-2", "w14:p3", "term_w14:p3", 0, 0)
	}
	th.mu.Unlock()
	run(t, teamOpts(t, dir, th, workforceConfig(t)))

	require.Eventually(t, bound.Load, ready, 10*time.Millisecond)
	polled(t, th.srv, 2)
	assert.Equal(t, workerRow{Pane: "w14:p3", Terminal: "term_w14:p3", Status: "active"}, row(t, dir, th, "ha-coder-2"))
}

// TestAFailedWorkersReadLosesNothing: a poll that cannot read the rows
// reconciles nothing, so it marks nothing lost and does not open the
// workforce ops, which refuse not_ready rather than read the rows
// themselves; once the rows can be read, the poll judges them.
func TestAFailedWorkersReadLosesNothing(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	dir := shortDir(t)
	seedRow(t, dir, th, "ha-coder-1", "w14:p2", "term_closed", 0, 0)
	execDB(t, dir, th, "ALTER TABLE workers RENAME TO workers_away")
	run(t, teamOpts(t, dir, th, workforceConfig(t)))
	healthy(t, dir, th.fakeHerdr)
	polled(t, th.srv, 2)
	sock := socketPath(t, dir, th.path)

	_, err := daemon.CallWith(t.Context(), sock, daemon.OpTeamStatus, json.RawMessage(`{"project":"ha"}`), 10*time.Second)
	require.ErrorIs(t, err, daemon.ErrNotReady)
	raw := rawDB(t, dir, th)
	defer raw.Close() //nolint:errcheck // test cleanup
	var status string
	require.NoError(t, raw.QueryRowContext(t.Context(), "SELECT status FROM workers_away").Scan(&status))
	assert.Equal(t, "active", status, "nothing was judged unread")
	execDB(t, dir, th, "ALTER TABLE workers_away RENAME TO workers")
	eventually(t, dir, th, "ha-coder-1", workerRow{Pane: "w14:p2", Terminal: "term_closed", Status: "lost"})
	assert.Equal(t, []workforce.UnboundWorker{{ID: "ha-coder-1", Group: "coder", PaneID: "w14:p2", Status: "lost"}}, haStatus(t, sock).Unbound)
}

// TestWorkforceOpsWaitForTheFirstReconcile: until the team poll has
// reconciled the rows since the daemon started, which needs agent.list,
// every workforce op refuses not_ready; daemon.Workforce asks again within
// the op's budget, so team status answers once the poll can run.
func TestWorkforceOpsWaitForTheFirstReconcile(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	th.mu.Lock()
	th.agentsErr = true
	th.mu.Unlock()
	dir := shortDir(t)
	run(t, teamOpts(t, dir, th, workforceConfig(t)))
	healthy(t, dir, th.fakeHerdr)
	polled(t, th.srv, 1)
	sock := socketPath(t, dir, th.path)

	for op, args := range map[string]string{daemon.OpTeamStatus: `{"project":"ha"}`, daemon.OpWorkerRetire: `{"target":"ha-coder-1"}`} {
		_, err := daemon.CallWith(t.Context(), sock, op, json.RawMessage(args), 10*time.Second)
		var re *daemon.RequestError
		require.ErrorAs(t, err, &re, op)
		assert.Equal(t, "not_ready", re.Code, op)
		require.ErrorIs(t, err, daemon.ErrNotReady)
	}

	time.AfterFunc(100*time.Millisecond, func() {
		th.mu.Lock()
		th.agentsErr = false
		th.mu.Unlock()
	})
	st := haStatus(t, sock)
	assert.Equal(t, "ha", st.Project)
}

// TestWorkforceRetriesNotReady: daemon.Workforce asks a daemon that answers
// not_ready again until it answers, all within the one wait the command has,
// which the caller's context can shorten; once that ends, it asks no more.
func TestWorkforceRetriesNotReady(t *testing.T) {
	t.Parallel()
	// serve answers op not_ready n times, running then before each, and
	// counts the asks.
	serve := func(n int32, then func(int32)) (string, *atomic.Int32) {
		var calls atomic.Int32
		sock := shortDir(t) + "/d.sock"
		closeOp, err := daemon.ServeOp(t.Context(), sock, daemon.OpTeamStatus, "not_ready", time.Second, func(context.Context) (any, error) {
			c := calls.Add(1)
			then(c)
			if c <= n {
				return nil, daemon.ErrNotReady
			}
			return map[string]bool{"ok": true}, nil
		})
		require.NoError(t, err)
		t.Cleanup(closeOp)
		return sock, &calls
	}

	sock, calls := serve(2, func(int32) {})
	data, err := daemon.Workforce(t.Context(), sock, daemon.OpTeamStatus, struct{}{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(data))
	assert.Equal(t, int32(3), calls.Load())

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	sock, calls = serve(1000, func(int32) {})
	_, err = daemon.Workforce(ctx, sock, daemon.OpTeamStatus, struct{}{})
	// The deadline ends an exchange, or the wait between two.
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, daemon.ErrNotReady), "%v", err)
	asked := calls.Load()
	assert.Positive(t, asked)
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, asked, calls.Load(), "no ask after the wait ended")
}

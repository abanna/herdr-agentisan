package daemon_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

const (
	muteWorker  = "ha-coder-1"
	muteOldPane = "w14:p2"
	muteNewPane = "w14:p3"
)

// TestRetireClearsTheBindingMute covers the immediate retire reply and list (NERD-5730).
func TestRetireClearsTheBindingMute(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	addMutePane(th, "ha-coder-2", true)
	seedRow(t, dir, th, "ha-coder-2", muteNewPane, "term_"+muteNewPane, 0, 0)
	s, _ := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	muteForThreeHours(t, s, "ha-coder-2")

	got := noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteOldPane})
	assert.JSONEq(t, `{"id":"ha-coder-1","pane_id":"w14:p2","changed":true,"outcome":"retired","mute_cleared":true}`, string(got))
	assert.Equal(t, []string{"ha-coder-2"}, listedMutes(t, s))
	got = noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
	assert.JSONEq(t, `{"id":"ha-coder-1","pane_id":"w14:p2","changed":false,"outcome":"already_retired"}`, string(got))
}

// TestReusedWorkerBindingStartsUnmuted covers an ID recycled after a confirmed loss (NERD-5730).
func TestReusedWorkerBindingStartsUnmuted(t *testing.T) {
	t.Parallel()
	selector := workforce.Selector{Project: "ha"}
	for _, tc := range []struct {
		op   string
		args any
	}{
		{daemon.OpWorkerSpawn, workforce.SpawnRequest{Selector: selector, Group: "coder", Count: 1}},
		{daemon.OpTeamUp, workforce.UpRequest{Selector: selector}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			th, _, dir := haTeam(t)
			s, logs := serveMuteWorkers(t, th, dir)
			muteForThreeHours(t, s, muteWorker)
			require.NoError(t, (herdr.Client{SocketPath: th.path}).ClosePane(t.Context(), muteOldPane))
			require.Eventually(t, func() bool {
				r, err := rowOf(t, dir, th, muteWorker)
				return err == nil && r.Status == store.StatusLost
			}, ready, 5*time.Millisecond)
			polled(t, th.srv, 2)
			assert.Empty(t, listedMutes(t, s), "the poll confirmed loss")

			noticeWorkforce(t, s, tc.op, tc.args)
			r := row(t, dir, th, muteWorker)
			require.Equal(t, muteNewPane, r.Pane, "the op reused the logical ID")
			polled(t, th.srv, 2)
			th.setPane(muteNewPane, "agent_status", "blocked")
			polled(t, th.srv, 2)
			assert.Contains(t, noticeLines(logs), "ha-coder-1 is blocked", "the first notice of the new row was delivered")
		})
	}
}

// TestMuteFollowsTheWorkerRowThroughHandoff covers the same generation on a new terminal (NERD-5730).
func TestMuteFollowsTheWorkerRowThroughHandoff(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	addMutePane(th, "ha-coder-2", true)
	th.setPane(muteNewPane, "tokens", map[string]string{"item": "NERD-5730", "stage": "verify"})
	st := stateStore(t, dir, th)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	th.setPane(muteOldPane, "agent_status", "blocked")
	polled(t, th.srv, 2)
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "ha-coder-1 is blocked")

	_, err := st.BeginHandoff(t.Context(), muteWorker, muteNewPane, "term_"+muteNewPane, time.Now())
	require.NoError(t, err)
	_, err = daemon.Workforce(t.Context(), s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
	var refusal *daemon.RequestError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "handoff_pending", refusal.Code)
	assert.Equal(t, []string{muteWorker}, listedMutes(t, s))
	th.setPane(muteOldPane, "agent_status", "idle")
	polled(t, th.srv, 2)
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "ha-coder-1 is no longer blocked")

	// A retry after the names moved can complete the pending handoff.
	th.setAgents(muteAgent("w14:p1", "ha-boss"), muteAgent(muteNewPane, muteWorker))
	noticeWorkforce(t, s, daemon.OpHandoffAck, workforce.HandoffAckRequest{Target: muteNewPane})
	require.Equal(t, muteNewPane, row(t, dir, th, muteWorker).Pane)
	polled(t, th.srv, 2)
	th.setPane(muteNewPane, "agent_status", "blocked")
	polled(t, th.srv, 2)
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "ha-coder-1 is blocked", "the replacement still uses the row's mute")
	assert.Equal(t, []string{muteWorker}, listedMutes(t, s))
	closed := noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteOldPane})
	assert.NotContains(t, string(closed), "mute_cleared")
	assert.Equal(t, []string{muteWorker}, listedMutes(t, s), "closing the old pane preserves the row mute")

	got := noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
	assert.JSONEq(t, `{"id":"ha-coder-1","pane_id":"w14:p3","changed":true,"outcome":"retired","mute_cleared":true}`, string(got))
	assert.Empty(t, listedMutes(t, s))
}

// TestRetiredBindingCannotBeMutedFromTheOldPoll holds the published model across retire (NERD-5730).
func TestRetiredBindingCannotBeMutedFromTheOldPoll(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, _ := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	arrived := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { close(resume) })
	th.mu.Lock()
	th.onCall = func(method string) {
		if method == "pane.report_metadata" {
			once.Do(func() { close(arrived); <-resume })
		}
	}
	th.mu.Unlock()
	select {
	case <-arrived:
	case <-time.After(ready):
		t.Fatal("the team poll did not reach its metadata push")
	}

	noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
	_, err := daemon.Workforce(t.Context(), s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: muteWorker})
	assertUnknownMuteWorker(t, err)
	assert.Empty(t, listedMutes(t, s))

	// The old model also cannot mute a new generation with the same ID.
	noticeWorkforce(t, s, daemon.OpWorkerSpawn, workforce.SpawnRequest{
		Selector: workforce.Selector{Project: "ha"}, Group: "coder", Count: 1,
	})
	_, err = daemon.Workforce(t.Context(), s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: muteWorker})
	assertUnknownMuteWorker(t, err)
	assert.Empty(t, listedMutes(t, s))
}

func assertUnknownMuteWorker(t *testing.T, err error) {
	t.Helper()
	var refusal *daemon.RequestError
	if assert.ErrorAs(t, err, &refusal) {
		assert.Equal(t, "unknown_worker", refusal.Code)
	}
}

func serveMuteWorkers(t *testing.T, th *teamHerdr, dir string) (string, *lockedLog) {
	t.Helper()
	normal := th.serve(22)
	th.mu.Lock()
	th.other = func(r herdrtest.Request) herdrtest.Reply {
		return muteWorkerHerdr(th, normal, r)
	}
	th.mu.Unlock()
	logs := &lockedLog{}
	o := teamOpts(t, dir, th, haNotices(t, "[notices]\ngap = 0\nrepeat = 0\n"))
	o.Hooks.Preflight = func(context.Context, string) error { return nil }
	logger, err := logging.New(logs, "info", "json")
	require.NoError(t, err)
	o.Logger = logger
	serving(t, o, th.fakeHerdr)
	waitLine(t, logs, "the daemon started")
	polled(t, th.srv, 2)
	return socketPath(t, dir, th.path), logs
}

func noticeWorkforce(t *testing.T, s, op string, req any) json.RawMessage {
	t.Helper()
	got, err := daemon.Workforce(t.Context(), s, op, req)
	require.NoError(t, err, "%s", op)
	return got
}

func muteForThreeHours(t *testing.T, s, worker string) {
	t.Helper()
	noticeWorkforce(t, s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: worker, For: 3 * time.Hour})
}

func listedMutes(t *testing.T, s string) []string {
	t.Helper()
	var listed daemon.MutesResult
	require.NoError(t, json.Unmarshal(noticeWorkforce(t, s, daemon.OpNoticesList, struct{}{}), &listed))
	out := make([]string, 0, len(listed.Mutes))
	for _, mute := range listed.Mutes {
		out = append(out, mute.Worker)
	}
	return out
}

func muteAgent(pane, name string) map[string]any {
	a := named(pane, name)
	a["terminal_id"] = "term_" + pane
	a["agent"] = "claude"
	a["interactive_ready"] = true
	return a
}

func addMutePane(th *teamHerdr, label string, running bool) (map[string]any, map[string]any) {
	th.mu.Lock()
	defer th.mu.Unlock()
	tab := map[string]any{"tab_id": "w14:t3", "workspace_id": "w14", "label": label}
	pane := map[string]any{"pane_id": muteNewPane, "terminal_id": "term_" + muteNewPane, "workspace_id": "w14", "tab_id": "w14:t3"}
	if running {
		pane["agent"], pane["agent_status"] = "claude", "idle"
		th.agents = append(slices.Clone(th.agents), muteAgent(muteNewPane, label))
	}
	th.tabs = append(th.tabs, tab)
	th.panes = append(slices.Clone(th.panes), pane)
	return tab, pane
}

func muteWorkerHerdr(th *teamHerdr, normal herdrtest.Handler, r herdrtest.Request) herdrtest.Reply {
	var args map[string]string
	result := map[string]any{"type": "ok"}
	switch r.Method {
	case "pane.close":
		_ = json.Unmarshal(r.Params, &args)
		id := args["pane_id"]
		th.mu.Lock()
		i := slices.IndexFunc(th.panes, func(p map[string]any) bool { return p["pane_id"] == id })
		if i >= 0 {
			tab := th.panes[i]["tab_id"]
			th.panes = slices.Delete(slices.Clone(th.panes), i, i+1)
			th.tabs = slices.DeleteFunc(slices.Clone(th.tabs), func(t map[string]any) bool { return t["tab_id"] == tab })
		}
		th.agents = slices.DeleteFunc(slices.Clone(th.agents), func(a map[string]any) bool { return a["pane_id"] == id })
		th.mu.Unlock()
	case "tab.create":
		var req struct {
			Label string `json:"label"`
		}
		_ = json.Unmarshal(r.Params, &req)
		tab, pane := addMutePane(th, req.Label, false)
		result = map[string]any{"type": "tab_created", "tab": tab, "root_pane": pane}
	case "agent.start":
		var req struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(r.Params, &req)
		agent := muteAgent(muteNewPane, req.Name)
		th.setPane(muteNewPane, "agent", "claude")
		th.setPane(muteNewPane, "agent_status", "idle")
		th.mu.Lock()
		th.agents = append(slices.Clone(th.agents), agent)
		th.mu.Unlock()
		result = map[string]any{"type": "agent_started", "agent": agent}
	case "agent.get":
		_ = json.Unmarshal(r.Params, &args)
		result = map[string]any{"type": "agent_info", "agent": muteAgent(muteNewPane, args["target"])}
	default:
		return normal(r)
	}
	return herdrtest.Reply{Result: result}
}

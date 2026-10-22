package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// teamHerdr is a fakeHerdr that also answers what the team poll asks: a
// layout of workspaces, tabs, panes and agents, workspace.report_metadata and
// pane.report_metadata, which it refuses for the workspaces and panes in
// refuse; and the agent view's set, which it refuses with viewErr's code
// when that is set, and clear.
type teamHerdr struct {
	*fakeHerdr
	mu         sync.Mutex
	workspaces []herdr.WorkspaceInfo
	tabs       []map[string]any
	panes      []map[string]any
	// panesErr makes pane.list fail, for the poll and the focus collector
	// alike.
	panesErr bool
	// agents is agent.list's answer; agentsErr makes it fail instead.
	agents    []map[string]any
	agentsErr bool
	refuse    map[string]bool
	viewErr   string
	// shells is each pane's shell pid, which pane.process_info answers; a
	// pane with none answers herdr's null. infoFails fails it for a pane.
	shells    map[string]uint32
	infoFails map[string]bool
	// procs are pane.process_info's foreground processes, by pane; a pane
	// with none runs no foreground job.
	procs map[string][]map[string]any
	// onCall, when set, runs before each request is answered.
	onCall func(method string)
	// prompt, when set, answers agent.prompt (messages_test.go).
	prompt herdrtest.Handler
}

// newTeamHerdr serves liveSpaces.
func newTeamHerdr(t *testing.T) *teamHerdr {
	t.Helper()
	workspaces, panes := liveSpaces()
	th := &teamHerdr{
		fakeHerdr:  &fakeHerdr{t: t, path: filepath.Join(shortDir(t), "h.sock"), streams: make(chan *stream, 64)},
		workspaces: workspaces, tabs: []map[string]any{}, panes: panes, refuse: map[string]bool{},
		shells: map[string]uint32{}, infoFails: map[string]bool{},
	}
	th.h = th.serveTeam(th.serve(22))
	th.srv = herdrtest.StartAt(t, th.path, th.h)
	return th
}

func (th *teamHerdr) serveTeam(normal herdrtest.Handler) herdrtest.Handler {
	return func(r herdrtest.Request) herdrtest.Reply {
		th.mu.Lock()
		hook := th.onCall
		th.mu.Unlock()
		if hook != nil {
			hook(r.Method)
		}
		switch r.Method {
		case "pane.list":
			th.mu.Lock()
			defer th.mu.Unlock()
			if th.panesErr {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": th.panes}}
		case "workspace.list":
			th.mu.Lock()
			defer th.mu.Unlock()
			// A copy: the server encodes the reply after the lock is released.
			return herdrtest.Reply{Result: map[string]any{"type": "workspace_list", "workspaces": slices.Clone(th.workspaces)}}
		case "tab.list":
			th.mu.Lock()
			defer th.mu.Unlock()
			return herdrtest.Reply{Result: map[string]any{"type": "tab_list", "tabs": slices.Clone(th.tabs)}}
		case "agent.list":
			th.mu.Lock()
			defer th.mu.Unlock()
			if th.agentsErr {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "agent_list", "agents": append([]map[string]any{}, th.agents...)}}
		case "pane.process_info":
			var params struct {
				PaneID string `json:"pane_id"`
			}
			_ = json.Unmarshal(r.Params, &params)
			th.mu.Lock()
			defer th.mu.Unlock()
			if th.infoFails[params.PaneID] {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
			}
			procs := th.procs[params.PaneID]
			if procs == nil {
				procs = []map[string]any{}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{
				"pane_id": params.PaneID, "shell_pid": th.shells[params.PaneID], "foreground_process_group_id": 0, "foreground_processes": slices.Clone(procs),
			}}}
		case "pane.report_metadata":
			var params struct {
				PaneID string `json:"pane_id"`
			}
			_ = json.Unmarshal(r.Params, &params)
			th.mu.Lock()
			defer th.mu.Unlock()
			if th.refuse[params.PaneID] {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane " + params.PaneID + " not found"}}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
		case "workspace.report_metadata":
			var params struct {
				WorkspaceID string `json:"workspace_id"`
			}
			_ = json.Unmarshal(r.Params, &params)
			th.mu.Lock()
			defer th.mu.Unlock()
			if th.refuse[params.WorkspaceID] {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "workspace_not_found", Message: "workspace " + params.WorkspaceID + " not found"}}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
		case "agent.view.set":
			th.mu.Lock()
			defer th.mu.Unlock()
			if th.viewErr != "" {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: th.viewErr, Message: "refused: " + th.viewErr}}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "agent_view", "active": true}}
		case "agent.view.clear":
			return herdrtest.Reply{Result: map[string]any{"type": "agent_view", "active": false}}
		case "agent.prompt":
			th.mu.Lock()
			prompt := th.prompt
			th.mu.Unlock()
			if prompt != nil {
				return prompt(r)
			}
		}
		return normal(r)
	}
}

func (th *teamHerdr) relabel(id, label string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	for i := range th.workspaces {
		if th.workspaces[i].WorkspaceID == id {
			th.workspaces[i].Label = label
		}
	}
}

// pushes returns the params of every workspace.report_metadata srv got, by
// workspace id, in order.
func pushes(t *testing.T, srv *herdrtest.Server) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, r := range srv.Requests() {
		if r.Method != "workspace.report_metadata" {
			continue
		}
		var params struct {
			WorkspaceID string `json:"workspace_id"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &params))
		out[params.WorkspaceID] = append(out[params.WorkspaceID], string(r.Params))
	}
	return out
}

// panePushes returns the params of every pane.report_metadata srv got, by
// pane id, in order.
func panePushes(t *testing.T, srv *herdrtest.Server) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, r := range srv.Requests() {
		if r.Method != "pane.report_metadata" {
			continue
		}
		var params struct {
			PaneID string `json:"pane_id"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &params))
		out[params.PaneID] = append(out[params.PaneID], string(r.Params))
	}
	return out
}

// lists counts the workspace.list requests srv got: one per poll.
func lists(srv *herdrtest.Server) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == "workspace.list" {
			n++
		}
	}
	return n
}

// teamCalls returns the requests srv got that only the team poll and the
// agent view's sync make: workspace.list, tab.list, agent.list, the pushes
// and the view's set and clear. The daemon's own
// client (HerdrClient) makes none of them. It is what a replacement is held
// to while Run's poll runs: Run's ping has no Dialed hook and finds a
// replaced socket by stillServed's own identity check, so a ping dialled just
// after a swap can still reach the replacement (daemon.go). The poll's calls
// and focus's never do. Every team pane.list that a replacement answers is
// followed, on the same client, by a workspace.list, so a team poll that
// reached the replacement shows here all the same.
func teamCalls(srv *herdrtest.Server) []string {
	var out []string
	for _, r := range srv.Requests() {
		switch r.Method {
		case "workspace.list", "tab.list", "agent.list", "workspace.report_metadata", "pane.report_metadata", "agent.view.set", "agent.view.clear":
			out = append(out, r.Method)
		}
	}
	return out
}

// polled waits until srv has answered n more polls than it had, so every
// push of the polls before has landed. A poll counted can be one that listed
// panes before a layout change, so polled(t, srv, n) promises n-1 whole polls
// of the new layout.
func polled(t *testing.T, srv *herdrtest.Server, n int) {
	t.Helper()
	from := lists(srv)
	require.Eventually(t, func() bool { return lists(srv) >= from+n+1 }, ready, 5*time.Millisecond,
		"the daemon did not poll %d more times", n)
}

// teamOpts are opts with the team poll on, reading configDir.
func teamOpts(t *testing.T, stateDir string, th *teamHerdr, configDir string) daemon.Options {
	t.Helper()
	o := opts(t, stateDir, th.fakeHerdr)
	o.Team = &daemon.TeamOptions{ConfigDir: configDir}
	return o
}

// liveSpaces is the live layout of 2026-10-10, trimmed: the boss space, three
// group spaces (one empty), the ignored edit shell, and a plain workspace.
func liveSpaces() ([]herdr.WorkspaceInfo, []map[string]any) {
	ws := []herdr.WorkspaceInfo{
		{WorkspaceID: "wJ", Label: "◆ boss"},
		{WorkspaceID: "wN", Label: "◆ coders"},
		{WorkspaceID: "wY", Label: "◆ qa"},
		{WorkspaceID: "w0", Label: "◆ research"},
		{WorkspaceID: "w12", Label: "◆ herdr"},
		{WorkspaceID: "w2", Label: "notes"},
	}
	pane := func(id, ws, agent, status string, tokens map[string]string) map[string]any {
		p := map[string]any{"pane_id": id, "terminal_id": "term_" + id, "workspace_id": ws, "agent": agent, "agent_status": status, "focused": false}
		if agent == "" {
			p["agent"] = nil
		}
		if tokens != nil {
			p["tokens"] = tokens
		}
		return p
	}
	panes := []map[string]any{
		pane("wJ:p1", "wJ", "claude", "working", nil),
		pane("wN:p1", "wN", "claude", "working", map[string]string{"ctx": "40"}),
		pane("wN:p2", "wN", "claude", "done", map[string]string{"handoff": "⟳ READY TO RESTART"}),
		pane("wN:p3", "wN", "claude", "blocked", nil),
		pane("wN:p4", "wN", "", "unknown", nil),
		pane("wY:p1", "wY", "codex", "idle", nil),
		pane("w12:p1", "w12", "", "unknown", nil),
		pane("w2:p1", "w2", "claude", "working", nil),
	}
	return ws, panes
}

// wantPushes are the exact params each group space of liveSpaces receives,
// every poll: its own $team, source agentisan, TTL 9000 ms, and no other
// token. Written out by hand rather than from the package's constants, with
// the middle dot as the JSON escape \u00b7 so the test cannot share a
// mistyped glyph with the code.
var wantPushes = map[string]string{
	"wN": `{"workspace_id":"wN","source":"agentisan","tokens":{"team":"4 agents \u00b7 1 working \u00b7 1 ready \u00b7 1 blocked"},"ttl_ms":9000}`,
	"wY": `{"workspace_id":"wY","source":"agentisan","tokens":{"team":"1 agent \u00b7 0 working \u00b7 0 ready"},"ttl_ms":9000}`,
	"w0": `{"workspace_id":"w0","source":"agentisan","tokens":{"team":"0 agents \u00b7 0 working \u00b7 0 ready"},"ttl_ms":9000}`,
}

// TestRunPushesTeamToEachGroupSpace is D4's $team on the spaces layout: on
// every poll, each ◆ group space receives its exact string as a workspace
// token, source agentisan, TTL 9000 ms. ◆ boss, ◆ herdr and workspaces
// without the prefix receive nothing.
func TestRunPushesTeamToEachGroupSpace(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	dir := shortDir(t)
	serving(t, teamOpts(t, dir, th, ""), th.fakeHerdr)

	polled(t, th.srv, 2)
	got := pushes(t, th.srv)
	assert.ElementsMatch(t, []string{"wN", "wY", "w0"}, slices.Collect(maps.Keys(got)), "only the group spaces receive $team")
	for ws, params := range got {
		assert.GreaterOrEqual(t, len(params), 2, "%s is pushed on every poll, renewing its TTL", ws)
		for _, p := range params {
			assert.JSONEq(t, wantPushes[ws], p)
		}
	}
}

// ctxPush is the exact params of the ctx display push to pane: value in the
// band key, the other two bands cleared, unless place is "", the pane's
// place in the spaces under order-spaces (A28 addendum), and the stage
// display of a pane with no stage, both keys cleared (A42); source
// agentisan, TTL 9000 ms. Written out by hand rather than from the
// package's constants.
func ctxPush(pane, key, value, place string) string {
	bands := map[string]string{"ctx_ok": "", "ctx_warn": "", "ctx_hot": ""}
	bands[key] = value
	order := ""
	if place != "" {
		order = fmt.Sprintf(`,"order-spaces":%q`, place)
	}
	return fmt.Sprintf(`{"pane_id":%q,"source":"agentisan","tokens":{"ctx_ok":%q,"ctx_warn":%q,"ctx_hot":%q%s,"stage_now":"","stage_old":""},"ttl_ms":9000}`,
		pane, bands["ctx_ok"], bands["ctx_warn"], bands["ctx_hot"], order)
}

// wantCtx are the ctx display pushes liveSpaces' agent panes receive, every
// poll: every pane with an agent, the boss's and one outside every team
// included, and no shell. Those the spaces place carry their place: the
// boss, then coders, then qa, the shell in coders not counted.
var wantCtx = map[string]string{
	"wJ:p1": ctxPush("wJ:p1", "ctx_ok", "ctx  --", "000"),
	"wN:p1": ctxPush("wN:p1", "ctx_ok", "ctx 40%", "001"),
	"wN:p2": ctxPush("wN:p2", "ctx_ok", "ctx  --", "002"),
	"wN:p3": ctxPush("wN:p3", "ctx_ok", "ctx  --", "003"),
	"wY:p1": ctxPush("wY:p1", "ctx_ok", "ctx  --", "004"),
	"w2:p1": ctxPush("w2:p1", "ctx_ok", "ctx  --", ""),
}

// setToken sets a token on the pane th lists as id, or removes it when value
// is empty. It copies what it changes: the server encodes a reply after the
// lock is released.
func (th *teamHerdr) setToken(id, key, value string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	panes := make([]map[string]any, 0, len(th.panes))
	for _, p := range th.panes {
		if p["pane_id"] == id {
			p = maps.Clone(p)
			tokens := map[string]string{}
			if old, ok := p["tokens"].(map[string]string); ok {
				maps.Copy(tokens, old)
			}
			tokens[key] = value
			if value == "" {
				delete(tokens, key)
			}
			p["tokens"] = tokens
		}
		panes = append(panes, p)
	}
	th.panes = panes
}

// TestRunPushesTheCtxDisplayToEveryAgentPane is A28's ctx display: on every
// poll, every pane herdr's agents panel lists gets its ctx as a seven-character
// display in its band's token, the other two cleared, source agentisan, TTL
// 9000 ms. That is every pane with an agent herdr detected or agent.list
// names, in a team or not; a shell gets none, and the display follows the
// pane's ctx from one poll to the next.
func TestRunPushesTheCtxDisplayToEveryAgentPane(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setToken("wN:p3", "ctx", "70")
	th.setToken("wY:p1", "ctx", "58")
	th.agents = []map[string]any{{"pane_id": "w12:p1", "name": "hooked"}}
	serving(t, teamOpts(t, shortDir(t), th, ""), th.fakeHerdr)

	polled(t, th.srv, 2)
	want := maps.Clone(wantCtx)
	want["wN:p3"] = ctxPush("wN:p3", "ctx_hot", "ctx 70%", "003")
	want["wY:p1"] = ctxPush("wY:p1", "ctx_warn", "ctx 58%", "004")
	want["w12:p1"] = ctxPush("w12:p1", "ctx_ok", "ctx  --", "") // ◆ herdr: the team's workspace, no group's place
	got := panePushes(t, th.srv)
	assert.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(got)), "every agent pane, and no shell")
	for pane, params := range got {
		assert.GreaterOrEqual(t, len(params), 2, "%s is pushed on every poll, renewing its TTL", pane)
		for _, p := range params {
			assert.JSONEq(t, want[pane], p)
		}
	}

	th.setToken("wN:p3", "ctx", "")
	polled(t, th.srv, 1) // a poll in flight may have listed the old ctx
	before := len(panePushes(t, th.srv)["wN:p3"])
	polled(t, th.srv, 1)
	for _, p := range panePushes(t, th.srv)["wN:p3"][before:] {
		assert.JSONEq(t, ctxPush("wN:p3", "ctx_ok", "ctx  --", "003"), p, "an expired ctx moves the pane back to the ok band")
	}
}

// TestRunReportsEachAgentPaneOnceAPoll: the order key rides the ctx push (A28
// addendum), so a poll reports each agent pane once, never twice.
func TestRunReportsEachAgentPaneOnceAPoll(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	serving(t, teamOpts(t, shortDir(t), th, ""), th.fakeHerdr)
	polled(t, th.srv, 3)

	var polls []map[string]int // each poll starts with its workspace.list
	for _, r := range th.srv.Requests() {
		switch {
		case r.Method == "workspace.list":
			polls = append(polls, map[string]int{})
		case r.Method == "pane.report_metadata" && len(polls) > 0:
			var p struct {
				PaneID string `json:"pane_id"`
			}
			require.NoError(t, json.Unmarshal(r.Params, &p))
			polls[len(polls)-1][p.PaneID]++
		}
	}
	require.GreaterOrEqual(t, len(polls), 3)
	for _, pushes := range polls[:len(polls)-1] { // the last may still be pushing
		assert.NotEmpty(t, pushes)
		for pane, n := range pushes {
			assert.Equal(t, 1, n, "%s is reported once a poll", pane)
		}
	}
}

// TestNoCtxDisplayWithoutAnAgent: a layout of plain shells gets `$team` and
// no ctx display: there is no agent pane to push one to.
func TestNoCtxDisplayWithoutAnAgent(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, _ := liveSpaces()
	th.layout(ws, []map[string]any{
		{"pane_id": "wN:p1", "workspace_id": "wN", "agent": nil, "agent_status": "unknown", "focused": false, "tokens": map[string]string{"ctx": "40"}},
	}, nil)
	serving(t, teamOpts(t, shortDir(t), th, ""), th.fakeHerdr)

	polled(t, th.srv, 2)
	assert.NotEmpty(t, pushes(t, th.srv)["wN"], "the space still gets $team")
	assert.Empty(t, panePushes(t, th.srv), "a shell gets no ctx display, its ctx token notwithstanding")
}

// TestTeamPushStopsWithTheDaemon: once the daemon has stopped, cancelled or
// because its herdr went away, nothing more is pushed, $team or ctx display,
// and nothing was cleared on the way out: every push carried a value and the
// 9 s TTL, so the tokens left behind are gone within about 9 s, after a crash
// just the same.
func TestTeamPushStopsWithTheDaemon(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stop func(th *teamHerdr, r *running)
		want error
	}{
		"the daemon is stopped":         {stop: func(_ *teamHerdr, r *running) { r.cancel() }},
		"herdr removes its socket":      {stop: func(th *teamHerdr, _ *running) { th.stopKeepingRequests() }, want: daemon.ErrHerdrGone},
		"herdr is replaced at its path": {stop: func(th *teamHerdr, _ *running) { th.replace() }, want: daemon.ErrHerdrGone},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			original := th.srv
			r := serving(t, teamOpts(t, shortDir(t), th, ""), th.fakeHerdr)
			polled(t, original, 1)

			tc.stop(th, r)
			err := r.wait(t, ready)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
			// Run has joined the poll, so everything it will ever send is
			// sent, but the server records a request on a goroutine of its
			// own once it has accepted the connection and read the line: a
			// push written just before the stop can land after Run returns.
			// Settle before the baseline, so it holds every push sent before
			// the stop, and before the second look, so that holds every push
			// sent since.
			original.Settle(t)
			after, afterCtx := pushes(t, original), panePushes(t, original)
			time.Sleep(10 * 20 * time.Millisecond) // ten polls' worth
			original.Settle(t)
			assert.Equal(t, after, pushes(t, original), "no push after the daemon stopped")
			assert.Equal(t, afterCtx, panePushes(t, original), "no ctx display after the daemon stopped")
			for ws, params := range after {
				for _, p := range params {
					assert.JSONEq(t, wantPushes[ws], p, "never an empty or cleared token, always the 9 s TTL")
				}
			}
			for pane, params := range afterCtx {
				for _, p := range params {
					assert.JSONEq(t, wantCtx[pane], p, "a display in its band, always the 9 s TTL")
				}
			}
			if th.srv != original {
				th.srv.Settle(t)
				assert.Empty(t, teamCalls(th.srv), "the replacement, which the next daemon belongs to, gets no team call")
			}
		})
	}
}

// stopKeepingRequests closes the server, removing its socket, as a herdr that
// exits does.
func (th *teamHerdr) stopKeepingRequests() { th.srv.Close() }

// replace puts a new server, answering the same layout, at the same path.
func (th *teamHerdr) replace() {
	th.srv.Close()
	th.srv = herdrtest.StartAt(th.t, th.path, th.serveTeam(th.serve(22)))
}

// TestTeamPushIgnoresAServerReplacedMidPoll is the check-to-use race on the
// poll, landed deterministically: herdr's socket is replaced while a list is
// being answered, after the poll's identity check. Every call after that
// dials the replacement, which the next daemon belongs to, and finds so once
// connected, before it sends anything: nothing more is asked of the original,
// nothing is pushed to either server, and the replacement is asked nothing at
// all, by the poll or by focus, whose every dial checks the same way. A swap
// while the panes' models are asked caches no snapshot either.
func TestTeamPushIgnoresAServerReplacedMidPoll(t *testing.T) {
	t.Parallel()

	// A tabs team, so the poll lists tabs too.
	tabsTeam := "[projects.ha]\nrepo = \"/x\"\nshort = \"ha\"\nlayout = \"tabs\"\n"
	for name, tc := range map[string]struct {
		during, config string
		models         bool
	}{
		"while the panes are listed":      {during: "pane.list"},
		"while the workspaces are listed": {during: "workspace.list"},
		"while the tabs are listed":       {during: "tab.list", config: tabsTeam},
		"while the agents are listed":     {during: "agent.list"},
		// Two rows with no shell: the second read dials the replacement.
		"while a worker's shell is read": {during: "pane.process_info"},
		// No rows, so only the model asks call it: the second agent pane's
		// dials the replacement. The swap must finish inside the first ask's
		// 1 s ModelBudget, not herdr's CallTimeout, or the second is not asked.
		"while a pane's model is asked": {during: "pane.process_info", models: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			during := tc.during
			th := newTeamHerdr(t)
			original := th.srv
			swap, swapped := make(chan struct{}), make(chan struct{})
			var first sync.Once
			th.onCall = func(method string) {
				if method == during {
					first.Do(func() { close(swap) })
					<-swapped // every such call waits, focus's pane.list included
				}
			}
			dir := shortDir(t)
			switch {
			case tc.models:
				th.mu.Lock()
				th.procs = map[string][]map[string]any{
					"wN:p1": {agentProc(1001, "claude", "--model", "opus")},
					"wN:p2": {agentProc(1002, "claude", "--model", "sonnet")},
				}
				th.mu.Unlock()
			case during == "pane.process_info":
				bindWorker(t, dir, th, "coders-1", "wN:p1")
				bindWorker(t, dir, th, "coders-2", "wN:p2")
			}
			o := teamOpts(t, dir, th, configDir(t, tc.config))
			o.Poll = time.Hour // only the poll at start runs
			r := serving(t, o, th.fakeHerdr)

			select {
			case <-swap:
			case <-time.After(ready):
				t.Fatalf("the daemon did not call %s", during)
			}
			require.NoError(t, os.Remove(th.path))
			replacement := herdrtest.StartAt(t, th.path, th.serveTeam(th.serve(22)))
			close(swapped)

			time.Sleep(time.Second)
			assert.Empty(t, pushes(t, original), "nothing is pushed across the swap")
			assert.Empty(t, panePushes(t, original), "no ctx display is pushed across the swap")
			if during == "pane.list" {
				assert.Zero(t, lists(original), "workspace.list, dialled after the swap, never reaches the original")
			}
			replacement.Settle(t)
			assert.Empty(t, replacement.Requests(), "the replacement is never asked anything, above all no push")
			if tc.models {
				_, err := daemon.Snapshot(t.Context(), socketPath(t, dir, th.path))
				require.ErrorIs(t, err, daemon.ErrNotReady, "no snapshot is cached from a poll the swap ended")
			}
			assert.True(t, r.alive(), "the poll stops; Run's own check ends the daemon")
		})
	}
}

// TestCtxPushStopsWhenTheServerIsReplaced is the same race on the ctx
// display: herdr's socket is replaced while the first pane push is being
// answered. That push lands on the original, which it was connected to;
// the next one dials the replacement and finds so once connected, before it
// sends anything, and the poll stops: the replacement never sees a team call.
func TestCtxPushStopsWhenTheServerIsReplaced(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	original := th.srv
	swap, swapped := make(chan struct{}), make(chan struct{})
	var first sync.Once
	th.onCall = func(method string) {
		if method == "pane.report_metadata" {
			first.Do(func() { close(swap) })
			<-swapped
		}
	}
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, "")
	o.Poll = time.Hour // only the poll at start runs
	o.Logger = zerolog.New(&logs)
	r := serving(t, o, th.fakeHerdr)

	select {
	case <-swap:
	case <-time.After(ready):
		t.Fatal("the daemon pushed no ctx display")
	}
	require.NoError(t, os.Remove(th.path))
	replacement := herdrtest.StartAt(t, th.path, th.serveTeam(th.serve(22)))
	close(swapped)

	time.Sleep(time.Second)
	n := 0
	for _, params := range panePushes(t, original) {
		n += len(params)
	}
	assert.Equal(t, 1, n, "only the push in flight at the swap reaches the original")
	replacement.Settle(t)
	assert.Empty(t, teamCalls(replacement), "the team poll pushes the replacement nothing")
	assert.Contains(t, logs.String(), "team poll stops", "the first push to find the replacement stops the poll")
	assert.NotContains(t, logs.String(), `"push the ctx display"`, "rather than trying every pane after it")
	assert.True(t, r.alive(), "the poll stops; Run's own check ends the daemon")
}

// TestTeamPushSurvivesAFailingPush: herdr refusing one push (a workspace or
// pane closed between the list and the push) is logged with the workspace or
// pane, and does not stop the others, in that poll or the next.
func TestTeamPushSurvivesAFailingPush(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.refuse["wN"] = true
	th.refuse["wN:p1"] = true
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, "")
	o.Logger = zerolog.New(&logs)
	r := serving(t, o, th.fakeHerdr)

	polled(t, th.srv, 2)
	got := pushes(t, th.srv)
	assert.GreaterOrEqual(t, len(got["wN"]), 2, "the refused push is tried every poll")
	assert.GreaterOrEqual(t, len(got["wY"]), 2)
	assert.GreaterOrEqual(t, len(got["w0"]), 2)
	assert.Contains(t, logs.String(), `"workspace":"wN"`)
	assert.Contains(t, logs.String(), "workspace_not_found")
	ctx := panePushes(t, th.srv)
	assert.GreaterOrEqual(t, len(ctx["wN:p1"]), 2, "the refused ctx display is tried every poll")
	assert.GreaterOrEqual(t, len(ctx["wN:p2"]), 2, "and the panes after it are still pushed")
	assert.Contains(t, logs.String(), `"pane":"wN:p1"`)
	assert.Contains(t, logs.String(), "pane_not_found")
	assert.True(t, r.alive())
}

// TestTeamPushFollowsARelabel: the model is rebuilt every poll, so a space
// renamed away from ◆ stops receiving $team (the TTL clears what it has),
// and a workspace renamed to a group space starts.
func TestTeamPushFollowsARelabel(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	serving(t, teamOpts(t, shortDir(t), th, ""), th.fakeHerdr)
	polled(t, th.srv, 1)

	th.relabel("wN", "coders")
	th.relabel("w2", "◆ docs")
	polled(t, th.srv, 1) // a poll in flight may have listed the old labels
	before := pushes(t, th.srv)
	polled(t, th.srv, 2)
	after := pushes(t, th.srv)

	assert.Equal(t, before["wN"], after["wN"], "a space renamed away from ◆ is no longer pushed")
	assert.Greater(t, len(after["w2"]), len(before["w2"]), "a workspace renamed into a group space is pushed")
	for _, p := range after["w2"] {
		assert.JSONEq(t, `{"workspace_id":"w2","source":"agentisan","tokens":{"team":"1 agent · 1 working · 0 ready"},"ttl_ms":9000}`, p)
	}
}

// TestTeamPushReadsTheConfiguredProject: the daemon reads [spaces] project
// once when it starts. No configuration at all, which is the user's case
// today, is the unnamed project; a file that does not resolve is logged and
// the spaces fall back to the unnamed project, since $team does not depend
// on it and focus and Back must keep working. Pushes go out either way.
func TestTeamPushReadsTheConfiguredProject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		file     string // the config file; "" for none, "nodir" for no config dir
		want     string
		wantLogs []string
	}{
		"no config dir":    {file: "nodir", want: ""},
		"no config file":   {want: ""},
		"the key":          {file: "[spaces]\nproject = \"b\"\n[projects.a]\nrepo = \"/x\"\nshort = \"a\"\n[projects.b]\nrepo = \"/y\"\n", want: "b"},
		"the sole project": {file: "[projects.a]\nrepo = \"/x\"\n", want: "a"},
		"several, no key":  {file: "[projects.a]\nrepo = \"/x\"\nshort = \"a\"\n[projects.b]\nrepo = \"/y\"\nshort = \"b\"\n", want: ""},
		"tabs teams, no short": {
			file: "[projects.a]\nrepo = \"/x\"\n[projects.b]\nrepo = \"/y\"\n", want: "", wantLogs: []string{`"level":"error"`, "projects.a.short"},
		},
		"an unknown project":  {file: "[spaces]\nproject = \"c\"\n[projects.a]\nrepo = \"/x\"\n", want: "", wantLogs: []string{`"level":"error"`, "no such project"}},
		"an unsupported file": {file: "-", want: "", wantLogs: []string{`"level":"error"`, "schema_version"}},
		"a non-ASCII project": {file: "[spaces]\nproject = \"ä\"\n[projects.a]\nrepo = \"/x\"\n", want: "", wantLogs: []string{`"level":"error"`}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			switch tc.file {
			case "nodir":
				dir = ""
			case "":
			case "-":
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 2\n"), 0o600))
			default:
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+tc.file), 0o600))
			}
			th := newTeamHerdr(t)
			var logs lockedLog
			o := teamOpts(t, shortDir(t), th, dir)
			o.Logger = zerolog.New(&logs)
			serving(t, o, th.fakeHerdr)

			polled(t, th.srv, 1)
			assert.Len(t, pushes(t, th.srv), 3, "the pushes do not depend on the project")
			assert.Contains(t, logs.String(), fmt.Sprintf(`"project":%q`, tc.want))
			for _, s := range tc.wantLogs {
				assert.Contains(t, logs.String(), s)
			}
		})
	}
}

// lockedLog is a log sink safe to read while the daemon writes it.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p) //nolint:wrapcheck // test sink
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// configDir is a config dir holding file as herdr-agentisan.toml after its
// schema_version; "" for a dir with no file.
func configDir(t *testing.T, file string) string {
	t.Helper()
	dir := t.TempDir()
	if file != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+file), 0o600))
	}
	return dir
}

// TestRunPushesTeamToEachTabsTeam is A19's $team on the team model, beside
// the ◆ spaces: each tabs team's workspace gets one count of its typed
// worker tabs, the boss's tab and tabs that do not parse left out; a team
// with only its boss's tab counts zero. A second workspace with the team's
// label is not the team: it gets nothing and is logged once, not every poll.
func TestRunPushesTeamToEachTabsTeam(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	pane := func(id, ws, tab, status string) map[string]any {
		return map[string]any{"pane_id": id, "workspace_id": ws, "tab_id": tab, "agent": "claude", "agent_status": status, "focused": false}
	}
	tab := func(id, ws, label string) map[string]any {
		return map[string]any{"tab_id": id, "workspace_id": ws, "number": 1, "label": label, "focused": false, "pane_count": 1, "agent_status": "idle"}
	}
	th.mu.Lock()
	th.workspaces = append(th.workspaces,
		herdr.WorkspaceInfo{WorkspaceID: "w14", Label: "herdr-agentisan"},
		herdr.WorkspaceInfo{WorkspaceID: "w15", Label: "herdr-agentisan"},
		herdr.WorkspaceInfo{WorkspaceID: "w20", Label: "assay"},
	)
	th.tabs = []map[string]any{
		tab("w14:t1", "w14", "ha-boss"), tab("w14:t2", "w14", "ha-coder-1"), tab("w14:t3", "w14", "ha-qa-1"),
		tab("w14:t4", "w14", "notes"), tab("w15:t1", "w15", "ha-coder-2"), tab("w20:t1", "w20", "as-boss"),
	}
	th.panes = append(th.panes,
		pane("w14:p1", "w14", "w14:t1", "working"), pane("w14:p2", "w14", "w14:t2", "working"),
		pane("w14:p3", "w14", "w14:t3", "blocked"), pane("w14:p4", "w14", "w14:t4", "working"),
		pane("w15:p1", "w15", "w15:t1", "working"), pane("w20:p1", "w20", "w20:t1", "idle"),
	)
	th.mu.Unlock()
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, configDir(t, `[spaces]
project = "agentisan"
[projects.agentisan]
repo = "/x"
[projects.herdr-agentisan]
repo = "/y"
short = "ha"
[projects.assay]
repo = "/z"
short = "as"
`))
	o.Logger = zerolog.New(&logs)
	serving(t, o, th.fakeHerdr)

	polled(t, th.srv, 3)
	got := pushes(t, th.srv)
	assert.ElementsMatch(t, []string{"wN", "wY", "w0", "w14", "w20"}, slices.Collect(maps.Keys(got)), "the group spaces and each team's workspace")
	want := map[string]string{
		"w14": `{"workspace_id":"w14","source":"agentisan","tokens":{"team":"2 agents \u00b7 1 working \u00b7 0 ready \u00b7 1 blocked"},"ttl_ms":9000}`,
		"w20": `{"workspace_id":"w20","source":"agentisan","tokens":{"team":"0 agents \u00b7 0 working \u00b7 0 ready"},"ttl_ms":9000}`,
	}
	maps.Copy(want, wantPushes)
	for ws, params := range got {
		for _, p := range params {
			assert.JSONEq(t, want[ws], p)
		}
	}
	assert.Equal(t, 1, strings.Count(logs.String(), `"ignored":["w15"]`), "the duplicate is logged once: %s", logs.String())
	assert.Contains(t, logs.String(), `"tabs_teams":["assay","herdr-agentisan"]`)
}

// TestTeamPollCostsAFailedTabList: with a tabs team configured, a tab.list
// herdr answers wrongly costs the poll, as a failed pane or workspace list
// does: nothing is pushed, the spaces included, it is logged, and the next
// poll tries again.
func TestTeamPollCostsAFailedTabList(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.mu.Lock()
	th.tabs = nil // tab_list with no tabs: a protocol error
	th.mu.Unlock()
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, configDir(t, "[projects.ha]\nrepo = \"/x\"\nshort = \"ha\"\nlayout = \"tabs\"\n"))
	o.Logger = zerolog.New(&logs)
	r := serving(t, o, th.fakeHerdr)

	polled(t, th.srv, 2)
	assert.Empty(t, pushes(t, th.srv))
	assert.Contains(t, logs.String(), "team poll: list tabs")
	assert.True(t, r.alive())
}

// TestTeamPollListsNoTabsWithoutATabsTeam: with no tabs team configured the
// poll never sends tab.list, so a herdr whose tab.list fails cannot cost the
// spaces' poll.
func TestTeamPollListsNoTabsWithoutATabsTeam(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	serving(t, teamOpts(t, shortDir(t), th, ""), th.fakeHerdr)
	polled(t, th.srv, 2)
	for _, r := range th.srv.Requests() {
		assert.NotEqual(t, "tab.list", r.Method, "no tabs team, no tab.list")
	}
}

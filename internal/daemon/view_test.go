// safety-scan: allow (test fixture: herdr's agent view sorts by a field named token, so `"token":"order_ha"` is the wire shape; the values are token names, not secrets)
package daemon_test

import (
	"maps"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// The exact params of the daemon's view calls, written out by hand: the
// set for the spaces and for the herdr-agentisan tabs team of viewTeams, and
// the clear.
const (
	viewSpaces = `{"source":"plugin:nerdsrun.agentisan","label":"agentisan","filter":{"op":"any","filters":[{"op":"in","field":"workspace_id","values":["w0","w12","wJ","wN","wY"]},{"op":"in","field":"status","values":["blocked"]}]},"sort":[{"field":{"token":"order-spaces"},"order":"asc"}]}`
	viewTabs   = `{"source":"plugin:nerdsrun.agentisan","label":"herdr-agentisan","filter":{"op":"any","filters":[{"op":"in","field":"workspace_id","values":["w14"]},{"op":"in","field":"status","values":["blocked"]}]},"sort":[{"field":{"token":"order_ha"},"order":"asc"}]}`
	viewClear  = `{"source":"plugin:nerdsrun.agentisan"}`
)

// viewTeams is liveSpaces, which the agentisan project owns, beside the
// herdr-agentisan tabs team, and the configuration that names both.
func viewTeams(t *testing.T) (*teamHerdr, string) {
	t.Helper()
	th := newTeamHerdr(t)
	tab := func(id, label string) map[string]any {
		return map[string]any{"tab_id": id, "workspace_id": "w14", "number": 1, "label": label, "focused": false, "pane_count": 1, "agent_status": "idle"}
	}
	pane := func(id, tab string) map[string]any {
		return map[string]any{"pane_id": id, "workspace_id": "w14", "tab_id": tab, "agent": "claude", "agent_status": "working", "focused": false}
	}
	th.mu.Lock()
	th.workspaces = append(th.workspaces, herdr.WorkspaceInfo{WorkspaceID: "w14", Label: "herdr-agentisan"})
	th.tabs = []map[string]any{tab("w14:t1", "ha-boss"), tab("w14:t2", "ha-coder-1")}
	th.panes = append(th.panes, pane("w14:p1", "w14:t1"), pane("w14:p2", "w14:t2"))
	th.mu.Unlock()
	return th, configDir(t, `[spaces]
project = "agentisan"
[projects.agentisan]
repo = "/x"
[projects.herdr-agentisan]
repo = "/y"
short = "ha"
`)
}

// viewCall is one agent view request: its method and params.
type viewCall struct {
	method, params string
}

// viewCalls returns the agent view requests srv got, in order.
func viewCalls(srv *herdrtest.Server) []viewCall {
	var out []viewCall
	for _, r := range srv.Requests() {
		if r.Method == "agent.view.set" || r.Method == "agent.view.clear" {
			out = append(out, viewCall{r.Method, string(r.Params)})
		}
	}
	return out
}

// awaitViews waits until srv has n agent view requests, and returns them.
func awaitViews(t *testing.T, srv *herdrtest.Server, n int) []viewCall {
	t.Helper()
	require.Eventually(t, func() bool { return len(viewCalls(srv)) >= n }, ready, 5*time.Millisecond,
		"the daemon did not send %d agent view requests: %v", n, viewCalls(srv))
	return viewCalls(srv)
}

// assertSet asserts c sets the view want.
func assertSet(t *testing.T, c viewCall, want string) {
	t.Helper()
	assert.Equal(t, "agent.view.set", c.method)
	assert.JSONEq(t, want, c.params)
}

// assertClear asserts c clears the daemon's view.
func assertClear(t *testing.T, c viewCall) {
	t.Helper()
	assert.Equal(t, "agent.view.clear", c.method)
	assert.JSONEq(t, viewClear, c.params)
}

// TestRunSetsTheFocusedTeamsView is the A28 addendum's view: focusing a pane
// of a team sets the agent view to that team, every workspace it holds or a
// blocked agent anywhere, labelled with its project and sorted by its order
// key; focusing the other team's pane sets that one's, and coming back sets
// the first again. The source is the plugin's own, so herdr drops the view
// with the plugin.
func TestRunSetsTheFocusedTeamsView(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	assertSet(t, awaitViews(t, th.srv, 1)[0], viewSpaces)
	s.lines <- herdrtest.PaneFocused("w14:p2", "w14")
	assertSet(t, awaitViews(t, th.srv, 2)[1], viewTabs)
	s.lines <- herdrtest.PaneFocused("wJ:p1", "wJ")
	assertSet(t, awaitViews(t, th.srv, 3)[2], viewSpaces)
	assert.Contains(t, viewSpaces, `{"source":"plugin:`+plugin.ID+`"`, "the plugin's own source")
}

// TestRunClearsTheViewOutsideEveryTeam: focus in a workspace no team holds
// clears the view, bringing back herdr's own order, the first decision
// after a start included, since a daemon before this one may have left a
// view behind.
func TestRunClearsTheViewOutsideEveryTeam(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("w2:p1", "w2")
	assertClear(t, awaitViews(t, th.srv, 1)[0])
	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	assertSet(t, awaitViews(t, th.srv, 2)[1], viewSpaces)
	s.lines <- herdrtest.PaneFocused("w2:p1", "w2")
	assertClear(t, awaitViews(t, th.srv, 3)[2])
}

// TestRunSendsAnIdenticalViewOnce: focus moving within a team, ◆ herdr
// included, and polls that change nothing send nothing more; nor does focus
// moving from one place outside every team to another. Each change after
// them, which the daemon reads in order, shows nothing was sent between.
func TestRunSendsAnIdenticalViewOnce(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	th.mu.Lock()
	th.workspaces = append(th.workspaces, herdr.WorkspaceInfo{WorkspaceID: "w3", Label: "scratch"})
	th.mu.Unlock()
	run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 1)
	for _, f := range [][2]string{{"wN:p2", "wN"}, {"wJ:p1", "wJ"}, {"w12:p1", "w12"}, {"wY:p1", "wY"}, {"wN:p1", "wN"}} {
		s.lines <- herdrtest.PaneFocused(f[0], f[1])
	}
	polled(t, th.srv, 3)
	s.lines <- herdrtest.PaneFocused("w2:p1", "w2")
	calls := awaitViews(t, th.srv, 2)
	require.Len(t, calls, 2, "one set for the team, then the clear: %v", calls)
	assertClear(t, calls[1])

	s.lines <- herdrtest.PaneFocused("w3:p1", "w3")
	polled(t, th.srv, 3)
	s.lines <- herdrtest.PaneFocused("w14:p1", "w14")
	calls = awaitViews(t, th.srv, 3)
	require.Len(t, calls, 3, "no second clear: %v", calls)
	assertSet(t, calls[2], viewTabs)
}

// TestRunSendsTheViewAgainAfterAResync: once herdr drops events the daemon
// had not read, focus is read afresh and the view sent again, identical or
// not: while the daemon was not listening, another source may have replaced
// it.
func TestRunSendsTheViewAgainAfterAResync(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 1)
	s.lines <- herdrtest.EventsLost(s.id)
	th.nextStream(t)
	calls := awaitViews(t, th.srv, 2)
	assertSet(t, calls[0], viewSpaces)
	assertSet(t, calls[1], viewSpaces)
}

// TestRunSendsTheViewAgainAfterAResyncThatCannotList: a resync whose
// pane.list fails reads no focus, yet still forgets the view last sent and
// sends it again: the daemon was not listening, and another source may have
// replaced it meanwhile.
func TestRunSendsTheViewAgainAfterAResyncThatCannotList(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 1)
	th.mu.Lock()
	th.panesErr = true // the polls' lists fail too, so none sends the view
	th.mu.Unlock()
	s.lines <- herdrtest.EventsLost(s.id)
	th.nextStream(t)
	calls := awaitViews(t, th.srv, 2)
	assertSet(t, calls[0], viewSpaces)
	assertSet(t, calls[1], viewSpaces)
}

// TestRunMovesTheViewOnAResyncWithoutWaitingForAPoll: the focus a resync
// reads moves the view at once. Focus moved while the daemon was not
// listening, and with no poll due, the next team's view is still set.
func TestRunMovesTheViewOnAResyncWithoutWaitingForAPoll(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	o := teamOpts(t, shortDir(t), th, cfg)
	o.Poll = time.Hour // one poll at start; then only focus moves the view
	run(t, o)
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	assertSet(t, awaitViews(t, th.srv, 1)[0], viewSpaces)
	th.listFocused("w14:p2")
	s.lines <- herdrtest.EventsLost(s.id)
	th.nextStream(t)
	assertSet(t, awaitViews(t, th.srv, 2)[1], viewTabs)
}

// TestRunSetsTheViewOfTheFocusAtStart: a daemon reads the focused pane as it
// starts, so it sets that team's view on its first poll, before focus moves:
// after daemon-restart the panel shows the team again without a keypress.
func TestRunSetsTheViewOfTheFocusAtStart(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	th.mu.Lock()
	for _, p := range th.panes {
		if p["pane_id"] == "wN:p1" {
			p["focused"] = true
		}
	}
	th.mu.Unlock()
	run(t, teamOpts(t, shortDir(t), th, cfg))
	th.nextStream(t)
	assertSet(t, awaitViews(t, th.srv, 1)[0], viewSpaces)
}

// TestRunFollowsAFocusOnlyThePollSees is A49: herdr can move focus without a
// pane.focused event, as a client shell's own navigation does, and the
// poll's pane list then moves the view within a poll, to a team and out of
// every team alike.
func TestRunFollowsAFocusOnlyThePollSees(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	assertSet(t, awaitViews(t, th.srv, 1)[0], viewSpaces)
	th.listFocused("w14:p2")
	assertSet(t, awaitViews(t, th.srv, 2)[1], viewTabs)
	th.listFocused("w2:p1")
	assertClear(t, awaitViews(t, th.srv, 3)[2])
}

// listFocused makes pane the one pane.list lists focused, with no event.
func (th *teamHerdr) listFocused(pane string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	panes := make([]map[string]any, 0, len(th.panes))
	for _, p := range th.panes {
		p = maps.Clone(p)
		p["focused"] = p["pane_id"] == pane
		panes = append(panes, p)
	}
	th.panes = panes
}

// TestRunTriesTheViewAgainAfterAFailure: a view herdr refuses, as it does
// while the plugin is not linked, is sent again on every poll, and logged
// once for each reason herdr gives, not every poll.
func TestRunTriesTheViewAgainAfterAFailure(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	th.viewErr = "plugin_not_found"
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, cfg)
	o.Logger = zerolog.New(&logs)
	run(t, o)
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 3)
	th.mu.Lock()
	th.viewErr = "plugin_disabled"
	th.mu.Unlock()
	n := len(viewCalls(th.srv))
	awaitViews(t, th.srv, n+3)

	for _, c := range viewCalls(th.srv) {
		assertSet(t, c, viewSpaces)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "refused: plugin_not_found"), "logged once: %s", logs.String())
	assert.Equal(t, 1, strings.Count(logs.String(), "refused: plugin_disabled"), "logged once: %s", logs.String())
}

// TestRunLogsARefusalAgainAfterASuccess: a call that succeeds ends a run of
// failures, so the same refusal coming back, on a new view, is logged again
// rather than taken for the one already logged.
func TestRunLogsARefusalAgainAfterASuccess(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	var sets atomic.Int32
	th.mu.Lock()
	th.onCall = func(method string) { // refused, then set, then refused for good
		if method != "agent.view.set" {
			return
		}
		n := sets.Add(1)
		th.mu.Lock()
		defer th.mu.Unlock()
		th.viewErr = "plugin_not_found"
		if n == 2 {
			th.viewErr = ""
		}
	}
	th.mu.Unlock()
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, cfg)
	o.Logger = zerolog.New(&logs)
	run(t, o)
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 2)
	s.lines <- herdrtest.PaneFocused("w14:p2", "w14")
	// The fourth is the next poll's try, so the third's failure is logged.
	calls := awaitViews(t, th.srv, 4)
	assertSet(t, calls[0], viewSpaces)
	assertSet(t, calls[1], viewSpaces)
	assertSet(t, calls[2], viewTabs)
	assert.Equal(t, 2, strings.Count(logs.String(), "refused: plugin_not_found"), "logged again after the success: %s", logs.String())
}

// TestRunSendsTheViewAgainAfterAFailedCall: after a call that fails, the
// daemon no longer knows the view herdr has, so focus coming back to the
// team of the last view it set sends that view again rather than taking it
// as already there.
func TestRunSendsTheViewAgainAfterAFailedCall(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)

	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 1)
	th.mu.Lock()
	th.viewErr = "server_unavailable"
	th.mu.Unlock()
	s.lines <- herdrtest.PaneFocused("w14:p2", "w14")
	assertSet(t, awaitViews(t, th.srv, 2)[1], viewTabs)
	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	require.Eventually(t, func() bool {
		calls := viewCalls(th.srv)
		last := calls[len(calls)-1]
		return last.method == "agent.view.set" && strings.Contains(last.params, `"label":"agentisan"`)
	}, ready, 5*time.Millisecond, "the spaces' view is sent again: %v", viewCalls(th.srv))
}

// TestCleanStopClearsTheView: a daemon stopped cleanly clears its view, so
// the agents panel goes back to herdr's own order.
func TestCleanStopClearsTheView(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	r := run(t, teamOpts(t, shortDir(t), th, cfg))
	s := th.nextStream(t)
	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 1)

	r.cancel()
	require.NoError(t, r.wait(t, ready))
	th.srv.Settle(t)
	calls := viewCalls(th.srv)
	require.Len(t, calls, 2, "the set, then the clear: %v", calls)
	assertClear(t, calls[1])
}

// TestNoViewClearOnAServerThatIsGone: a daemon whose server is gone sends no
// clear, and logs none failing: the view went with the server. Not to a
// replacement at the path, which the next daemon belongs to, whether the
// daemon exits on finding it or is stopped while it is there, nor to a
// socket that is gone, whose dial would fail before the client's identity
// check. The original gets the set alone: once the server has closed the
// focus stream, a resubscribe finds it gone (herdrtest closes its listener
// before its streams, NERD-5346), so no resync sends the set again.
func TestNoViewClearOnAServerThatIsGone(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stop func(th *teamHerdr, r *running)
		want error
	}{
		"herdr is replaced at its path": {stop: func(th *teamHerdr, _ *running) { th.replace() }, want: daemon.ErrHerdrGone},
		"herdr removes its socket":      {stop: func(th *teamHerdr, _ *running) { th.stopKeepingRequests() }, want: daemon.ErrHerdrGone},
		"a stop while a replacement is there": {stop: func(th *teamHerdr, r *running) {
			th.replace()
			r.cancel()
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th, cfg := viewTeams(t)
			var logs lockedLog
			o := teamOpts(t, shortDir(t), th, cfg)
			o.Logger = zerolog.New(&logs)
			r := run(t, o)
			original := th.srv
			s := th.nextStream(t)
			s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
			awaitViews(t, original, 1)

			tc.stop(th, r)
			err := r.wait(t, ready)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			original.Settle(t)
			calls := viewCalls(original)
			require.Len(t, calls, 1, "the set alone: %v", calls)
			assertSet(t, calls[0], viewSpaces)
			if th.srv != original {
				th.srv.Settle(t)
				assert.Empty(t, viewCalls(th.srv), "the replacement gets no view call")
			}
			assert.NotContains(t, logs.String(), "clear the agent view", "nothing to clear, nothing to log")
		})
	}
}

// TestViewNeverReachesAReplacement: every view call dials through the
// identity check (A3). herdr's socket is replaced while the daemon's own
// server still streams focus, before the poll notices: the view the next
// focus event makes due finds the replacement on dialling, and the sync
// stops without sending it anything.
func TestViewNeverReachesAReplacement(t *testing.T) {
	t.Parallel()
	th, cfg := viewTeams(t)
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, cfg)
	o.Poll = time.Hour // one poll at start; then only focus moves the view
	o.Logger = zerolog.New(&logs)
	r := run(t, o)
	s := th.nextStream(t)
	s.lines <- herdrtest.PaneFocused("wN:p1", "wN")
	awaitViews(t, th.srv, 1)

	require.NoError(t, os.Remove(th.path)) // the original serves on, unlinked
	replacement := herdrtest.StartAt(t, th.path, th.serveTeam(th.serve(22)))
	s.lines <- herdrtest.PaneFocused("w14:p2", "w14")
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "agent view stops") }, ready, 5*time.Millisecond,
		"the sync never found the replacement: %s", logs.String())
	replacement.Settle(t)
	assert.Empty(t, viewCalls(replacement), "the replacement gets no view call")
	assert.True(t, r.alive())
}

// syncModel is a model of the ◆ spaces, which hold wN, beside w2, which no
// team holds: the view is a set in wN, a clear in w2.
func syncModel() team.Model {
	return team.Build(team.Layout{Workspaces: []herdr.WorkspaceInfo{
		{WorkspaceID: "wN", Label: "◆ coders"},
		{WorkspaceID: "w2", Label: "notes"},
	}}, team.Spaces{}, nil)
}

// TestViewSyncKeepsWhatItSentForgottenAfterARacingForget: a call that fails
// while another is on its way forgets what was sent, and the one on its way,
// once it succeeds, must not record its view over that forget: the next
// decision sends again.
func TestViewSyncKeepsWhatItSentForgottenAfterARacingForget(t *testing.T) {
	t.Parallel()
	v := daemon.NewViewSync()
	v.Polled(syncModel(), "", v.Seen())
	v.Focus("wN")

	_, sent, ok := v.Next()
	require.True(t, ok, "the first decision sends")
	v.Forget()
	sent()
	_, _, ok = v.Next()
	assert.True(t, ok, "a send that raced a forget records nothing, so the view is sent again")

	_, sent, _ = v.Next()
	sent()
	_, _, ok = v.Next()
	assert.False(t, ok, "without a forget, what was sent is not sent again")
}

// TestViewSyncTakesFocusFromThePoll is A49: herdr can move focus without a
// pane.focused event, so the poll's pane list is a source of focus too, but
// an event the collector recorded after the poll read its count is newer
// than the list and wins. A list with no pane focused changes nothing, and a
// poll forgets nothing, so one that finds focus where it was sends nothing.
func TestViewSyncTakesFocusFromThePoll(t *testing.T) {
	t.Parallel()
	m := syncModel()

	t.Run("a focus only the poll sees", func(t *testing.T) {
		t.Parallel()
		v := daemon.NewViewSync()
		v.Polled(m, "wN", v.Seen())
		clear, sent, ok := v.Next()
		require.True(t, ok)
		assert.False(t, clear, "the poll's focus, wN, sets the spaces' view")
		sent()

		v.Polled(m, "w2", v.Seen())
		clear, sent, ok = v.Next()
		require.True(t, ok)
		assert.True(t, clear, "focus moved to w2 with no event")
		sent()

		v.Polled(m, "w2", v.Seen())
		_, _, ok = v.Next()
		assert.False(t, ok, "a poll that finds focus where it was forgets nothing and sends nothing")
		v.Polled(m, "", v.Seen())
		_, _, ok = v.Next()
		assert.False(t, ok, "a list with no pane focused changes nothing")
		v.Forget()
		clear, _, ok = v.Next()
		require.True(t, ok, "after a failed call the focus is still known")
		assert.True(t, clear, "still w2, not lost to the list with none")
	})

	for name, record := range map[string]func(v daemon.ViewSync){
		"an event": func(v daemon.ViewSync) { v.Focus("wN") },
		"a resync": func(v daemon.ViewSync) { v.Resynced("wN") },
	} {
		t.Run(name+" after the poll read its count wins", func(t *testing.T) {
			t.Parallel()
			v := daemon.NewViewSync()
			v.Polled(m, "w2", v.Seen())
			seen := v.Seen()
			record(v)
			v.Polled(m, "w2", seen) // listed before the collector recorded wN
			clear, _, ok := v.Next()
			require.True(t, ok)
			assert.False(t, clear, "wN, the collector's focus, not the older list's w2")

			v.Polled(m, "w2", v.Seen()) // a list after it
			clear, _, ok = v.Next()
			require.True(t, ok)
			assert.True(t, clear, "a poll that read the count after the collector takes its focus")
		})
	}
}

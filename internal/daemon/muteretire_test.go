package daemon_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// The poll waits mid-cycle, so it cannot see the closed pane and clear the
	// mute before retire does: this test is the retire-first order. The
	// poll-first order is TestRetireAfterThePollConfirmedLoss.
	holdMutePoll(t, th, "pane.report_metadata", nil, nil)

	got := noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteOldPane})
	assert.JSONEq(t, `{"id":"ha-coder-1","pane_id":"w14:p2","changed":true,"outcome":"retired","mute_cleared":true}`, string(got))
	assert.Equal(t, []string{"ha-coder-2"}, listedMutes(t, s))
	got = noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
	assert.JSONEq(t, `{"id":"ha-coder-1","pane_id":"w14:p2","changed":false,"outcome":"already_retired"}`, string(got))
}

// TestRetireAfterThePollConfirmedLoss is the poll-first order of
// TestRetireClearsTheBindingMute: the poll marks the closed pane's row lost
// and clears its mute, then retire ends the row. Retire's own clear finds
// nothing, so `mute_cleared` is absent: it reports only what retire cleared.
func TestRetireAfterThePollConfirmedLoss(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	closeBetweenPolls(t, th)
	// The list, not the row's status: the poll writes lost before it clears.
	require.Eventually(t, func() bool { return len(listedMutes(t, s)) == 0 }, ready, 5*time.Millisecond,
		"the poll confirmed loss and cleared the mute")
	assert.Equal(t, store.StatusLost, row(t, dir, th, muteWorker).Status)
	waitLine(t, logs, "ha-coder-1 left the team (pane w14:p2)")

	var got map[string]any
	require.NoError(t, json.Unmarshal(noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker}), &got))
	assert.Equal(t, true, got["changed"], "retire still ends the lost row")
	assert.NotContains(t, got, "mute_cleared", "retire cleared nothing, the poll had")
	assert.Equal(t, store.StatusRetired, row(t, dir, th, muteWorker).Status)
	assert.Empty(t, listedMutes(t, s))

	noticeWorkforce(t, s, daemon.OpWorkerSpawn, workforce.SpawnRequest{Selector: workforce.Selector{Project: "ha"}, Group: "coder", Count: 1})
	polled(t, th.srv, 2)
	th.setPane(muteNewPane, "agent_status", "blocked")
	polled(t, th.srv, 2)
	waitLine(t, logs, "ha-coder-1 is blocked")
}

// TestRetireHoldsTheMuteWhileItCloses is the retire-first order of the Gone
// notice (NERD-5730): retire is held at its pane.close, and the worker's mute
// is held, not cleared: a second mute of the worker is refused, so none
// survives the retirement, and the notice of its departure is told. The poll
// first order is TestRetireAfterThePollConfirmedLoss.
func TestRetireHoldsTheMuteWhileItCloses(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	// The poll waits mid-cycle, so it cannot mark the row lost between the
	// close and retire's end of the row, which would clear the mute first.
	releasePoll := holdMutePoll(t, th, "pane.report_metadata", nil, nil)
	var got json.RawMessage
	var retireErr error
	done := make(chan struct{})
	release := holdMutePoll(t, th, "pane.close", nil, func() {
		go func() {
			defer close(done)
			got, retireErr = daemon.Workforce(t.Context(), s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
		}()
	})
	_, err := daemon.Workforce(t.Context(), s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: muteWorker})
	assertUnknownMuteWorker(t, err)
	assert.Equal(t, []string{muteWorker}, listedMutes(t, s), "held until the row has ended")
	release()
	<-done
	require.NoError(t, retireErr)
	assert.JSONEq(t, `{"id":"ha-coder-1","pane_id":"w14:p2","changed":true,"outcome":"retired","mute_cleared":true}`, string(got))
	assert.Empty(t, listedMutes(t, s))
	releasePoll()
	waitLine(t, logs, "ha-coder-1 left the team (pane w14:p2)")
}

// TestMutedDepartureSurvivesAnAbandonedHandoff (NERD-5730, sixth review): the
// outgoing pane closes while its handoff is pending, so no departure is told
// under the mute; abandoning the handoff confirms the loss, which tells it.
func TestMutedDepartureSurvivesAnAbandonedHandoff(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	addMutePane(th, "ha-coder-2", true)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	startMuteHandoff(t, th, dir, "term_"+muteNewPane)
	polled(t, th.srv, 2)
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "left the team")
	noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteNewPane})
	waitLine(t, logs, "ha-coder-1 left the team (pane w14:p2)")
	assert.Empty(t, listedMutes(t, s))
}

// TestMutedDepartureSurvivesAFailedAgentList: a poll that cannot list the
// agents reconciles nothing, so it cannot clear the mute of a worker whose
// pane has closed; it must not tell the departure either, which the mute
// would drop for good. The next poll that reconciles tells it.
func TestMutedDepartureSurvivesAFailedAgentList(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	th.mu.Lock()
	th.agentsErr = true
	th.mu.Unlock()
	polled(t, th.srv, 2)
	require.NoError(t, (herdr.Client{SocketPath: th.path}).ClosePane(t.Context(), muteOldPane))
	polled(t, th.srv, 3)
	assert.Equal(t, []string{muteWorker}, listedMutes(t, s), "no loss is confirmed without the agent list")
	th.mu.Lock()
	th.agentsErr = false
	th.mu.Unlock()
	waitLine(t, logs, "ha-coder-1 left the team (pane w14:p2)")
}

// closeBetweenPolls closes the worker's pane while a poll waits after its
// lists, so no poll reads the panes before the close and the agents after it:
// that one sees the agent gone from a pane still listed, and tells its
// departure under the mute, which drops it, leaving the next poll nothing to
// tell.
func closeBetweenPolls(t *testing.T, th *teamHerdr) {
	t.Helper()
	release := holdMutePoll(t, th, "pane.report_metadata", nil, nil)
	require.NoError(t, (herdr.Client{SocketPath: th.path}).ClosePane(t.Context(), muteOldPane))
	release()
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

	// Retire-first again: the poll waits, so it cannot mark the closed pane's row lost first.
	holdMutePoll(t, th, "pane.report_metadata", nil, nil)
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
	holdMutePoll(t, th, "pane.report_metadata", nil, nil)

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

// Holding a poll before publication leaves the previous model available to the socket.
func holdMutePoll(t *testing.T, th *teamHerdr, method string, when func() bool, afterArming func()) func() {
	t.Helper()
	arrived := make(chan struct{})
	resume := make(chan struct{})
	var paused, released sync.Once
	release := func() { released.Do(func() { close(resume) }) }
	t.Cleanup(release)
	th.mu.Lock()
	th.onCall = func(called string) {
		if called == method && (when == nil || when()) {
			paused.Do(func() { close(arrived); <-resume })
		}
	}
	th.mu.Unlock()
	if afterArming != nil {
		afterArming()
	}
	select {
	case <-arrived:
	case <-time.After(ready):
		t.Fatalf("the team poll did not pause at %s", method)
	}
	return release
}

func TestLostBindingCannotBeMutedFromTheOldPoll(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	st := stateStore(t, dir, th)
	s, _ := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	holdMutePoll(t, th, "workspace.report_metadata", func() bool {
		r, err := st.WorkerRow(t.Context(), muteWorker)
		return err == nil && r.Status == store.StatusLost
	}, func() {
		require.NoError(t, (herdr.Client{SocketPath: th.path}).ClosePane(t.Context(), muteOldPane))
	})

	require.Empty(t, listedMutes(t, s), "the poll marked the row lost and cleared its mute")
	_, err := daemon.Workforce(t.Context(), s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: muteWorker})
	assertUnknownMuteWorker(t, err)
	assert.Empty(t, listedMutes(t, s))
}

func TestStaleUnboundPaneCannotMuteAReusedWorkerID(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	st := stateStore(t, dir, th)
	r, err := st.WorkerRow(t.Context(), muteWorker)
	require.NoError(t, err)
	require.NoError(t, st.MarkWorkerLost(t.Context(), r.Worker))
	s, logs := serveMuteWorkers(t, th, dir)
	release := holdMutePoll(t, th, "pane.report_metadata", nil, nil)
	require.NoError(t, (herdr.Client{SocketPath: th.path}).ClosePane(t.Context(), muteOldPane))
	noticeWorkforce(t, s, daemon.OpWorkerSpawn, workforce.SpawnRequest{
		Selector: workforce.Selector{Project: "ha"}, Group: "coder", Count: 1,
	})
	require.Equal(t, muteNewPane, row(t, dir, th, muteWorker).Pane)
	_, err = daemon.Workforce(t.Context(), s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: muteWorker})
	assertUnknownMuteWorker(t, err)
	assert.Empty(t, listedMutes(t, s))
	release()

	polled(t, th.srv, 2)
	th.setPane(muteNewPane, "agent_status", "blocked")
	polled(t, th.srv, 2)
	assert.Contains(t, noticeLines(logs), "ha-coder-1 is blocked")
}

// TestMuteThroughPendingHandoff: the row's mute follows it through a handoff.
// While the handoff is pending the replacement is an unbound pane, which no
// mute can name; after the ack it is the row's pane again, still muted.
func TestMuteThroughPendingHandoff(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	addMutePane(th, "ha-coder-2", true)
	th.setPane(muteNewPane, "tokens", map[string]string{"item": "NERD-5730", "stage": "verify"})
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	startMuteHandoff(t, th, dir, "term_"+muteNewPane)
	_, err := daemon.Workforce(t.Context(), s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: muteWorker})
	assertUnknownMuteWorker(t, err)
	noticeWorkforce(t, s, daemon.OpHandoffAck, workforce.HandoffAckRequest{Target: muteNewPane})
	require.Equal(t, muteNewPane, row(t, dir, th, muteWorker).Pane)
	polled(t, th.srv, 2)

	th.setPane(muteNewPane, "agent_status", "blocked")
	polled(t, th.srv, 2)
	assert.Equal(t, []string{muteWorker}, listedMutes(t, s))
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "ha-coder-1 is blocked")
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "left the team", "the old pane's Gone stays muted through the handoff")
}

// The pending handoff prevents a lost transition when the outgoing pane closes.
// The new agent uses the worker name while the replacement still has no binding.
func startMuteHandoff(t *testing.T, th *teamHerdr, dir, replacementTerminal string) {
	t.Helper()
	st := stateStore(t, dir, th)
	_, err := st.BeginHandoff(t.Context(), muteWorker, muteNewPane, replacementTerminal, time.Now())
	require.NoError(t, err)
	require.NoError(t, (herdr.Client{SocketPath: th.path}).ClosePane(t.Context(), muteOldPane))
	th.setAgents(muteAgent("w14:p1", "ha-boss"), muteAgent(muteNewPane, muteWorker))
	polled(t, th.srv, 2)
}

func TestMutePrefersBoundWorkerOverEarlierUnboundNamesake(t *testing.T) {
	t.Parallel()
	const worker = "ha-qa-1"
	th := newTeamHerdr(t)
	withTeam(th)
	addMutePane(th, worker, true)
	// The coder pane takes the name, but the later QA pane owns the row.
	th.setAgents(muteAgent("w14:p1", "ha-boss"), muteAgent(muteOldPane, worker), muteAgent(muteNewPane, "qa-session"))
	th.setPane(muteOldPane, "tokens", map[string]string{"stage": "unbound pane"})
	th.setPane(muteNewPane, "tokens", map[string]string{"stage": "bound pane"})
	dir := shortDir(t)
	execDB(t, dir, th, `INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, terminal_id)
VALUES (?, 'ha', 'qa', ?, 7, 'active', 0, ?)`, worker, muteNewPane, "term_"+muteNewPane)
	s, logs := serveMuteWorkersWithGroups(t, th, dir, "[projects.ha.groups.qa]\nworkers = 0\nslots = 1\n")

	muteForThreeHours(t, s, worker)
	require.Equal(t, []string{worker}, listedMutes(t, s))
	th.setPane(muteOldPane, "agent_status", "blocked")
	th.setPane(muteNewPane, "agent_status", "blocked")
	polled(t, th.srv, 2)
	assert.Contains(t, noticeLines(logs), worker+" is blocked · unbound pane")
	assert.NotContains(t, noticeLines(logs), worker+" is blocked · bound pane", "the row's generation is muted")

	got := noticeWorkforce(t, s, daemon.OpNoticesUnmute, daemon.UnmuteRequest{Worker: worker})
	assert.JSONEq(t, `{"worker":"ha-qa-1","ended":true}`, string(got))
	assert.Empty(t, listedMutes(t, s))
	th.setPane(muteNewPane, "agent_status", "idle")
	polled(t, th.srv, 2)
	th.setPane(muteNewPane, "agent_status", "blocked")
	waitLine(t, logs, worker+" is blocked · bound pane")
}

func TestPendingReplacementNoticeIsDeliveredBeforeAck(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	addMutePane(th, "ha-coder-2", true)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	startMuteHandoff(t, th, dir, "term_"+muteNewPane)
	th.setPane(muteNewPane, "agent_status", "blocked")
	polled(t, th.srv, 2)
	assert.Contains(t, noticeLines(logs), "ha-coder-1 is blocked")
}

func TestMutedBoundWorkerTurnEndsWithoutNotice(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	boss := named("w14:p1", "ha-boss")
	th.setAgents(boss, turned(muteOldPane, muteWorker, 1))
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	th.setAgents(boss, turned(muteOldPane, muteWorker, 2))
	polled(t, th.srv, 2)
	require.NotContains(t, strings.Join(noticeLines(logs), "\n"), "ha-coder-1 finished a turn")

	noticeWorkforce(t, s, daemon.OpNoticesUnmute, daemon.UnmuteRequest{Worker: muteWorker})
	th.setAgents(boss, turned(muteOldPane, muteWorker, 3))
	waitLine(t, logs, "ha-coder-1 finished a turn")
}

func TestRetiringAMutedWorkerDeliversGoneNotice(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
	waitLine(t, logs, "ha-coder-1 left the team (pane w14:p2)")
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
	return serveMuteWorkersWithGroups(t, th, dir, "")
}

func serveMuteWorkersWithGroups(t *testing.T, th *teamHerdr, dir, groups string) (string, *lockedLog) {
	t.Helper()
	normal := th.serve(22)
	th.mu.Lock()
	th.other = func(r herdrtest.Request) herdrtest.Reply {
		return muteWorkerHerdr(th, normal, r)
	}
	th.mu.Unlock()
	logs := &lockedLog{}
	o := teamOpts(t, dir, th, haNotices(t, groups+"[notices]\ngap = 0\nrepeat = 0\n"))
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

// TestConfirmedLossOfAMutedWorkerDeliversGone: the poll that confirms a muted
// worker lost clears its mute before it builds that poll's notices, so the
// worker's departure is told, as at retire.
func TestConfirmedLossOfAMutedWorkerDeliversGone(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	closeBetweenPolls(t, th)
	waitLine(t, logs, "ha-coder-1 left the team")
	assert.Empty(t, listedMutes(t, s))
}

// TestRetireDoesNotReleaseABlockedNoticeSampledUnderTheMute (NERD-5730,
// seventh review): a poll samples the muted worker blocked and waits for its
// panes' processes; retire ends the row meanwhile. The notice the poll then
// batches was sampled under the mute, so it stays dropped; only Gone passes.
func TestRetireDoesNotReleaseABlockedNoticeSampledUnderTheMute(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	var after atomic.Int64
	release := holdMutePoll(t, th, "pane.process_info", func() bool { return after.Load() > 0 && int64(lists(th.srv)) > after.Load() }, func() {
		th.setPane(muteOldPane, "agent_status", "blocked")
		after.Store(int64(lists(th.srv)))
	})
	noticeWorkforce(t, s, daemon.OpWorkerRetire, workforce.RetireRequest{Target: muteWorker})
	release()
	waitLine(t, logs, "ha-coder-1 left the team (pane w14:p2)")
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "ha-coder-1 is blocked")
}

// TestMutedExitStillTellsTheDepartureAtLoss (NERD-5730, seventh review): the
// muted worker's agent exits to a shell, which the mute would drop for good;
// the departure waits, and is told when the pane closes and the loss is confirmed.
func TestMutedExitStillTellsTheDepartureAtLoss(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	s, logs := serveMuteWorkers(t, th, dir)
	muteForThreeHours(t, s, muteWorker)
	th.setPane(muteOldPane, "agent", nil)
	th.setPane(muteOldPane, "agent_status", "unknown")
	th.mu.Lock()
	th.agents = slices.DeleteFunc(slices.Clone(th.agents), func(a map[string]any) bool { return a["pane_id"] == muteOldPane })
	th.mu.Unlock()
	polled(t, th.srv, 2)
	assert.NotContains(t, strings.Join(noticeLines(logs), "\n"), "agent exited", "muted")
	closeBetweenPolls(t, th)
	waitLine(t, logs, "ha-coder-1 left the team (pane w14:p2)")
	assert.Empty(t, listedMutes(t, s))
}

// TestAnUnboundPaneCannotBeMuted: only a bound worker's row can be muted (the
// owner's ruling, NERD-5730). Muting an unbound agent, by its name or its pane
// id, is refused with a message that says so, and its notices still come.
func TestAnUnboundPaneCannotBeMuted(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	addMutePane(th, "ha-coder-2", true)
	th.setPane(muteNewPane, "tokens", map[string]string{"stage": "scratch"})
	s, logs := serveMuteWorkers(t, th, dir)
	_, err := daemon.Workforce(t.Context(), s, daemon.OpNoticesMute, daemon.MuteRequest{Worker: "ha-coder-2"})
	assertUnknownMuteWorker(t, err)
	assert.ErrorContains(t, err, "only a worker bound to a row can be muted")
	assert.Empty(t, listedMutes(t, s))
	th.setPane(muteNewPane, "agent_status", "blocked")
	waitLine(t, logs, "ha-coder-2 is blocked · scratch")
}

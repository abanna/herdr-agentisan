package daemon_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestSendOverTheSocket is send end to end (A30): the op queues the message
// from the sender it reads, records its queued event, wakes the dispatcher,
// which types it into the recipient, and answers with who it took the
// message to be to and from. Each refusal has its code.
func TestSendOverTheSocket(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	events, rec := telemetrytest.NewEvents()
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Team.Events = events
	run(t, o)
	healthy(t, dir, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)

	send := func(t *testing.T, req workforce.SendRequest) workforce.SendResult {
		t.Helper()
		data, err := daemon.Workforce(t.Context(), sock, daemon.OpSend, req)
		require.NoError(t, err)
		var res workforce.SendResult
		require.NoError(t, json.Unmarshal(data, &res))
		return res
	}

	res := send(t, workforce.SendRequest{To: "boss", Body: "secret: done", CallerPane: "w14:p2"})
	assert.Equal(t, workforce.SendResult{
		ID: res.ID, State: store.MessageQueued,
		To: workforce.Party{Kind: store.KindBoss, ID: "ha-boss"}, From: workforce.Party{Kind: store.KindWorker, ID: "ha-coder-1"},
	}, res)
	got := p.next(t)
	assert.Equal(t, typed{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + res.ID + " from ha-coder-1] secret: done"}, got)
	eventuallyState(t, st, res.ID, store.MessageDelivered)
	var queued []telemetrytest.Event
	for _, e := range rec.Events() {
		if e.Name == telemetry.EventMessageQueued {
			queued = append(queued, e)
		}
	}
	assert.Equal(t, []telemetrytest.Event{{Name: telemetry.EventMessageQueued, Attrs: map[string]any{
		"agentisan.message.id": res.ID, "agentisan.message.from.kind": "worker", "agentisan.message.from.id": "ha-coder-1",
		"agentisan.message.to.kind": "boss", "agentisan.message.to.id": "ha-boss",
		"agentisan.message.returned_from": "", "agentisan.message.attempts": int64(0), "agentisan.message.reason": "",
	}}}, queued)
	// .queued's time is the message's own queued time: the commit comes
	// before the event, and a pass may deliver the message in between, so
	// a trail sorted by time still puts .queued first (QA #75, N9).
	at := func(name string) time.Time {
		t.Helper()
		var ts time.Time
		require.Eventually(t, func() bool {
			for _, r := range rec.Records() {
				r.WalkAttributes(func(kv attribute.KeyValue) bool {
					if r.EventName() == name && kv.Key == "agentisan.message.id" && kv.Value.AsString() == res.ID {
						ts = r.Timestamp()
					}
					return true
				})
			}
			return !ts.IsZero()
		}, ready, 10*time.Millisecond, "the %s record", name)
		return ts
	}
	queuedAt, deliveredAt := at(telemetry.EventMessageQueued), at(telemetry.EventMessageDelivered)
	assert.True(t, queuedAt.Equal(stateOf(t, st, res.ID).CreatedAt), "stamped %v, queued %v", queuedAt, stateOf(t, st, res.ID).CreatedAt)
	assert.True(t, queuedAt.Before(deliveredAt), "queued %v, delivered %v", queuedAt, deliveredAt)
	// No record carries the body: read once the delivered record, which the
	// dispatcher emits after its commit, is there too (NERD-5535).
	for _, r := range rec.Records() {
		assert.NotContains(t, r.Body().AsString(), "secret")
	}

	res = send(t, workforce.SendRequest{To: "ha-coder-1", Body: "next", CallerPane: "w14:p1"})
	assert.Equal(t, workforce.Party{Kind: store.KindBoss, ID: "ha-boss"}, res.From, "the boss's pane sends as the boss")
	assert.Equal(t, typed{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + res.ID + " from ha-boss] next"}, p.next(t))

	tests := map[string]struct {
		req  workforce.SendRequest
		code string
	}{
		"a worker no row binds":    {req: workforce.SendRequest{To: "ha-coder-9", Body: "x", CallerPane: "w14:p1"}, code: "unknown_recipient"},
		"boss from no team":        {req: workforce.SendRequest{To: "boss", Body: "x", CallerPane: "w2:p1"}, code: "unknown_recipient"},
		"no sender":                {req: workforce.SendRequest{To: "ha-boss", Body: "x"}, code: "unknown_sender"},
		"an empty body":            {req: workforce.SendRequest{To: "ha-boss", CallerPane: "w14:p2"}, code: "bad_request"},
		"no recipient":             {req: workforce.SendRequest{Body: "x", CallerPane: "w14:p2"}, code: "bad_request"},
		"a body over the cap":      {req: workforce.SendRequest{To: "ha-boss", Body: strings.Repeat("x", store.MaxBody+1), CallerPane: "w14:p2"}, code: "too_large"},
		"a recipient with a break": {req: workforce.SendRequest{To: "ha-boss\n", Body: "x", CallerPane: "w14:p2"}, code: "unknown_recipient"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := daemon.Workforce(t.Context(), sock, daemon.OpSend, tc.req)
			var re *daemon.RequestError
			require.ErrorAs(t, err, &re)
			assert.Equal(t, tc.code, re.Code)
			assert.NotEmpty(t, re.Message)
		})
	}
	n := 0
	for _, e := range rec.Events() {
		if e.Name == telemetry.EventMessageQueued {
			n++
		}
	}
	assert.Equal(t, 2, n, "a refused send records no event")
}

// TestAPaneSendersNoticeFollowsItsTerminal is B3 end to end: a pane that is
// no worker and no boss sends as its terminal, and the notice of its message's
// failure is typed into the pane that shows that terminal when it is
// served, though herdr gave the pane another id meanwhile.
func TestAPaneSendersNoticeFollowsItsTerminal(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	p.setAfter("ha-coder-1", errReply("timeout", "timed out"))
	release := p.hold(t, "ha-coder-1")
	st := stateStore(t, dir, th)
	run(t, teamOpts(t, dir, th, workforceConfig(t)))
	healthy(t, dir, th.fakeHerdr)

	data, err := daemon.Workforce(t.Context(), socketPath(t, dir, th.path), daemon.OpSend,
		workforce.SendRequest{To: "ha-coder-1", Body: "from notes", CallerPane: "w2:p1"})
	require.NoError(t, err)
	var res workforce.SendResult
	require.NoError(t, json.Unmarshal(data, &res))
	assert.Equal(t, workforce.Party{Kind: store.KindPane, ID: "term_w2:p1"}, res.From)
	assert.Equal(t, "ha-coder-1", p.next(t).Target)

	movePane(th, "term_w2:p1", "w2:p7")
	release()
	notice := noticeOf(t, st, messageIDs(t, dir, th), res.ID)
	assert.Equal(t, store.Address{Kind: store.KindPane, ID: "term_w2:p1"}, notice.To)
	got := p.to("w2:p7")
	require.Len(t, got, 1)
	assert.Equal(t, "w2:p7", got[0].Target)
	assert.True(t, strings.HasPrefix(got[0].Text, "[agentisan] message "+res.ID+" to ha-coder-1 may not have been delivered"), got[0].Text)
	assert.Empty(t, p.to("w2:p1"), "never the pane's old id")
}

// TestSendWakesTheDispatcher: a message send queued is typed at once, not
// at the next poll. Only the poll at start runs, and its pass serves the
// boss's message seeded before it; the coder, whom that pass did not list,
// is served only because the send woke the dispatcher.
func TestSendWakesTheDispatcher(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	seeded := sent(t, st, haBoss, coder, "before the start")
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Poll = time.Hour // only the poll at start runs
	serving(t, o, th.fakeHerdr)
	assert.Equal(t, "ha-boss", p.next(t).Target)
	eventuallyState(t, st, seeded.ID, store.MessageDelivered)

	data, err := daemon.Workforce(t.Context(), socketPath(t, dir, th.path), daemon.OpSend,
		workforce.SendRequest{To: "ha-coder-1", Body: "now", CallerPane: "w14:p1"})
	require.NoError(t, err)
	var res workforce.SendResult
	require.NoError(t, json.Unmarshal(data, &res))
	assert.Equal(t, typed{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + res.ID + " from ha-boss] now"}, p.next(t))
}

// TestSendWaitsForTheFirstReconcile: before the team poll has reconciled the
// rows since the daemon started, send refuses not_ready, as the workforce
// ops do (A35): a row a herdr restart left behind would name its sender.
func TestSendWaitsForTheFirstReconcile(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	th.mu.Lock()
	th.agentsErr = true
	th.mu.Unlock()
	run(t, teamOpts(t, dir, th, workforceConfig(t)))
	healthy(t, dir, th.fakeHerdr)
	polled(t, th.srv, 1)

	_, err := daemon.CallWith(t.Context(), socketPath(t, dir, th.path), daemon.OpSend, json.RawMessage(`{"to":"boss","body":"x","caller_pane":"w14:p2"}`), 10*time.Second)
	var re *daemon.RequestError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "not_ready", re.Code)
}

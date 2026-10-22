package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

var (
	coder  = store.Address{Kind: store.KindWorker, ID: "ha-coder-1"}
	haBoss = store.Address{Kind: store.KindBoss, ID: "ha-boss"}
	// aPane is a sender with no other identity, by a terminal no pane
	// shows (B3).
	aPane = store.Address{Kind: store.KindPane, ID: "term_gone"}
	// notes is the pane sender in liveSpaces' plain workspace, w2:p1, by its
	// terminal.
	notes = store.Address{Kind: store.KindPane, ID: "term_w2:p1"}
)

// typed is one prompt herdr typed: the name it was sent to, the pane that
// name resolved to, and the text.
type typed struct{ Target, Pane, Text string }

// prompter answers agent.prompt as herdr does (0.9.5 validate_agent_prompt):
// it resolves the target to the pane agent.list names it on, refuses a name
// no agent has (agent_not_found), a name two or more agents share
// (agent_target_ambiguous) and an empty text, and records what it typed into
// which pane, so a test can tell a prompt to the right name in the wrong
// pane. refuse answers a target before anything is typed; after answers it
// once the text is typed, and a hold makes that answer wait.
type prompter struct {
	th      *teamHerdr
	mu      sync.Mutex
	typed   []typed
	refuse  map[string]string
	after   map[string]herdrtest.Reply
	holds   map[string]chan struct{}
	arrived chan typed
}

func newPrompter(th *teamHerdr) *prompter {
	p := &prompter{th: th, refuse: map[string]string{}, after: map[string]herdrtest.Reply{}, holds: map[string]chan struct{}{}, arrived: make(chan typed, 64)}
	th.mu.Lock()
	defer th.mu.Unlock()
	th.prompt = p.handle
	return p
}

func errReply(code, message string) herdrtest.Reply {
	return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: message}}
}

func (p *prompter) handle(r herdrtest.Request) herdrtest.Reply {
	var params struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(r.Params, &params); err != nil {
		return errReply("invalid_request", err.Error())
	}
	if params.Text == "" {
		return errReply("empty_agent_prompt", "agent prompt must not be empty")
	}
	pane, ambiguous := p.th.agentPane(params.Target)
	if ambiguous {
		return errReply("agent_target_ambiguous", "agent target "+params.Target+" is ambiguous")
	}
	p.mu.Lock()
	code := p.refuse[params.Target]
	after, answered := p.after[params.Target]
	hold := p.holds[params.Target]
	t := typed{Target: params.Target, Pane: pane, Text: params.Text}
	if pane != "" && code == "" {
		p.typed = append(p.typed, t)
	}
	p.mu.Unlock()
	switch {
	case pane == "":
		return errReply("agent_not_found", "agent target "+params.Target+" not found")
	case code != "":
		return errReply(code, "refused: "+code)
	}
	select {
	case p.arrived <- t:
	default:
	}
	if hold != nil {
		<-hold
	}
	if answered {
		return after
	}
	return herdrtest.Reply{Result: map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": pane, "name": params.Target}}}
}

// all is every prompt typed so far, in order.
func (p *prompter) all() []typed {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]typed{}, p.typed...)
}

// to is every prompt typed into pane so far.
func (p *prompter) to(pane string) []typed {
	var out []typed
	for _, t := range p.all() {
		if t.Pane == pane {
			out = append(out, t)
		}
	}
	return out
}

func (p *prompter) setRefuse(target, code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refuse[target] = code
}

func (p *prompter) setAfter(target string, reply herdrtest.Reply) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.after[target] = reply
}

// hold makes every prompt to target, once typed, wait to be answered until
// the function it returns is called. The test's cleanup calls it too,
// before the server's close, which waits for its handlers.
func (p *prompter) hold(t *testing.T, target string) (release func()) {
	t.Helper()
	c := make(chan struct{})
	p.mu.Lock()
	p.holds[target] = c
	p.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(c) }) }
	t.Cleanup(release)
	return release
}

// next waits for the next prompt herdr types.
func (p *prompter) next(t *testing.T) typed {
	t.Helper()
	select {
	case got := <-p.arrived:
		return got
	case <-time.After(ready):
		t.Fatal("no prompt reached herdr")
		return typed{}
	}
}

// agentPane is the pane herdr resolves target to (0.9.5
// resolve_agent_target, src/app/terminal_targets.rs:74-104): the pane of
// that id when an agent runs there, else the one pane agent.list names
// target on; "" for none. A name two or more agents share is ambiguous,
// and herdr types it into neither.
func (th *teamHerdr) agentPane(name string) (pane string, ambiguous bool) {
	th.mu.Lock()
	defer th.mu.Unlock()
	for _, p := range th.panes {
		if agent, _ := p["agent"].(string); p["pane_id"] == name && agent != "" {
			return name, false
		}
	}
	matches := 0
	for _, a := range th.agents {
		if a["name"] == name {
			matches++
			pane, _ = a["pane_id"].(string)
		}
	}
	if matches > 1 {
		return "", true
	}
	return pane, false
}

func (th *teamHerdr) setAgents(agents ...map[string]any) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.agents = agents
}

// haTeam is th serving the ha team (withTeam): its boss's agent named
// ha-boss in the boss's tab, its coder's named ha-coder-1, a workers row
// binding ha-coder-1 to the coder's pane by its terminal, and agent.prompt
// answered by a prompter. dir is the daemon's state dir.
func haTeam(t *testing.T) (th *teamHerdr, p *prompter, dir string) {
	t.Helper()
	th = newTeamHerdr(t)
	withTeam(th)
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"))
	p = newPrompter(th)
	dir = shortDir(t)
	seedRow(t, dir, th, "ha-coder-1", "w14:p2", "term_w14:p2", 0, 0)
	return th, p, dir
}

// stateStore opens the state.db of th's daemon in dir beside the daemon's
// own connection, creating it if no daemon has yet.
func stateStore(t *testing.T, dir string, th *teamHerdr) *store.Store {
	t.Helper()
	p := paths(t, dir, th.fakeHerdr)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	st, err := store.Open(t.Context(), p.DB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func sent(t *testing.T, st *store.Store, to, from store.Address, body string) store.Message {
	t.Helper()
	m, err := st.Send(t.Context(), to, from, body, time.Now())
	require.NoError(t, err)
	return m
}

func stateOf(t *testing.T, st *store.Store, id string) store.Message {
	t.Helper()
	m, err := st.Message(t.Context(), id)
	require.NoError(t, err)
	return m
}

// eventuallyState waits until message id is in state.
func eventuallyState(t *testing.T, st *store.Store, id, state string) store.Message {
	t.Helper()
	var last store.Message
	require.Eventually(t, func() bool {
		m, err := st.Message(t.Context(), id)
		last = m
		return err == nil && m.State == state
	}, ready, 10*time.Millisecond, "message %s never became %s; last %+v", id, state, &last)
	return last
}

// noticeOf waits until the notice reporting id's failure is delivered, and
// returns it.
func noticeOf(t *testing.T, st *store.Store, raw func() []string, id string) store.Message {
	t.Helper()
	var notice store.Message
	require.Eventually(t, func() bool {
		for _, nid := range raw() {
			m, err := st.Message(t.Context(), nid)
			if err == nil && m.NoticeOf == id && m.State == store.MessageDelivered {
				notice = m
				return true
			}
		}
		return false
	}, ready, 10*time.Millisecond, "the notice of %s was never delivered", id)
	return notice
}

// messageIDs reads every message id in the state.db of th's daemon in dir.
func messageIDs(t *testing.T, dir string, th *teamHerdr) func() []string {
	t.Helper()
	return func() []string {
		return query(t, paths(t, dir, th.fakeHerdr).DB, "SELECT id FROM messages ORDER BY seq")
	}
}

// prompts counts the agent.prompt requests srv got.
func prompts(srv *herdrtest.Server) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == "agent.prompt" {
			n++
		}
	}
	return n
}

// TestDispatcherDeliversByName is A30's delivery: each queued message is
// typed once into the agent its recipient's name resolves to, a worker's
// wrapped with its id and sender so it can reply, the daemon's own as its
// body, and recorded delivered after one attempt; the one queued first is
// typed first.
func TestDispatcherDeliversByName(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	toCoder := sent(t, st, coder, haBoss, "build it")
	toBoss := sent(t, st, haBoss, coder, "done")
	digest := sent(t, st, haBoss, store.Daemon, "[agentisan] 2 turns ended")
	serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)

	for _, m := range []store.Message{toCoder, toBoss, digest} {
		got := eventuallyState(t, st, m.ID, store.MessageDelivered)
		assert.Equal(t, 1, got.Attempts, "%s: one attempt", m.Body)
	}
	want := []typed{
		{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + toCoder.ID + " from ha-boss] build it"},
		{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + toBoss.ID + " from ha-coder-1] done"},
		{Target: "ha-boss", Pane: "w14:p1", Text: "[agentisan] 2 turns ended"},
	}
	assert.Equal(t, want, p.all())
	polled(t, th.srv, 2)
	assert.Equal(t, want, p.all(), "each typed once")
}

// TestDispatcherServesARecipientUntilItsQueueIsEmpty: one pass delivers
// all of a recipient's messages, in order, not one a poll.
func TestDispatcherServesARecipientUntilItsQueueIsEmpty(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	var want []typed
	for i := range 3 {
		m := sent(t, st, haBoss, coder, fmt.Sprintf("report %d", i))
		want = append(want, typed{Target: "ha-boss", Pane: "w14:p1", Text: fmt.Sprintf("[msg %s from ha-coder-1] report %d", m.ID, i)})
	}
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Poll = time.Hour // one poll, so one pass after it
	serving(t, o, th.fakeHerdr)

	require.Eventually(t, func() bool { return len(p.all()) == len(want) }, ready, 10*time.Millisecond, "not all delivered after one poll: %v", p.all)
	assert.Equal(t, want, p.all())
}

// TestDispatcherIsExactlyOnceAcrossAHandoff is I3 end to end: three messages
// queued while a handoff holds the worker's inbox reach neither session
// while it is pending, and once it completes onto the new session, which
// has taken the worker's name, each is typed there exactly once, in order,
// and never into the old one.
func TestDispatcherIsExactlyOnceAcrossAHandoff(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	th.mu.Lock()
	th.panes = append(th.panes, map[string]any{"pane_id": "w14:p3", "terminal_id": "term_w14:p3", "workspace_id": "w14", "tab_id": "w14:t2", "agent": "claude", "agent_status": "idle"})
	th.mu.Unlock()
	// The new session runs beside the old one under a name of its own, as
	// 5261's handoff starts it.
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("w14:p3", "ha-coder-1-next"))
	h, err := st.BeginHandoff(t.Context(), "ha-coder-1", "w14:p3", "term_w14:p3", time.Now())
	require.NoError(t, err)
	var ids []string
	for i := range 3 {
		ids = append(ids, sent(t, st, coder, haBoss, fmt.Sprintf("queued in the handoff, %d", i)).ID)
	}
	serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)

	polled(t, th.srv, 3)
	assert.Empty(t, p.all(), "the pending handoff holds the inbox")

	// The old session exits and the new one takes the name, then the
	// handoff completes: the rename comes first (5b's contract).
	var left []map[string]any
	for _, pane := range th.listedPanes() {
		if pane["pane_id"] != "w14:p2" {
			left = append(left, pane)
		}
	}
	th.setPanes(left)
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p3", "ha-coder-1"))
	require.NoError(t, st.CompleteHandoff(t.Context(), h, time.Now()))

	var want []typed
	for i, id := range ids {
		eventuallyState(t, st, id, store.MessageDelivered)
		want = append(want, typed{Target: "ha-coder-1", Pane: "w14:p3", Text: fmt.Sprintf("[msg %s from ha-boss] queued in the handoff, %d", id, i)})
	}
	polled(t, th.srv, 2)
	assert.Equal(t, want, p.all(), "each once, in order, into the new session alone")
}

// TestDispatcherFailsWhatMayHaveBeenTyped is A2's timeout rule and its
// kin: a prompt that ended in a way that may have come after the text was
// typed (herdr's timeout or agent_prompt_failed, a connection closed with no
// answer, an answer over the client's 1 MiB cap, no answer within the
// client's timeout) fails the message, with its sender told and given the
// body, and is never typed again.
func TestDispatcherFailsWhatMayHaveBeenTyped(t *testing.T) {
	t.Parallel()

	tests := map[string]func(t *testing.T, p *prompter, o *daemon.Options){
		"herdr's timeout": func(_ *testing.T, p *prompter, _ *daemon.Options) {
			p.setAfter("ha-coder-1", errReply("timeout", "timed out waiting for the agent"))
		},
		"agent_prompt_failed": func(_ *testing.T, p *prompter, _ *daemon.Options) {
			p.setAfter("ha-coder-1", errReply("agent_prompt_failed", "pty actor closed"))
		},
		"a connection closed with no answer": func(_ *testing.T, p *prompter, _ *daemon.Options) {
			p.setAfter("ha-coder-1", herdrtest.Reply{Silent: true})
		},
		"an answer over the client's cap": func(_ *testing.T, p *prompter, _ *daemon.Options) {
			p.setAfter("ha-coder-1", herdrtest.Reply{Raw: []byte(strings.Repeat("x", 1<<20+16))})
		},
		"no answer within the client's timeout": func(t *testing.T, p *prompter, o *daemon.Options) {
			p.hold(t, "ha-coder-1")
			o.Herdr = herdr.Client{SocketPath: o.HerdrSocket, Timeout: time.Second}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th, p, dir := haTeam(t)
			st := stateStore(t, dir, th)
			m := sent(t, st, coder, haBoss, "the body")
			o := teamOpts(t, dir, th, workforceConfig(t))
			setup(t, p, &o)
			serving(t, o, th.fakeHerdr)

			got := eventuallyState(t, st, m.ID, store.MessageFailed)
			assert.Equal(t, 1, got.Attempts)
			assert.NotEmpty(t, got.Reason)
			notice := noticeOf(t, st, messageIDs(t, dir, th), m.ID)
			assert.Equal(t, haBoss, notice.To, "the sender is told")
			assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: notice.Body}}, p.to("w14:p1"), "typed as the daemon wrote it")
			assert.Contains(t, notice.Body, m.ID)
			assert.Contains(t, notice.Body, "the body", "with the body to resend")

			polled(t, th.srv, 3)
			assert.Equal(t, []typed{{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + m.ID + " from ha-boss] the body"}}, p.to("w14:p2"), "never typed again")
			assert.Equal(t, store.MessageFailed, stateOf(t, st, m.ID).State)
		})
	}
}

// TestDispatcherRetriesARefusal: a prompt herdr refuses before typing
// anything leaves the message queued, tried again at each pass with every
// attempt counted, never in a loop within one, and logged once at warn
// while the refusal lasts (Q3). Once herdr takes it, it is typed once.
func TestDispatcherRetriesARefusal(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"agent_not_found", "agent_target_ambiguous", "agent_blocked", "agent_not_ready", "empty_agent_prompt"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			th, p, dir := haTeam(t)
			st := stateStore(t, dir, th)
			p.setRefuse("ha-coder-1", code)
			m := sent(t, st, coder, haBoss, "try again")
			next := sent(t, st, haBoss, coder, "queued after it")
			var logs lockedLog
			o := teamOpts(t, dir, th, workforceConfig(t))
			o.Logger = zerolog.New(&logs)
			serving(t, o, th.fakeHerdr)

			// Between two tries it is queued; a read can also find it
			// claimed for the next one.
			var got store.Message
			require.Eventually(t, func() bool {
				got = stateOf(t, st, m.ID)
				return got.Attempts >= 3 && got.State == store.MessageQueued
			}, ready, 5*time.Millisecond, "not tried again, or not left queued")
			assert.Contains(t, got.Reason, code)
			polled(t, th.srv, 2)
			assert.LessOrEqual(t, prompts(th.srv), 3*lists(th.srv)+3, "one prompt a pass, never a loop")
			eventuallyState(t, st, next.ID, store.MessageDelivered)
			assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + next.ID + " from ha-coder-1] queued after it"}}, p.all(),
				"the refusal ends the coder's turn in the pass, not the pass: nothing typed for the coder, the boss served after it")
			assert.Equal(t, 1, warnings(logs.String(), "herdr refused the prompt before typing it"), "logged once while it lasts")

			p.setRefuse("ha-coder-1", "")
			eventuallyState(t, st, m.ID, store.MessageDelivered)
			polled(t, th.srv, 2)
			assert.Len(t, p.to("w14:p2"), 1, "typed once")
		})
	}
}

// warnings counts the warn lines in logs that say msg.
func warnings(logs, msg string) int {
	n := 0
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, `"level":"warn"`) && strings.Contains(line, msg) {
			n++
		}
	}
	return n
}

// TestDispatcherWaitsOutAnAmbiguousWorkerName: a second agent takes the
// worker's name on another pane. The poll's model still shows the name on
// the bound pane, so the dispatcher prompts, and herdr refuses the name as
// ambiguous before typing anything. The message stays queued, tried again
// each pass and warned of once, and the boss is still served. Once the
// duplicate is renamed, the message lands once, in the bound pane (QA #72).
func TestDispatcherWaitsOutAnAmbiguousWorkerName(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	th.mu.Lock()
	th.tabs = append(th.tabs, map[string]any{"tab_id": "w14:t3", "workspace_id": "w14", "label": "intruder"})
	th.panes = append(th.panes, map[string]any{"pane_id": "w14:p3", "terminal_id": "term_w14:p3", "workspace_id": "w14", "tab_id": "w14:t3", "agent": "claude", "agent_status": "idle"})
	th.mu.Unlock()
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("w14:p3", "ha-coder-1"))
	st := stateStore(t, dir, th)
	m := sent(t, st, coder, haBoss, "which one?")
	toBoss := sent(t, st, haBoss, coder, "the boss is served anyway")
	var logs lockedLog
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Logger = zerolog.New(&logs)
	run(t, o)

	require.Eventually(t, func() bool {
		got := stateOf(t, st, m.ID)
		return got.Attempts >= 3 && got.State == store.MessageQueued
	}, ready, 5*time.Millisecond, "not retried, or not left queued")
	eventuallyState(t, st, toBoss.ID, store.MessageDelivered)
	assert.Empty(t, p.to("w14:p2"), "nothing typed into the bound pane")
	assert.Empty(t, p.to("w14:p3"), "nothing typed into the intruder")
	assert.Contains(t, stateOf(t, st, m.ID).Reason, "agent_target_ambiguous")
	assert.Equal(t, 1, warnings(logs.String(), "herdr refused the prompt before typing it"), "warned once")

	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("w14:p3", "intruder"))
	eventuallyState(t, st, m.ID, store.MessageDelivered)
	polled(t, th.srv, 2)
	assert.Equal(t, []typed{{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + m.ID + " from ha-boss] which one?"}}, p.to("w14:p2"), "once, in the bound pane")
	assert.Empty(t, p.to("w14:p3"))
}

// TestDispatcherWaitsOutAnAmbiguousBossName: another agent named ha-boss
// runs outside the boss place. The boss's own pane still proves the name,
// herdr refuses it as ambiguous, and the message waits until the duplicate
// is gone (QA #72).
func TestDispatcherWaitsOutAnAmbiguousBossName(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("wJ:p1", "ha-boss"))
	st := stateStore(t, dir, th)
	m := sent(t, st, haBoss, coder, "boss twice")
	run(t, teamOpts(t, dir, th, workforceConfig(t)))

	require.Eventually(t, func() bool {
		got := stateOf(t, st, m.ID)
		return got.Attempts >= 2 && got.State == store.MessageQueued
	}, ready, 5*time.Millisecond, "not retried, or not left queued")
	assert.Empty(t, p.all(), "nothing typed into either ha-boss")
	assert.Contains(t, stateOf(t, st, m.ID).Reason, "agent_target_ambiguous")

	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"))
	eventuallyState(t, st, m.ID, store.MessageDelivered)
	polled(t, th.srv, 2)
	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + m.ID + " from ha-coder-1] boss twice"}}, p.all(), "once, in the boss's pane")
}

// TestDispatcherHoldsAnUnprovenTarget is B1 and B2: a message is typed only
// by a name the team poll's last lists show where it should be, so nothing
// is typed while agent.list fails, into an agent of the worker's name on a
// pane its row does not bind, for a row whose terminal no pane has (a herdr
// restart, before the reconcile), or into a boss of the right name outside
// its own team's boss place. The message stays queued, never claimed.
func TestDispatcherHoldsAnUnprovenTarget(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		to    store.Address
		setup func(t *testing.T, th *teamHerdr, dir string)
	}{
		"no agent list, to a worker": {to: coder, setup: func(_ *testing.T, th *teamHerdr, _ string) {
			th.mu.Lock()
			defer th.mu.Unlock()
			th.agentsErr = true
		}},
		"no agent list, to a boss": {to: haBoss, setup: func(_ *testing.T, th *teamHerdr, _ string) {
			th.mu.Lock()
			defer th.mu.Unlock()
			th.agentsErr = true
		}},
		"the worker's name on a pane its row does not bind": {to: coder, setup: func(_ *testing.T, th *teamHerdr, _ string) {
			th.mu.Lock()
			th.tabs = append(th.tabs, map[string]any{"tab_id": "w14:t3", "workspace_id": "w14", "label": "ha-coder-2"})
			th.panes = append(th.panes, map[string]any{"pane_id": "w14:p3", "terminal_id": "term_w14:p3", "workspace_id": "w14", "tab_id": "w14:t3", "agent": "claude", "agent_status": "idle"})
			th.mu.Unlock()
			th.setAgents(named("w14:p1", "ha-boss"), named("w14:p3", "ha-coder-1"))
		}},
		"the worker's name on the boss's pane": {to: coder, setup: func(_ *testing.T, th *teamHerdr, _ string) {
			th.setAgents(named("w14:p1", "ha-coder-1"))
		}},
		"a row whose terminal no pane has": {to: coder, setup: func(t *testing.T, th *teamHerdr, dir string) {
			execDB(t, dir, th, "UPDATE workers SET terminal_id = 'term_before_the_restart' WHERE logical_id = 'ha-coder-1'")
		}},
		"the boss's name outside its boss place": {to: haBoss, setup: func(_ *testing.T, th *teamHerdr, _ string) {
			th.setAgents(named("w14:p2", "ha-boss"))
		}},
		"the boss's name in another team's boss place": {to: haBoss, setup: func(_ *testing.T, th *teamHerdr, _ string) {
			th.setAgents(named("wJ:p1", "ha-boss"), named("w14:p2", "ha-coder-1"))
		}},
		"a boss no team has": {to: store.Address{Kind: store.KindBoss, ID: "zz-boss"}, setup: func(_ *testing.T, th *teamHerdr, _ string) {
			th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("w2:p1", "zz-boss"))
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th, p, dir := haTeam(t)
			st := stateStore(t, dir, th)
			tc.setup(t, th, dir)
			m := sent(t, st, tc.to, aPane, "wait")
			serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)

			polled(t, th.srv, 3)
			assert.Empty(t, p.all(), "nothing typed")
			got := stateOf(t, st, m.ID)
			assert.Equal(t, store.MessageQueued, got.State)
			assert.Zero(t, got.Attempts, "never claimed")
		})
	}
}

// TestDispatcherDeliversToAPaneByItsTerminal is B3: a message to a pane
// sender, kept by its terminal, is typed into the pane that shows the
// terminal when it is served, by that pane's id, which herdr resolves only
// to an agent there. A pane whose id herdr reissued still gets the next one.
func TestDispatcherDeliversToAPaneByItsTerminal(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)

	first := sent(t, st, notes, store.Daemon, "[agentisan] first")
	eventuallyState(t, st, first.ID, store.MessageDelivered)
	assert.Equal(t, []typed{{Target: "w2:p1", Pane: "w2:p1", Text: "[agentisan] first"}}, p.all())

	movePane(th, notes.ID, "w2:p7")
	second := sent(t, st, notes, store.Daemon, "[agentisan] second")
	eventuallyState(t, st, second.ID, store.MessageDelivered)
	require.Len(t, p.all(), 2)
	assert.Equal(t, typed{Target: "w2:p7", Pane: "w2:p7", Text: "[agentisan] second"}, p.all()[1], "the pane that shows the terminal now")
}

// movePane gives the pane showing terminal the id pane, as herdr reissuing
// pane ids does.
func movePane(th *teamHerdr, terminal, pane string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	panes := slices.Clone(th.panes) // a list in flight may still hold the old slice
	for i, pn := range panes {
		if pn["terminal_id"] == terminal {
			moved := maps.Clone(pn)
			moved["pane_id"] = pane
			panes[i] = moved
		}
	}
	th.panes = panes
}

// TestDispatcherFailsAPaneMessageItCannotType is B3 and Q3: a message to a
// pane whose terminal no pane shows is failed at once, never typed; one to a
// pane with no agent, which herdr refuses on every pass, waits while it is
// newer than an hour, then is failed, with its failed event. A message the
// daemon sent that fails sends no notice.
func TestDispatcherFailsAPaneMessageItCannotType(t *testing.T) {
	t.Parallel()
	shell := store.Address{Kind: store.KindPane, ID: "term_w12:p1"} // ◆ herdr's shell
	tests := map[string]struct {
		to     store.Address
		age    time.Duration
		failed bool
		reason string
	}{
		"a terminal no pane shows":        {to: aPane, failed: true, reason: "no pane shows the terminal it was sent to"},
		"a shell, for under an hour":      {to: shell, age: 59 * time.Minute},
		"a shell, for an hour":            {to: shell, age: time.Hour, failed: true, reason: "no agent in the pane took it within 1h0m0s"},
		"an agent's pane, for over a day": {to: notes, age: 25 * time.Hour, failed: true, reason: "no agent in the pane took it within 1h0m0s"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th, p, dir := haTeam(t)
			st := stateStore(t, dir, th)
			now := time.Now()
			m, err := st.Send(t.Context(), tc.to, store.Daemon, "[agentisan] a notice", now.Add(-tc.age))
			require.NoError(t, err)
			events, rec := telemetrytest.NewEvents()
			o := teamOpts(t, dir, th, workforceConfig(t))
			o.Team.Events = events
			o.Now = func() time.Time { return now } // the age is exact
			serving(t, o, th.fakeHerdr)

			if tc.failed {
				got := eventuallyState(t, st, m.ID, store.MessageFailed)
				assert.Equal(t, tc.reason, got.Reason)
				polled(t, th.srv, 2)
				assert.Equal(t, []string{m.ID}, messageIDs(t, dir, th)(), "a message the daemon sent gets no notice")
				var failed []telemetrytest.Event
				for _, e := range rec.Events() {
					if e.Name == telemetry.EventMessageFailed {
						failed = append(failed, e)
					}
				}
				require.Len(t, failed, 1)
				assert.Equal(t, m.ID, failed[0].Attrs["agentisan.message.id"])
				assert.Equal(t, tc.reason, failed[0].Attrs["agentisan.message.reason"])
			} else {
				require.Eventually(t, func() bool {
					got := stateOf(t, st, m.ID)
					return got.State == store.MessageQueued && got.Attempts >= 2
				}, ready, 10*time.Millisecond, "tried pass after pass, refused each time, and queued again")
			}
			assert.Empty(t, p.all(), "nothing typed")
		})
	}
}

// TestDispatcherNeverAgesOutAWorkersMessage is Q3: the hour is for a pane
// recipient only, so a worker's message held two hours, such as one queued
// through a long handoff, is still typed.
func TestDispatcherNeverAgesOutAWorkersMessage(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	now := time.Now()
	m, err := st.Send(t.Context(), coder, haBoss, "held two hours", now.Add(-2*time.Hour))
	require.NoError(t, err)
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Now = func() time.Time { return now } // the age is exact
	serving(t, o, th.fakeHerdr)

	eventuallyState(t, st, m.ID, store.MessageDelivered)
	assert.Equal(t, typed{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + m.ID + " from ha-boss] held two hours"}, p.next(t))
}

// TestDispatcherHoldsWhenAListFailsLater: every poll replaces the model the
// dispatcher checks targets against, so a list that fails after good polls
// holds every message until it answers again, rather than leaving the last
// good lists to vouch for names that may have moved.
func TestDispatcherHoldsWhenAListFailsLater(t *testing.T) {
	t.Parallel()

	for name, fail := range map[string]func(th *teamHerdr, failing bool){
		"pane.list":  func(th *teamHerdr, failing bool) { th.mu.Lock(); th.panesErr = failing; th.mu.Unlock() },
		"agent.list": func(th *teamHerdr, failing bool) { th.mu.Lock(); th.agentsErr = failing; th.mu.Unlock() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th, p, dir := haTeam(t)
			st := stateStore(t, dir, th)
			serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)
			polled(t, th.srv, 2)

			fail(th, true)
			listed(t, th.srv, 2)
			m := sent(t, st, coder, haBoss, "wait for the lists")
			listed(t, th.srv, 3)
			assert.Empty(t, p.all(), "nothing typed on lists that failed")
			assert.Equal(t, store.MessageQueued, stateOf(t, st, m.ID).State)

			fail(th, false)
			eventuallyState(t, st, m.ID, store.MessageDelivered)
		})
	}
}

// listed waits until srv has answered n more pane.list requests than it
// had: polls, even ones that fail at their first list.
func listed(t *testing.T, srv *herdrtest.Server, n int) {
	t.Helper()
	count := func() int {
		c := 0
		for _, r := range srv.Requests() {
			if r.Method == "pane.list" {
				c++
			}
		}
		return c
	}
	from := count()
	require.Eventually(t, func() bool { return count() >= from+n+1 }, ready, 5*time.Millisecond, "the daemon did not list the panes %d more times", n)
}

// TestDispatcherHoldsAPaneNoticeWhenItsListFails is B3: the panes the
// dispatcher lists for its pane recipients failing holds a pane's notice
// for the next pass, untried, and never fails it as shown by no pane; the
// pass goes on to the other recipients.
func TestDispatcherHoldsAPaneNoticeWhenItsListFails(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	shell := store.Address{Kind: store.KindPane, ID: "term_w12:p1"} // ◆ herdr's shell: refused, queued again
	m := sent(t, st, shell, store.Daemon, "[agentisan] a notice")
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Poll = time.Hour // the poll at start only: the send's wake makes the next pass
	run(t, o)
	healthy(t, dir, th.fakeHerdr)
	require.Eventually(t, func() bool {
		got := stateOf(t, st, m.ID)
		return got.State == store.MessageQueued && got.Attempts >= 1
	}, ready, 10*time.Millisecond, "the pass at start tried it")
	before := stateOf(t, st, m.ID)

	// The send lists the panes before the agents, the dispatcher after:
	// fail the first pane.list after an agent.list, and only that one.
	var sawAgents, failed atomic.Bool
	th.mu.Lock()
	th.onCall = func(method string) {
		th.mu.Lock()
		defer th.mu.Unlock()
		if failed.Load() {
			th.panesErr = false
		}
		switch {
		case method == "agent.list":
			sawAgents.Store(true)
		case method == "pane.list" && sawAgents.Load() && !failed.Load():
			failed.Store(true)
			th.panesErr = true
		}
	}
	th.mu.Unlock()

	data, err := daemon.Workforce(t.Context(), socketPath(t, dir, th.path), daemon.OpSend,
		workforce.SendRequest{To: "ha-coder-1", Body: "wake", CallerPane: "w14:p1"})
	require.NoError(t, err)
	var res workforce.SendResult
	require.NoError(t, json.Unmarshal(data, &res))
	assert.Equal(t, "ha-coder-1", p.next(t).Target, "the pass went on to the coder")
	eventuallyState(t, st, res.ID, store.MessageDelivered)
	require.True(t, failed.Load(), "the dispatcher's pane.list was failed")
	got := stateOf(t, st, m.ID)
	assert.Equal(t, store.MessageQueued, got.State, "held, not failed: %+v", got)
	assert.Equal(t, before.Attempts, got.Attempts, "not claimed while the list failed")
}

// TestDispatcherLogsAWaitOnce (Q3): a message held poll after poll is
// logged at warn once, not every pass, and again once the recipient's queue
// has emptied and a new wait begins.
func TestDispatcherLogsAWaitOnce(t *testing.T) {
	t.Parallel()
	const held = "no configured team has this boss"
	th, _, dir := haTeam(t)
	st := stateStore(t, dir, th)
	zz := store.Address{Kind: store.KindBoss, ID: "zz-boss"}
	first := sent(t, st, zz, aPane, "one")
	var logs lockedLog
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Logger = zerolog.New(&logs)
	serving(t, o, th.fakeHerdr)

	polled(t, th.srv, 3)
	assert.Equal(t, 1, warnings(logs.String(), held), "once over three polls")

	m, ok, err := st.Claim(t.Context(), zz, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first.ID, m.ID)
	require.NoError(t, st.Delivered(t.Context(), m.ID, time.Now()))
	polled(t, th.srv, 2)
	sent(t, st, zz, aPane, "two")
	polled(t, th.srv, 3)
	assert.Equal(t, 2, warnings(logs.String(), held), "a wait that begins again is logged again")
}

// TestDispatcherReturnsAnEndedQueueToItsBoss is Q7: once a worker's binding
// ends, retired or its id bound again at a later generation, what was
// queued for it goes to its project's boss, typed there with whom it was
// for, and never to the new binding. A project no team is configured for
// keeps the queue.
func TestDispatcherReturnsAnEndedQueueToItsBoss(t *testing.T) {
	t.Parallel()

	retire := func(t *testing.T, st *store.Store) {
		t.Helper()
		require.NoError(t, st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: "ha-coder-1"}, store.StatusRetired, time.Now()))
	}
	tests := map[string]func(t *testing.T, st *store.Store){
		"retired": retire,
		"its id bound again": func(t *testing.T, st *store.Store) {
			t.Helper()
			retire(t, st)
			gen, err := st.BindWorker(t.Context(), store.Worker{LogicalID: "ha-coder-1", Project: "ha", Group: "coder", PaneID: "w14:p2", TerminalID: "term_w14:p2"}, time.Now())
			require.NoError(t, err)
			require.EqualValues(t, 1, gen)
			require.NoError(t, st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: "ha-coder-1", Generation: 1}, store.StatusActive, time.Now()))
		},
	}
	for name, end := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th, p, dir := haTeam(t)
			st := stateStore(t, dir, th)
			m := sent(t, st, coder, aPane, "for the coder")
			end(t, st)
			serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)

			got := eventuallyState(t, st, m.ID, store.MessageDelivered)
			assert.Equal(t, haBoss, got.To)
			assert.Equal(t, "ha-coder-1", got.ReturnedFrom)
			polled(t, th.srv, 2)
			assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + m.ID + " from term_gone, originally to ha-coder-1] for the coder"}}, p.all())
		})
	}
	t.Run("a project no team is configured for", func(t *testing.T) {
		t.Parallel()
		th, p, dir := haTeam(t)
		execDB(t, dir, th, `INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, terminal_id)
VALUES ('zz-coder-1', 'zz', 'coder', 'w2:p1', 0, 'active', 0, 'term_w2:p1')`)
		st := stateStore(t, dir, th)
		m := sent(t, st, store.Address{Kind: store.KindWorker, ID: "zz-coder-1"}, aPane, "nowhere to go")
		require.NoError(t, st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: "zz-coder-1"}, store.StatusRetired, time.Now()))
		var logs lockedLog
		o := teamOpts(t, dir, th, workforceConfig(t))
		o.Logger = zerolog.New(&logs)
		serving(t, o, th.fakeHerdr)

		polled(t, th.srv, 3)
		got := stateOf(t, st, m.ID)
		assert.Equal(t, store.MessageQueued, got.State)
		assert.Equal(t, "zz-coder-1", got.To.ID, "kept where it was")
		assert.Empty(t, p.all())
		assert.Equal(t, 1, warnings(logs.String(), "no team is configured for its project zz"))
	})
}

// TestDispatcherTypesCleanText is B6 on the wire: CR and CRLF arrive as LF,
// LF and TAB as they are, every other control character not at all, so no
// body can end herdr's bracketed paste or press a key, and an emoji joined
// by ZWJ arrives whole. The store keeps the body as sent.
func TestDispatcherTypesCleanText(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	body := "a\r\nb\rc\x1b[201~d\x07e\u0085f\u009bg\x7fh\ti\nj 👩\u200d💻"
	m := sent(t, st, coder, haBoss, body)
	serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)

	eventuallyState(t, st, m.ID, store.MessageDelivered)
	assert.Equal(t, []typed{{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + m.ID + " from ha-boss] a\nb\nc[201~defgh\ti\nj 👩\u200d💻"}}, p.all())
	assert.Equal(t, body, stateOf(t, st, m.ID).Body)
}

// TestDispatcherStopFinishesThePromptInFlight: a daemon stopped while a
// prompt is in flight waits for its answer and records the message
// delivered, so a stop never turns a deliverable message into a failure.
func TestDispatcherStopFinishesThePromptInFlight(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	release := p.hold(t, "ha-coder-1")
	m := sent(t, st, coder, haBoss, "in flight")
	r := serving(t, teamOpts(t, dir, th, workforceConfig(t)), th.fakeHerdr)
	p.next(t)

	r.cancel()
	assert.Never(t, func() bool { return !r.alive() }, 300*time.Millisecond, 10*time.Millisecond, "the stop waits for the prompt in flight")
	release()
	require.NoError(t, r.wait(t, ready))
	assert.Equal(t, store.MessageDelivered, stateOf(t, st, m.ID).State)
}

// TestDispatcherStopsBeforeTheNextClaim: two messages wait for one worker,
// and the daemon stops while the first prompt is in flight. That prompt
// finishes and is recorded, and the second is never claimed: serve checks
// the stop before each claim, and that check is the only guard between
// messages, since the claim, deliver and drop all go on past a stop (QA
// #84, S2).
func TestDispatcherStopsBeforeTheNextClaim(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	release := p.hold(t, "ha-coder-1")
	first := sent(t, st, coder, haBoss, "first")
	second := sent(t, st, coder, haBoss, "second")
	r := run(t, teamOpts(t, dir, th, workforceConfig(t)))
	p.next(t)

	r.cancel()
	time.Sleep(100 * time.Millisecond)
	release()
	require.NoError(t, r.wait(t, ready))
	assert.Equal(t, store.MessageDelivered, stateOf(t, st, first.ID).State)
	got := stateOf(t, st, second.ID)
	assert.Equal(t, store.MessageQueued, got.State, "not claimed after the stop")
	assert.Equal(t, 0, got.Attempts)
	assert.Len(t, p.all(), 1, "one prompt typed: the one in flight")
}

// TestClaimIgnoresTheStop: a stop that lands while serve claims a message
// does not cut the claim short (QA #72), as it cuts short neither deliver's
// nor drop's write, so the claimed message goes on to its prompt and is
// never left dispatching behind an error, to be failed at the next start
// though never typed. A context already done stands in for the stop: the
// claim still takes the message. database/sql returns on a done context
// before the statement runs, so this pins that the claim ignores the stop,
// not the race inside the statement itself.
func TestClaimIgnoresTheStop(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	m := sent(t, st, haBoss, store.Address{Kind: store.KindPane, ID: "term_w9:p9"}, "claimed through a stop")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, ok, err := daemon.ClaimOver(ctx, st, haBoss)
	require.NoError(t, err)
	require.True(t, ok, "the stop did not cut the claim short")
	assert.Equal(t, m.ID, got.ID)
	after := stateOf(t, st, m.ID)
	assert.Equal(t, store.MessageDispatching, after.State)
	assert.Equal(t, 1, after.Attempts)
}

// TestDispatcherEmitsMessageEvents is A30's events: a delivered message, a
// failed one and one returned to the boss each give their event, with the
// message's id, sender, recipient, attempts and reason, and never its body.
func TestDispatcherEmitsMessageEvents(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	seedRow(t, dir, th, "ha-coder-2", "w14:p8", "term_w14:p8", 0, 0)
	st := stateStore(t, dir, th)
	p.setAfter("ha-coder-1", errReply("timeout", "timed out"))
	failed := sent(t, st, coder, haBoss, "secret one")
	returned := sent(t, st, store.Address{Kind: store.KindWorker, ID: "ha-coder-2"}, aPane, "secret two")
	require.NoError(t, st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: "ha-coder-2"}, store.StatusRetired, time.Now()))
	events, rec := telemetrytest.NewEvents()
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Team.Events = events
	serving(t, o, th.fakeHerdr)

	eventuallyState(t, st, returned.ID, store.MessageDelivered)
	notice := noticeOf(t, st, messageIDs(t, dir, th), failed.ID)
	event := func(name string, m store.Message, attempts int, reason string) telemetrytest.Event {
		return telemetrytest.Event{Name: name, Attrs: map[string]any{
			"agentisan.message.id": m.ID, "agentisan.message.from.kind": m.From.Kind, "agentisan.message.from.id": m.From.ID,
			"agentisan.message.to.kind": "boss", "agentisan.message.to.id": "ha-boss",
			"agentisan.message.returned_from": "", "agentisan.message.attempts": int64(attempts), "agentisan.message.reason": reason,
		}}
	}
	failedEvent := event(telemetry.EventMessageFailed, failed, 1, "herdr agent.prompt: timeout: timed out")
	failedEvent.Attrs["agentisan.message.to.kind"], failedEvent.Attrs["agentisan.message.to.id"] = "worker", "ha-coder-1"
	returnedEvent := event(telemetry.EventMessageReturned, returned, 0, "")
	returnedEvent.Attrs["agentisan.message.returned_from"] = "ha-coder-2"
	deliveredEvent := event(telemetry.EventMessageDelivered, returned, 1, "")
	deliveredEvent.Attrs["agentisan.message.returned_from"] = "ha-coder-2"
	want := []telemetrytest.Event{failedEvent, event(telemetry.EventMessageDelivered, notice, 1, ""), returnedEvent, deliveredEvent}

	var got []telemetrytest.Event
	require.Eventually(t, func() bool {
		got = got[:0]
		for _, e := range rec.Events() {
			if strings.HasPrefix(e.Name, "agentisan.message.") {
				got = append(got, e)
			}
		}
		return len(got) >= len(want)
	}, ready, 10*time.Millisecond, "the events never came")
	assert.ElementsMatch(t, want, got)
	for _, r := range rec.Records() {
		assert.NotContains(t, r.Body().AsString(), "secret")
	}
	for _, e := range got {
		for k, v := range e.Attrs {
			assert.NotContains(t, fmt.Sprint(v), "secret", "%s carries no body", k)
		}
	}
}

// TestRecoverDispatchingWithoutTheTeamPoll: a daemon fails what the last
// one left dispatching as it starts, telling the sender, whether or not it
// runs the team poll, which the dispatcher needs.
func TestRecoverDispatchingWithoutTheTeamPoll(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	st := stateStore(t, dir, th)
	m := sent(t, st, coder, haBoss, "cut off")
	claimed, ok, err := st.Claim(t.Context(), coder, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, m.ID, claimed.ID)
	run(t, opts(t, dir, th.fakeHerdr))

	got := eventuallyState(t, st, m.ID, store.MessageFailed)
	assert.Equal(t, daemon.RecoveredReason, got.Reason)
	var queued store.Message
	require.Eventually(t, func() bool {
		var ok bool
		queued, ok, err = st.Claim(t.Context(), haBoss, time.Now())
		return err == nil && ok
	}, ready, 10*time.Millisecond, "no notice was queued")
	assert.Equal(t, m.ID, queued.NoticeOf)
}

// TestRecoveredFailureEmitsFailed is A30's events: a message a crash left
// dispatching gives one agentisan.message.failed as the next daemon starts,
// with its one attempt and the recovered reason.
func TestRecoveredFailureEmitsFailed(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	st := stateStore(t, dir, th)
	m := sent(t, st, coder, haBoss, "cut off")
	_, ok, err := st.Claim(t.Context(), coder, time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	events, rec := telemetrytest.NewEvents()
	o := teamOpts(t, dir, th, workforceConfig(t))
	o.Team.Events = events
	run(t, o)

	eventuallyState(t, st, m.ID, store.MessageFailed)
	of := func() []telemetrytest.Event {
		var got []telemetrytest.Event
		for _, e := range rec.Events() {
			if e.Attrs["agentisan.message.id"] == m.ID {
				got = append(got, e)
			}
		}
		return got
	}
	require.Eventually(t, func() bool { return len(of()) > 0 }, ready, 10*time.Millisecond, "no event for the recovered message")
	assert.Equal(t, []telemetrytest.Event{{Name: telemetry.EventMessageFailed, Attrs: map[string]any{
		"agentisan.message.id": m.ID, "agentisan.message.from.kind": "boss", "agentisan.message.from.id": "ha-boss",
		"agentisan.message.to.kind": "worker", "agentisan.message.to.id": "ha-coder-1",
		"agentisan.message.returned_from": "", "agentisan.message.attempts": int64(1), "agentisan.message.reason": daemon.RecoveredReason,
	}}}, of())
}

// TestACrashMidDispatchFailsTheMessage is A2's crash rule, for real: a
// daemon SIGKILLed while herdr is typing a message leaves it dispatching;
// the next daemon fails it as it starts, never types it again, and delivers
// the notice to its sender.
func TestACrashMidDispatchFailsTheMessage(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	cfg := workforceConfig(t)
	st := stateStore(t, dir, th)
	release := p.hold(t, "ha-coder-1")
	m := sent(t, st, coder, haBoss, "cut off by a crash")

	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := newExecCmd(t.Context(), exe)
	// Built from nothing: the child must not see this shell's HERDR_*.
	cmd.Env = []string{helperEnv + "=dispatch", "DAEMON_TEST_DIR=" + dir, "DAEMON_TEST_SOCKET=" + th.path, "DAEMON_TEST_CONFIG=" + cfg}
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		exited <- nil
	})
	// Its start first, then its first dispatch, each within ready (QA #77,
	// F-5).
	healthy(t, dir, th.fakeHerdr)

	got := p.next(t)
	require.Equal(t, typed{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + m.ID + " from ha-boss] cut off by a crash"}, got)
	require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))
	select {
	case err := <-exited:
		var exit *exitError
		require.ErrorAs(t, err, &exit, "killed")
		exited <- err
	case <-time.After(ready):
		t.Fatal("the killed daemon did not exit")
	}
	release()
	require.Equal(t, store.MessageDispatching, stateOf(t, st, m.ID).State, "the crash cut the dispatch off")

	serving(t, restarted(teamOpts(t, dir, th, cfg)), th.fakeHerdr)
	failed := eventuallyState(t, st, m.ID, store.MessageFailed)
	assert.Equal(t, daemon.RecoveredReason, failed.Reason)
	notice := noticeOf(t, st, messageIDs(t, dir, th), m.ID)
	polled(t, th.srv, 2)
	assert.Equal(t, []typed{got, {Target: "ha-boss", Pane: "w14:p1", Text: notice.Body}}, p.all(), "the body typed once, before the crash; then the notice")
}

// helperDispatch runs a daemon with the team poll on, from the state dir,
// herdr socket and config dir the DAEMON_TEST_* variables name, until it is
// killed. It gives up after a minute, so a failed test leaves nothing behind
// for long.
func helperDispatch() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sock := os.Getenv("DAEMON_TEST_SOCKET")
	err := daemon.Run(ctx, daemon.Options{
		StateDir: os.Getenv("DAEMON_TEST_DIR"), HerdrSocket: sock, Herdr: herdr.Client{SocketPath: sock},
		LockWait: time.Second, Poll: 20 * time.Millisecond, Logger: zerolog.Nop(),
		Team: &daemon.TeamOptions{ConfigDir: os.Getenv("DAEMON_TEST_CONFIG")},
	})
	if err != nil {
		os.Exit(6)
	}
	os.Exit(0)
}

// TestRefusal is A2's line between a prompt that typed nothing and one
// that may have: herdr's codes for a refusal before the pane, and a call
// that reached no herdr, are refusals, retried; everything else, a code
// herdr may add later included, may have typed.
func TestRefusal(t *testing.T) {
	t.Parallel()
	api := func(code string) error { return &herdr.APIError{Method: "agent.prompt", Code: code, Message: "m"} }
	tests := map[string]struct {
		err  error
		want string
	}{
		"empty_agent_prompt":     {api("empty_agent_prompt"), "empty_agent_prompt"},
		"agent_not_found":        {api("agent_not_found"), "agent_not_found"},
		"agent_target_ambiguous": {api("agent_target_ambiguous"), "agent_target_ambiguous"},
		"agent_blocked":          {api("agent_blocked"), "agent_blocked"},
		"agent_not_ready":        {api("agent_not_ready"), "agent_not_ready"},
		"nothing sent":           {fmt.Errorf("%w: %w: dial: refused", herdr.ErrUnavailable, herdr.ErrNotSent), "not_sent"},
		"a Dialed refusal":       {fmt.Errorf("%w: agent.prompt: %w", herdr.ErrNotSent, daemon.ErrHerdrGone), "not_sent"},
		"timeout":                {api("timeout"), ""},
		"agent_prompt_failed":    {api("agent_prompt_failed"), ""},
		"a code herdr adds":      {api("agent_busy"), ""},
		"broken after the write": {fmt.Errorf("%w: agent.prompt: EOF", herdr.ErrUnavailable), ""},
		"the client's deadline":  {fmt.Errorf("agent.prompt: %w", context.DeadlineExceeded), ""},
		"a protocol violation":   {fmt.Errorf("%w: agent.prompt: bad", herdr.ErrProtocol), ""},
		"anything else":          {errors.New("no idea"), ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, daemon.Refusal(tc.err))
		})
	}
}

// TestCleanText is B6's rule, byte by byte.
func TestCleanText(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ in, want string }{
		"plain":                {"hello", "hello"},
		"LF and TAB stay":      {"a\nb\tc", "a\nb\tc"},
		"CRLF becomes LF":      {"a\r\nb", "a\nb"},
		"a lone CR becomes LF": {"a\rb", "a\nb"},
		"CR CR LF":             {"a\r\r\nb", "a\n\nb"},
		"ESC ends no paste":    {"x\x1b[201~y", "x[201~y"},
		"other C0":             {"\x00\x01\x07\x08\x0b\x0c\x1f.", "."},
		"DEL":                  {"a\x7fb", "ab"},
		"C1":                   {"a\u0080b\u0085c\u009bd\u009fe", "abcde"},
		"format characters":    {"👩\u200d💻 \u200b\ufeff", "👩\u200d💻 \u200b\ufeff"},
		"non-ASCII":            {"エージェント é", "エージェント é"},
		"invalid UTF-8":        {"a\xffb\xe2\x82", "a\ufffdb\ufffd\ufffd"}, // each byte that does not decode, a truncated sequence's too
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, daemon.CleanText(tc.in))
		})
	}
}

// TestTextOf is Q9 and B5: what each message is typed as.
func TestTextOf(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		m    store.Message
		want string
	}{
		"from a boss":   {store.Message{ID: "01J", From: haBoss, Body: "go"}, "[msg 01J from ha-boss] go"},
		"from a worker": {store.Message{ID: "01J", From: coder, Body: "done"}, "[msg 01J from ha-coder-1] done"},
		"from a pane":   {store.Message{ID: "01J", From: aPane, Body: "hi"}, "[msg 01J from term_gone] hi"},
		"from the daemon": {
			store.Message{ID: "01J", From: store.Daemon, Body: "[agentisan] message 01H to x may not have been delivered"},
			"[agentisan] message 01H to x may not have been delivered",
		},
		"returned": {store.Message{ID: "01J", From: aPane, Body: "hi", ReturnedFrom: "ha-coder-1"}, "[msg 01J from term_gone, originally to ha-coder-1] hi"},
		"the daemon's, returned": {
			store.Message{ID: "01J", From: store.Daemon, Body: "[agentisan] notice", ReturnedFrom: "ha-coder-1"},
			"[agentisan] originally to ha-coder-1: [agentisan] notice",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, daemon.TextOf(tc.m))
		})
	}
}

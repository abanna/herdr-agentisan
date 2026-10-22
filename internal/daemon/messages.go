package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// The dispatcher (ADR-001 D8, A2, A30) types each queued message into its
// recipient with agent.prompt, by the name herdr knows the recipient's agent
// by: a worker's logical id, a boss's <short>-boss, or boss for the ◆ spaces
// (A35, UQ2). herdr resolves the name to a pane and types in one step, so a
// pane id herdr reissued is never typed into. The team poll's last model is
// what proves the name is where it should be (B2); the store's claim holds
// what must wait: a handoff, a lost or starting worker, an ended binding.

// recoveredReason is why a message a crash left dispatching failed.
const recoveredReason = "the daemon stopped mid-dispatch"

// settleTimeout bounds the write that records how a dispatch ended.
const settleTimeout = store.BusyTimeout

// paneWait is how long a message to a pane waits for an agent there to take
// it before it is failed (A30): a pane sender's notice whose pane runs a
// shell, which herdr refuses on every pass, would otherwise wait forever,
// and the prune never removes a queued message.
const paneWait = time.Hour

// refusals are herdr's agent.prompt codes that refuse before any byte
// reaches the pane (0.9.5 validate_agent_prompt).
var refusals = map[string]bool{
	"empty_agent_prompt": true, "agent_not_found": true, "agent_target_ambiguous": true,
	"agent_blocked": true, "agent_not_ready": true,
}

// notSent is the refusal of a prompt that reached no herdr at all.
const notSent = "not_sent"

// refusal is the code of a prompt that certainly typed nothing, or "" for
// one that may have: any other failure, one herdr does not name included,
// is taken as maybe typed (A2).
func refusal(err error) string {
	if errors.Is(err, herdr.ErrNotSent) {
		return notSent
	}
	var apiErr *herdr.APIError
	if errors.As(err, &apiErr) && refusals[apiErr.Code] {
		return apiErr.Code
	}
	return ""
}

// inbox is what the team poll gives the dispatcher: the model its last poll
// built, which every target is checked against, each configured team's
// boss, and a wake.
type inbox struct {
	mu    sync.Mutex
	model *team.Model
	// bosses is each configured project's boss, by the agent name herdr
	// knows it by.
	bosses map[string]string
	wake   chan struct{}
}

func newInbox() *inbox { return &inbox{wake: make(chan struct{}, 1)} }

// configure records each team's boss: <short>-boss for a tabs team, boss for
// the ◆ spaces (UQ2).
func (in *inbox) configure(c teamConfig) {
	bosses := map[string]string{c.spaces.Project: team.SpacesBoss}
	for _, t := range c.tabs {
		bosses[t.Project] = t.BossTab()
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.bosses = bosses
}

// polled replaces the model with a poll's, nil when the poll built none, and
// wakes the dispatcher. Every poll replaces it, so a list that fails holds
// every message until it answers again: a model built without agent.list
// names no agent, and one built without the workers rows binds no worker.
// A model that binds and names a worker is one whose poll reconciled the
// rows first, so no worker is prompted before the first reconcile since the
// daemon started (B1): a row a herdr restart left behind still names a
// terminal no pane has.
func (in *inbox) polled(m *team.Model) {
	in.mu.Lock()
	in.model = m
	in.mu.Unlock()
	in.Wake()
}

// queued is what send does with each message it queues (A30): it wakes the
// dispatcher through in, and records the message's queued event. o runs
// the team poll, since only then is there a send op.
func (o Options) queued(in *inbox) func(context.Context, workforce.SendResult) {
	return func(ctx context.Context, r workforce.SendResult) {
		in.Wake()
		o.Team.Events.MessageQueued(ctx, telemetry.Message{ID: r.ID, FromKind: r.From.Kind, From: r.From.ID, ToKind: r.To.Kind, To: r.To.ID})
	}
}

// Wake asks the dispatcher for a pass. It never blocks, and wakes that come
// before the pass starts are one.
func (in *inbox) Wake() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

func (in *inbox) view() (*team.Model, map[string]string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.model, in.bosses
}

// dispatcher delivers state.db's queued messages, one prompt in flight at a
// time (Q2). Only its goroutine touches it.
type dispatcher struct {
	st     *store.Store
	c      herdr.Client
	in     *inbox
	events *telemetry.Events
	log    zerolog.Logger
	now    func() time.Time
	// warned is, by recipient, why its messages last waited: logged at warn
	// when it begins, then at debug while it lasts (Q3), until the reason
	// changes or the recipient has nothing queued.
	warned map[store.Address]string
}

// newDispatcher is the dispatcher of a daemon run with o, which the team
// poll feeds through in. It prompts through the daemon's client of its own
// server (forServer), so no prompt reaches a replacement (A3), with that
// client's Timeout bounding each.
func (o Options) newDispatcher(st *store.Store, in *inbox) *dispatcher {
	c, _ := o.Herdr.(herdr.Client)
	return &dispatcher{
		st: st, c: c, in: in, events: o.Team.Events, log: o.Logger, now: o.Now,
		warned: map[store.Address]string{},
	}
}

// run makes a pass now and at each wake until ctx ends: after every poll,
// so a refused message is tried again once a poll (Q4), and after whatever
// queues a message. A pass under way when ctx ends finishes the prompt in
// flight first.
func (d *dispatcher) run(ctx context.Context) {
	for {
		d.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.in.wake:
		}
	}
}

// pass returns every ended binding's queue to its boss, then serves each
// recipient with a message queued, the one waiting longest first, whose
// target the poll's model proves. With no model it does nothing: there are
// no lists to check a target against.
func (d *dispatcher) pass(ctx context.Context) {
	m, bosses := d.in.view()
	if m == nil {
		return
	}
	kept := d.sweep(ctx, bosses)
	queued, err := d.st.QueuedRecipients(ctx)
	if err != nil {
		d.warnUnlessStopping(ctx, err, "messages: read the queued recipients")
		return
	}
	d.forget(queued)
	var panes map[string]string // listed once a pass, for the pane recipients
	for _, to := range queued {
		if kept[to] {
			continue // sent to a binding that ended; the sweep said why
		}
		var (
			target, held string
			why          error
		)
		if to.Kind == store.KindPane {
			if panes == nil {
				panes, why = d.terminals(ctx)
			}
			target = panes[to.ID]
			if why != nil {
				held = "messages wait: herdr did not list the panes their terminals are in"
			}
		} else {
			target, held = targetOf(*m, bosses, to)
		}
		if held != "" {
			d.note(to, held, held, why)
			continue
		}
		if !d.serve(ctx, to, target) {
			return
		}
	}
}

// targetOf is the name to prompt to by, from the poll's model m (B2): a
// worker's logical id, when an agent of that name is on the pane its workers
// row binds; a boss's name, when an agent of that name is on a pane in its
// team's boss place. Otherwise it says why to's messages wait.
func targetOf(m team.Model, bosses map[string]string, to store.Address) (target, held string) {
	switch to.Kind {
	case store.KindWorker:
		for _, p := range m.Projects {
			for _, g := range p.Groups {
				for _, w := range g.Workers {
					if w.Bound && w.ID == to.ID && w.Pane.Name == to.ID {
						return to.ID, ""
					}
				}
			}
		}
		return "", "messages wait: no agent of the worker's name is on the pane its row binds"
	case store.KindBoss:
		known := false
		for project, boss := range bosses {
			if boss != to.ID {
				continue
			}
			known = true
			for _, p := range m.Projects {
				if p.Name != project {
					continue
				}
				for _, b := range p.Boss {
					if b.Pane.Name == to.ID {
						return to.ID, ""
					}
				}
			}
		}
		if !known {
			return "", "messages wait: no configured team has this boss"
		}
		return "", "messages wait: no agent of the boss's name is in its team's boss place"
	}
	return "", "messages wait: no recipient of kind " + to.Kind + " is delivered to"
}

// terminals maps each terminal herdr lists to the pane showing it, the
// pane a pane recipient is typed to (B3): listed when it is served, so the
// pane id is the one herdr gives the terminal now, not one it reissued.
func (d *dispatcher) terminals(ctx context.Context) (map[string]string, error) {
	panes, err := d.c.ListPanes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list panes: %w", err)
	}
	out := make(map[string]string, len(panes))
	for _, p := range panes {
		if p.TerminalID != "" {
			out[p.TerminalID] = p.PaneID
		}
	}
	return out, nil
}

// serve delivers to's messages, oldest first, while they land. A refusal or
// a failure ends to's turn this pass, so neither a refused recipient nor a
// stuck pane holds up the others. It reports false once the daemon is
// stopping, which ends the pass; a stop is checked before each claim, never
// inside one (claim).
func (d *dispatcher) serve(ctx context.Context, to store.Address, target string) bool {
	for ctx.Err() == nil {
		m, ok, err := d.claim(ctx, to)
		if err != nil {
			d.warnUnlessStopping(ctx, err, "messages: claim "+to.ID+"'s next message")
			return true
		}
		if !ok {
			return true
		}
		if why := d.undeliverable(m, target); why != "" {
			d.drop(ctx, m, why)
			continue
		}
		if !d.deliver(ctx, m, target) {
			return true
		}
	}
	return false
}

// claim claims to's next message, if it has one. Once serve has decided to
// claim, a stop does not cut the claim short, as it cuts short neither
// deliver's nor drop's write: a stop inside the statement could leave the
// row dispatching while the claim reports an error, and the next start
// would fail a message that was never typed (QA #72). It is bounded on its
// own: settleTimeout ends its wait for the store's one connection, and
// SQLite ends a busy statement at store.BusyTimeout whatever the deadline,
// so a busy database holds up a stop for about 10 s at most (QA #84).
func (d *dispatcher) claim(ctx context.Context, to store.Address) (store.Message, bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	m, ok, err := d.st.Claim(ctx, to, d.now())
	if err != nil {
		return store.Message{}, false, fmt.Errorf("claim: %w", err)
	}
	return m, ok, nil
}

// deliver prompts target with m's text and records how it ended: delivered;
// queued again after a refusal that typed nothing, for the next pass; or
// failed, with a notice to its sender, after anything that may have typed it
// (A2). Once m is claimed (claim) a stop cuts short neither the prompt nor
// the write after it, each bounded on its own, so a stop never turns a
// deliverable message into a failure; a cancelled context would also interrupt the
// write's statement (NERD-5355). It reports whether m landed.
func (d *dispatcher) deliver(ctx context.Context, m store.Message, target string) bool {
	ctx = context.WithoutCancel(ctx)
	_, err := d.c.PromptAgent(ctx, target, cleanText(textOf(m)))
	sctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	log := d.log.With().Str("message", m.ID).Str("recipient", m.To.ID).Str("kind", m.To.Kind).Int("attempts", m.Attempts).Logger()
	if err == nil {
		if d.settled(log, d.st.Delivered(sctx, m.ID, d.now()), "delivered") {
			d.events.MessageDelivered(ctx, messageEvent(m, ""))
		}
		return true
	}
	if code := refusal(err); code != "" {
		d.settled(log, d.st.Requeue(sctx, m.ID, err.Error(), d.now()), "queued again")
		d.note(m.To, code, "messages wait: herdr refused the prompt before typing it", err)
		return false
	}
	log.Warn().Err(err).Msg("messages: the prompt may have been typed; the message is failed and its sender told, never retried (A2)")
	if d.settled(log, d.st.Fail(sctx, m.ID, err.Error(), d.now()), "failed") {
		d.events.MessageFailed(ctx, messageEvent(m, err.Error()))
	}
	return false
}

// undeliverable is why m, claimed for a pane recipient, is failed without a
// prompt, or "" (B3, A30): no pane shows its terminal any more, which herdr
// never gives another pane, or it has waited paneWait.
func (d *dispatcher) undeliverable(m store.Message, target string) string {
	switch {
	case m.To.Kind != store.KindPane:
		return ""
	case target == "":
		return "no pane shows the terminal it was sent to"
	case d.now().Sub(m.CreatedAt) >= paneWait:
		return "no agent in the pane took it within " + paneWait.String()
	}
	return ""
}

// drop fails m, claimed, without a prompt, why being the reason: nothing
// was typed, but neither will it be. Like deliver, it records that even
// once the daemon is stopping, so m never stays dispatching.
func (d *dispatcher) drop(ctx context.Context, m store.Message, why string) {
	ctx = context.WithoutCancel(ctx)
	sctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	log := d.log.With().Str("message", m.ID).Str("recipient", m.To.ID).Str("kind", m.To.Kind).Int("attempts", m.Attempts).Logger()
	log.Info().Str("reason", why).Msg("messages: a message to a pane is failed without a prompt")
	if d.settled(log, d.st.Fail(sctx, m.ID, why, d.now()), "failed") {
		d.events.MessageFailed(ctx, messageEvent(m, why))
	}
}

// settled reports whether the write recording how a dispatch ended landed.
// One that did not leaves the message dispatching, which holds its
// recipient's queue, until the next daemon start fails it: never typed
// twice.
func (d *dispatcher) settled(log zerolog.Logger, err error, what string) bool {
	if err != nil {
		log.Error().Err(err).Msg("messages: record the message " + what + "; it stays dispatching until the daemon restarts and fails it")
		return false
	}
	return true
}

// sweep returns every ended binding's queue to its project's boss (Q7): a
// worker retired, failed or gone, or its id bound again at a later
// generation. A project no team is configured for keeps its queue; it
// returns the workers it kept one for.
func (d *dispatcher) sweep(ctx context.Context, bosses map[string]string) (kept map[store.Address]bool) {
	ended, err := d.st.EndedQueues(ctx)
	if err != nil {
		d.warnUnlessStopping(ctx, err, "messages: read the ended queues")
		return nil
	}
	for _, q := range ended {
		boss, ok := bosses[q.Project]
		if !ok {
			to := store.Address{Kind: store.KindWorker, ID: q.Worker}
			why := "messages wait: their binding ended, and no team is configured for its project " + q.Project
			d.note(to, why, why, nil)
			if kept == nil {
				kept = map[store.Address]bool{}
			}
			kept[to] = true
			continue
		}
		returned, err := d.st.ReturnToBoss(ctx, q.Worker, q.Project, store.Address{Kind: store.KindBoss, ID: boss}, d.now())
		if err != nil {
			d.warnUnlessStopping(ctx, err, "messages: return "+q.Worker+"'s queue to "+boss)
			continue
		}
		for _, m := range returned {
			d.events.MessageReturned(ctx, messageEvent(m, ""))
		}
		if len(returned) > 0 {
			d.log.Info().Str("worker", q.Worker).Str("boss", boss).Int("messages", len(returned)).
				Msg("messages: an ended worker's queue is returned to its boss")
		}
	}
	return kept
}

// note logs why to's messages wait: at warn when key is new for to, then at
// debug while it lasts.
func (d *dispatcher) note(to store.Address, key, msg string, err error) {
	ev := d.log.Debug()
	if d.warned[to] != key {
		d.warned[to] = key
		ev = d.log.Warn()
	}
	ev.Err(err).Str("recipient", to.ID).Str("kind", to.Kind).Msg(msg)
}

// forget drops what was noted for a recipient with nothing queued now, so a
// wait that ends and begins again is logged again.
func (d *dispatcher) forget(queued []store.Address) {
	still := make(map[store.Address]bool, len(queued))
	for _, to := range queued {
		still[to] = true
	}
	for to := range d.warned {
		if !still[to] {
			delete(d.warned, to)
		}
	}
}

// warnUnlessStopping logs err unless the daemon is stopping, which ends
// every read in flight.
func (d *dispatcher) warnUnlessStopping(ctx context.Context, err error, msg string) {
	if ctx.Err() == nil {
		d.log.Warn().Err(err).Msg(msg)
	}
}

// textOf is what m is typed as (Q9, B5): a message the daemon sent as its
// body, which starts [agentisan and invites no reply; any other as
// [msg <id> from <sender>] <body>, which the recipient can reply to. A
// message returned to the boss says whom it was for.
func textOf(m store.Message) string {
	switch {
	case m.From.Kind == store.KindDaemon && m.ReturnedFrom == "":
		return m.Body
	case m.From.Kind == store.KindDaemon:
		return "[agentisan] originally to " + m.ReturnedFrom + ": " + m.Body
	case m.ReturnedFrom != "":
		return fmt.Sprintf("[msg %s from %s, originally to %s] %s", m.ID, m.From.ID, m.ReturnedFrom, m.Body)
	}
	return fmt.Sprintf("[msg %s from %s] %s", m.ID, m.From.ID, m.Body)
}

// cleanText is text as it is typed (B6). CRLF and CR become LF, LF and TAB
// stay, and every other control character, C0, DEL and C1, ESC among them,
// is dropped: herdr types the text as one bracketed paste where the agent
// enabled it, and raw otherwise, so a body could end the paste early and
// press keys. Format characters stay, so an emoji joined by ZWJ stays whole.
// The store keeps the text as sent.
func cleanText(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\r':
			return '\n'
		case r == '\n', r == '\t':
			return r
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, strings.ReplaceAll(text, "\r\n", "\n"))
}

// messageEvent is m as a message event carries it, with reason. Never its
// body.
func messageEvent(m store.Message, reason string) telemetry.Message {
	return telemetry.Message{
		ID: m.ID, FromKind: m.From.Kind, From: m.From.ID, ToKind: m.To.Kind, To: m.To.ID,
		ReturnedFrom: m.ReturnedFrom, Attempts: m.Attempts, Reason: reason,
	}
}

// recoverDispatching fails every message a crash left dispatching, and tells
// their senders, before anything can claim one: such a dispatch may or may
// not have reached its pane, so it is never redelivered (A2). It runs
// whether or not the team poll does. A failure is logged: the rows stay
// dispatching, which holds their recipients' queues, until a start that
// recovers them.
func (o Options) recoverDispatching(ctx context.Context, st *store.Store) {
	failed, err := st.RecoverDispatching(ctx, recoveredReason, o.Now())
	if err != nil {
		o.Logger.Error().Err(err).Msg("messages: fail the dispatches the last daemon did not finish; their recipients' queues wait for the next start")
		return
	}
	var events *telemetry.Events
	if o.Team != nil {
		events = o.Team.Events
	}
	for _, m := range failed {
		o.Logger.Warn().Str("message", m.ID).Str("recipient", m.To.ID).Str("kind", m.To.Kind).
			Msg("messages: the last daemon stopped mid-dispatch; the message is failed and its sender told, never retried (A2)")
		events.MessageFailed(ctx, messageEvent(m, recoveredReason))
	}
}

package team

import (
	"slices"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// CtxGate is what the poll verified about a pane's foreground agent. Agent
// is that process, zero when none was verified, and Start its wall-clock
// start (A47), zero when unknown.
type CtxGate struct {
	Start time.Time
	Agent report.Proc
}

// Ctx returns p's report when it is the current agent's (A61). A report
// that names its writing processes (ctx_owner) counts only while Agent is
// one of them, so with no verified agent it is unknown. One that names none
// is an older build's: CurrentCtx's timestamp rule.
func (g CtxGate) Ctx(p Pane) string {
	if len(p.CtxOwner) > 0 {
		if g.Agent.PID != 0 && slices.Contains(p.CtxOwner, g.Agent) {
			return p.Ctx
		}
		return ""
	}
	return CurrentCtx(p, g.Start)
}

// CurrentCtx is the rule for a ctx that names no writer (ADR-001 A61): p's
// ctx unless its ctx_at is earlier than start, when its pane's agent process
// started, since an agent before it in the pane left it, as a Codex's ctx
// lives 24 h. A pane whose start is unknown (zero) keeps it, as A56 keeps a
// reported model. So does a ctx with no ctx_at, unlike A56's model, which
// counts only with its model_at: a build before A61 wrote ctx with none.
func CurrentCtx(p Pane, start time.Time) string {
	if !start.IsZero() && !p.CtxAt.IsZero() && p.CtxAt.Before(start) {
		return ""
	}
	return p.Ctx
}

// GateCtx applies each team's ctx gates before the poll's readers see it.
func GateCtx(m *Model, gates map[string]CtxGate) {
	gate := func(ws []Worker) {
		for i := range ws {
			p := &ws[i].Pane
			p.Ctx = gates[p.ID].Ctx(*p)
		}
	}
	for i := range m.Projects {
		gate(m.Projects[i].Boss)
		for j := range m.Projects[i].Groups {
			gate(m.Projects[i].Groups[j].Workers)
		}
	}
}

// unreadableOwner is the owner of a ctx whose ctx_owner is not the one its
// ctx_at was written with: a process nobody is, so no agent verified in
// front of the pane is among them.
var unreadableOwner = []report.Proc{{}}

// ctxOwnerOf is the processes p's ctx_owner token names for the ctx stamped
// ctxAt; none when it is absent, or when herdr detects no agent in p, as for
// ctxOf. A token that is not a value the report writes, or that belongs to
// another stamp, as when an older build wrote ctx and ctx_at over it, names
// the processes of nobody.
func ctxOwnerOf(p herdr.PaneInfo, ctxAt time.Time) []report.Proc {
	v := p.Tokens[report.CtxOwnerKey]
	if p.Agent == "" || v == "" {
		return nil
	}
	at, owner, ok := report.ParseCtxOwner(v)
	if !ok || ctxAt.IsZero() || at.UnixMilli() != ctxAt.UnixMilli() {
		return unreadableOwner
	}
	return owner
}

// ctxAtOf is when p's ctx was reported, from its ctx_at token (A61); zero
// when it is absent, not a value the report writes, or when herdr detects no
// agent in p, as for ctxOf.
func ctxAtOf(p herdr.PaneInfo) time.Time {
	if p.Agent == "" {
		return time.Time{}
	}
	at, _ := report.ParseCtxAt(p.Tokens[report.CtxAtKey])
	return at
}

// StartClock holds the last poll's ctx gates by pane and terminal for team
// status. It is safe for concurrent use. A pane it holds no gate for, as
// before the poll's first look at it, has the zero gate: no owned ctx counts,
// for no agent is verified in front of it.
type StartClock struct {
	mu     sync.Mutex
	byPane map[string]paneStart
}

type paneStart struct {
	terminal string
	gate     CtxGate
}

// Observe replaces c's gates for the team panes and terminals in m.
func (c *StartClock) Observe(m Model, gates map[string]CtxGate) {
	if c == nil {
		return
	}
	next := map[string]paneStart{}
	add := func(ws []Worker) {
		for _, w := range ws {
			if gate, ok := gates[w.Pane.ID]; ok {
				next[w.Pane.ID] = paneStart{terminal: w.Pane.TerminalID, gate: gate}
			}
		}
	}
	for _, pr := range m.Projects {
		add(pr.Boss)
		for _, g := range pr.Groups {
			add(g.Workers)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byPane = next
}

// Ctx gates p's report by the gate c holds for its pane while the pane still
// shows the same terminal.
func (c *StartClock) Ctx(p Pane) string {
	var gate CtxGate
	if c != nil {
		c.mu.Lock()
		if s, ok := c.byPane[p.ID]; ok && s.terminal == p.TerminalID {
			gate = s.gate
		}
		c.mu.Unlock()
	}
	return gate.Ctx(p)
}

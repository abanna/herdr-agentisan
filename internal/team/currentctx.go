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

// ctxOwnerOf is the processes p's ctx_owner token names; none when it is
// absent, not a value the report writes, or when herdr detects no agent in p,
// as for ctxOf.
func ctxOwnerOf(p herdr.PaneInfo) []report.Proc {
	if p.Agent == "" {
		return nil
	}
	owner, _ := report.ParseCtxOwner(p.Tokens[report.CtxOwnerKey])
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
// status. It is safe for concurrent use. A nil clock applies no gate.
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

// Ctx gates p's report while its pane still shows the same terminal.
func (c *StartClock) Ctx(p Pane) string {
	if c == nil {
		return p.Ctx
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.byPane[p.ID]
	if !ok || s.terminal != p.TerminalID {
		return p.Ctx
	}
	return s.gate.Ctx(p)
}

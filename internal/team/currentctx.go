package team

import (
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// CtxGate qualifies a pane's report against the last verified foreground
// start. RequireFresh covers a lost or replaced process. After is the last
// stamp observed before a replacement, which its own report must exceed.
type CtxGate struct {
	Start, After time.Time
	RequireFresh bool
}

// Ctx keeps the legacy fallback for an initial unknown start or old token.
func (g CtxGate) Ctx(p Pane) string {
	if g.RequireFresh && (g.Start.IsZero() || !p.CtxAt.After(g.After)) {
		return ""
	}
	if !g.Start.IsZero() && !p.CtxAt.IsZero() && p.CtxAt.Before(g.Start) {
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

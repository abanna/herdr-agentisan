package team

import (
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// CurrentCtx is p's ctx while its pane's current agent reported it (ADR-001
// A61): "" when its ctx_at is earlier than start, when that agent's process
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

// GateCtx sets, in place, the ctx of every boss and worker pane of m's teams
// to CurrentCtx's, by its agent's start as started holds it by pane id, so
// every reader of the model's ctx sees the same gate (A61).
func GateCtx(m *Model, started map[string]time.Time) {
	gate := func(ws []Worker) {
		for i := range ws {
			ws[i].Pane.Ctx = CurrentCtx(ws[i].Pane, started[ws[i].Pane.ID])
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

// StartClock holds, for each agent pane of the teams the poll last saw, the
// terminal it showed and when its agent process started (A47), so a reader
// with no process reads of its own, team status, gates ctx as the poll does
// (A61), as fresh as the last poll. It is safe for concurrent use, and a nil
// clock knows nothing.
type StartClock struct {
	mu     sync.Mutex
	byPane map[string]paneStart
}

type paneStart struct {
	terminal string
	at       time.Time
}

// Observe replaces what c knows with the starts started holds for the panes
// of m's teams, each with the terminal m shows its pane on.
func (c *StartClock) Observe(m Model, started map[string]time.Time) {
	if c == nil {
		return
	}
	next := map[string]paneStart{}
	add := func(ws []Worker) {
		for _, w := range ws {
			if at, ok := started[w.Pane.ID]; ok {
				next[w.Pane.ID] = paneStart{terminal: w.Pane.TerminalID, at: at}
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

// Start is the start c holds for pane while it shows terminal: zero when c
// holds none for it, the pane showed another terminal, a reissued id, or c
// is nil.
func (c *StartClock) Start(pane, terminal string) time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.byPane[pane]
	if !ok || s.terminal != terminal {
		return time.Time{}
	}
	return s.at
}

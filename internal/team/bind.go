package team

import (
	"slices"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// A Check is one live workers row whose binding the team poll has work on
// (ADR-001 A35), as Reconcile finds it in a layout.
type Check struct {
	Row store.Worker
	// Pane is the pane herdr lists the row's terminal under; "" when it
	// lists none, after a live handoff or a restart renewed every terminal,
	// or when the pane closed.
	Pane string
	// Candidates are the panes that may hold the row's shell now, in the
	// order the poll tries them, when herdr lists its terminal nowhere and
	// the row has a shell to prove: the pane at the row's pane id, the pane
	// agent.list names the logical id, then the agent panes of a tab the
	// logical id labels. Each is listed once, has a terminal, and is no
	// other live row's.
	Candidates []herdr.PaneInfo
}

// Moved reports whether herdr lists the row's terminal under another pane
// id: the pane moved to another workspace.
func (c Check) Moved() bool { return c.Pane != "" && c.Pane != c.Row.PaneID }

// NeedsShell reports whether the row is bound but has no shell yet, for the
// poll to read from Pane.
func (c Check) NeedsShell() bool { return c.Pane != "" && c.Row.ShellPID == 0 }

// Lost reports whether the row cannot be proven on any pane: herdr lists
// its terminal nowhere, it has no shell to look for, and no handoff of it is
// pending. A row with a shell is lost only once every candidate is shown not
// to hold it.
func (c Check) Lost() bool { return c.Pane == "" && c.Row.ShellPID == 0 && !c.Row.HandoffPending }

// Reconcile is the work the poll has on rows in l: one check per live row
// that moved, has no shell yet, or whose terminal herdr lists nowhere. A row
// already lost is the boss's and has none; so has a row with nothing to
// prove while a handoff of it is pending.
func Reconcile(l Layout, rows []store.Worker) []Check {
	byTerminal := map[string]herdr.PaneInfo{}
	byID := map[string]herdr.PaneInfo{}
	for _, p := range l.Panes {
		byID[p.PaneID] = p
		if p.TerminalID != "" {
			byTerminal[p.TerminalID] = p
		}
	}
	held := map[string]bool{}
	for _, r := range rows {
		if r.Status != store.StatusLost && r.TerminalID != "" {
			held[r.TerminalID] = true
		}
	}
	names := agentNames(l.Agents)
	var out []Check
	for _, r := range rows {
		if r.Status == store.StatusLost {
			continue
		}
		c := Check{Row: r}
		if p, ok := byTerminal[r.TerminalID]; ok {
			c.Pane = p.PaneID
			if c.Moved() || c.NeedsShell() {
				out = append(out, c)
			}
			continue
		}
		if r.ShellPID != 0 {
			c.Candidates = candidates(l, r.LogicalID, byID[r.PaneID], names, held)
		} else if r.HandoffPending {
			continue
		}
		out = append(out, c)
	}
	return out
}

// candidates are the panes in l that may hold id's shell, in order: hint,
// the pane named id, then the agent panes of a tab labelled id. A pane with
// no terminal, or one a live row holds, cannot be id's.
func candidates(l Layout, id string, hint herdr.PaneInfo, names map[string]string, held map[string]bool) []herdr.PaneInfo {
	var out []herdr.PaneInfo
	add := func(p herdr.PaneInfo) {
		if p.TerminalID == "" || held[p.TerminalID] || slices.ContainsFunc(out, func(o herdr.PaneInfo) bool { return o.PaneID == p.PaneID }) {
			return
		}
		out = append(out, p)
	}
	add(hint)
	for _, p := range l.Panes {
		if names[p.PaneID] == id {
			add(p)
		}
	}
	for _, t := range l.Tabs {
		if t.Label != id {
			continue
		}
		for _, p := range l.Panes {
			if p.TabID == t.TabID && (p.Agent != "" || names[p.PaneID] != "") {
				add(p)
			}
		}
	}
	return out
}

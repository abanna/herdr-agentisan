package notice

import (
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// Watch is the team poll's source: it diffs each model the poll builds
// against the last one, as A21's status events do, and reads agent.list for
// the end of each turn. Only one goroutine uses it, the team poll's.
//
// It covers every agent pane a team places in a group, bound or not, and
// one whose agent exited, leaving a shell. A plain shell's status is noise,
// and the boss's own panes are left out, or every notice the boss handled
// would tell it about itself.
type Watch struct {
	// last is the last model observed; nil before the first.
	last *team.Model
	// agents are the group panes judged to run an agent at the last call,
	// as that call saw them. A call whose agent.list failed knows no names,
	// so it cannot tell a pane herdr knows only by name from a shell: it
	// keeps each pane judged before as it was named, and judges no exit.
	agents []placed
	// turns is each terminal's completion_seq as last read; a nil value is
	// read, and none.
	turns map[string]*uint64
	// unknown holds the panes that turned unknown at the last poll, told
	// about only if they still are at this one.
	unknown map[string]bool
}

// NewWatch is a Watch that has seen nothing yet.
func NewWatch() *Watch {
	return &Watch{turns: map[string]*uint64{}, unknown: map[string]bool{}}
}

// Observe is what changed from the last model observed to m, as notices:
//   - the first call only takes m as its baseline, and is one Started
//     notice per team in m;
//   - a worker that turned blocked, or left the team, at once: its pane
//     closed or moved away, or, when agents answered (named), its agent
//     exited and left a shell;
//   - a worker that turned unknown, when it still is at the next call;
//   - a worker whose terminal's completion_seq changed to a new value,
//     when agents is agent.list's answer for this poll (named). A terminal
//     seen for the first time is a baseline; one agent.list no longer lists
//     is forgotten. Without named, nothing is compared and the turns kept.
//
// A call is made only for a model the poll built: a poll that failed a
// list never becomes the baseline (A21).
func (w *Watch) Observe(m team.Model, agents []herdr.AgentInfo, named bool) []Notice {
	first := w.last == nil
	now := w.judge(m, named)
	var out []Notice
	if first {
		for _, p := range m.Projects {
			out = append(out, Notice{Project: p.Name, Subject: p.Name, Kind: Started, Text: startedText})
		}
	} else {
		out = append(out, w.statuses(*w.last, m, now, named)...)
	}
	if named {
		out = append(out, w.turnsOf(m, agents, first)...)
	}
	w.last, w.agents = &m, now
	return out
}

// judge is m's group panes that run an agent. Without named, a pane judged
// one before is still one, named as it was, and a pane seen for the first
// time is one only by the agent herdr detected in it. A pane judged before
// on the same terminal keeps its name while agent.list names it nothing: a
// poll reads pane.list before agent.list, so an exit between the two reads
// drops the name a poll before it drops the agent.
func (w *Watch) judge(m team.Model, named bool) []placed {
	was := map[string]team.Worker{}
	for _, a := range w.agents {
		was[a.worker.Pane.ID] = a.worker
	}
	groups := groupWorkers(m)
	out := make([]placed, 0, len(groups))
	for _, g := range groups {
		k, known := was[g.worker.Pane.ID]
		switch {
		case !named && known:
		case !g.worker.Pane.IsAgent():
			continue
		}
		if known && g.worker.Pane.Name == "" && g.worker.Pane.TerminalID == k.Pane.TerminalID {
			g.worker.Pane.Name = k.Pane.Name
		}
		out = append(out, g)
	}
	return out
}

// statuses are the notices for the status changes from prev to m, of the
// panes now judges to run an agent, and for the agent panes the last call
// judged that have left: their pane closed or moved out of every group, or,
// when agent.list answered this call (named), their agent exited and left
// a shell.
func (w *Watch) statuses(prev, m team.Model, now []placed, named bool) []Notice {
	agents := map[string]team.Worker{}
	for _, a := range now {
		agents[a.worker.Pane.ID] = a.worker
	}
	var out []Notice
	held := w.unknown
	w.unknown = map[string]bool{}
	for _, tr := range team.Transitions(prev, m) {
		worker, ok := agents[tr.Worker.Pane.ID]
		if tr.Boss || !ok {
			continue
		}
		switch tr.To {
		case string(team.StatusBlocked):
			out = append(out, Notice{Project: tr.Project, Subject: worker.Pane.ID, Kind: Blocked, Text: blockedText(worker)})
		case string(team.StatusUnknown):
			w.unknown[worker.Pane.ID] = true
		}
	}
	// shell is every pane m places, and whether it is a group pane that
	// runs no agent; a boss's pane is placed, never a shell here.
	shell := map[string]bool{}
	for _, p := range m.Projects {
		for _, b := range p.Boss {
			shell[b.Pane.ID] = false
		}
	}
	for _, g := range groupWorkers(m) {
		shell[g.worker.Pane.ID] = !g.worker.Pane.IsAgent()
	}
	for _, a := range w.agents {
		id := a.worker.Pane.ID
		isShell, there := shell[id]
		switch {
		case !there:
			out = append(out, Notice{Project: a.project, Subject: id, Kind: Gone, Text: goneText(a.worker)})
		case isShell && named:
			// An agent that exits leaves its pane a shell: the worker has
			// left the team as surely as if the pane had closed, whether or
			// not the status changed with it.
			out = append(out, Notice{Project: a.project, Subject: id, Kind: Gone, Text: exitedText(a.worker)})
		}
	}
	for _, a := range now {
		if held[a.worker.Pane.ID] && a.worker.Pane.Status == team.StatusUnknown {
			out = append(out, Notice{Project: a.project, Subject: a.worker.Pane.ID, Kind: Unknown, Text: unknownText(a.worker)})
		}
	}
	return out
}

// turnsOf are the notices for the turns that ended since the last named
// call, and keeps each terminal's completion_seq for the next. On the first
// call it only keeps them.
func (w *Watch) turnsOf(m team.Model, agents []herdr.AgentInfo, first bool) []Notice {
	byPane := map[string]herdr.AgentInfo{}
	for _, a := range agents {
		if _, dup := byPane[a.PaneID]; !dup {
			byPane[a.PaneID] = a
		}
	}
	var out []Notice
	turns := map[string]*uint64{}
	for _, g := range groupWorkers(m) {
		a, ok := byPane[g.worker.Pane.ID]
		if !ok || a.TerminalID == "" || !g.worker.Pane.IsAgent() {
			continue
		}
		turns[a.TerminalID] = a.CompletionSeq
		last, seen := w.turns[a.TerminalID]
		if first || !seen || a.CompletionSeq == nil || (last != nil && *last == *a.CompletionSeq) {
			continue
		}
		out = append(out, Notice{Project: g.project, Subject: g.worker.Pane.ID, Kind: TurnEnded, Text: turnText(g.worker)})
	}
	w.turns = turns
	return out
}

// placed is a worker in a team's group.
type placed struct {
	project string
	worker  team.Worker
}

// groupWorkers are m's workers in groups, the boss's left out, in model
// order; a pane in two places is listed once.
func groupWorkers(m team.Model) []placed {
	var out []placed
	seen := map[string]bool{}
	for _, p := range m.Projects {
		for _, g := range p.Groups {
			for _, w := range g.Workers {
				if !seen[w.Pane.ID] {
					seen[w.Pane.ID] = true
					out = append(out, placed{p.Name, w})
				}
			}
		}
	}
	return out
}

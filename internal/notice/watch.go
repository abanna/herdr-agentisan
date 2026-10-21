package notice

import (
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// Watch is the team poll's source: it diffs each model the poll builds
// against the last one, as A21's status events do, and reads agent.list for
// the end of each turn. Only one goroutine uses it, the team poll's.
//
// It covers every agent pane a team places in a group, bound or not. A
// plain shell's status is noise, and the boss's own panes are left out, or
// every notice the boss handled would tell it about itself.
type Watch struct {
	// last is the last model observed; nil before the first.
	last *team.Model
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
//   - a worker that turned blocked, or left the team, at once;
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
	var out []Notice
	if first {
		for _, p := range m.Projects {
			out = append(out, Notice{Project: p.Name, Subject: p.Name, Kind: Started, Text: startedText})
		}
	} else {
		out = append(out, w.statuses(*w.last, m)...)
	}
	if named {
		out = append(out, w.turnsOf(m, agents, first)...)
	}
	w.last = &m
	return out
}

// statuses are the notices for the status changes from prev to m.
func (w *Watch) statuses(prev, m team.Model) []Notice {
	var out []Notice
	held := w.unknown
	w.unknown = map[string]bool{}
	for _, tr := range team.Transitions(prev, m) {
		if tr.Boss || !tr.Worker.Pane.IsAgent() {
			continue
		}
		n := Notice{Project: tr.Project, Subject: tr.Worker.Pane.ID}
		switch tr.To {
		case string(team.StatusBlocked):
			n.Kind, n.Text = Blocked, blockedText(tr.Worker)
		case string(team.StatusUnknown):
			w.unknown[tr.Worker.Pane.ID] = true
			continue
		case team.Absent:
			n.Kind, n.Text = Gone, goneText(tr.Worker)
		default:
			continue
		}
		out = append(out, n)
	}
	for _, g := range groupWorkers(m) {
		if held[g.worker.Pane.ID] && g.worker.Pane.Status == team.StatusUnknown && g.worker.Pane.IsAgent() {
			out = append(out, Notice{Project: g.project, Subject: g.worker.Pane.ID, Kind: Unknown, Text: unknownText(g.worker)})
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

package notice

import (
	"slices"

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
	// untold holds the panes that turned blocked at a call whose agent.list
	// failed, which could not judge them to run an agent, while they stay
	// blocked and unjudged: each block is told at the first call that judges
	// an agent there.
	untold map[string]bool
	// turns is each terminal's completion_seq as last read; a nil value is
	// read, and none.
	turns map[string]*uint64
	// unknown holds the panes that turned unknown at the last poll, told
	// about only if they still are at this one.
	unknown map[string]bool
	// known holds the agent panes seen in a status other than unknown since
	// they were last judged to run an agent; fresh counts, for each of the
	// others, the polls it has been judged one, all of them unknown. A pane
	// that stops running an agent leaves both, and starts again if it comes
	// back, as does one whose terminal changed: a new agent runs there.
	known map[string]bool
	fresh map[string]int
}

// freshPolls is how many polls an agent pane never seen in a known status
// is unknown, counted from the first that judged it to run an agent, before
// it is told unknown: a spawned agent herdr has not yet classified is no
// news (ADR-001 A44's addendum, the cutover's F3). A pane seen known keeps
// the two-poll rule.
const freshPolls = 20

// NewWatch is a Watch that has seen nothing yet.
func NewWatch() *Watch {
	return &Watch{untold: map[string]bool{}, turns: map[string]*uint64{}, unknown: map[string]bool{}, known: map[string]bool{}, fresh: map[string]int{}}
}

// Observe is what changed from the last model observed to m, as notices:
//   - the first call only takes m as its baseline, and is one Started
//     notice per team in m;
//   - a worker that turned blocked, or from blocked to working, idle or
//     done, or left the team, at once: its pane closed or moved away, or,
//     when agents answered (named), its agent exited and left a shell;
//   - a pane that turned blocked at a call that could not judge it to run
//     an agent, at the first call that judges one there, if it is still
//     blocked; the end of that block is not told;
//   - a worker seen in a known status that turned unknown, when it still
//     is at the next call; one never seen known, at the freshPolls-th call
//     in a row that judged it to run an agent, the first call included. A
//     pane whose terminal changed runs a new agent, never seen known;
//   - a worker whose terminal's completion_seq changed to a new value,
//     when agents is agent.list's answer for this poll (named). A terminal
//     seen for the first time is a baseline; one agent.list no longer lists
//     is forgotten. Without named, nothing is compared and the turns kept.
//
// A call is made only for a model the poll built: a poll that failed a
// list never becomes the baseline (A21).
func (w *Watch) Observe(m team.Model, agents []herdr.AgentInfo, named bool) []Notice {
	first := w.last == nil
	now := w.judge(m, agents, named)
	w.renew(now)
	var out []Notice
	if first {
		for _, p := range m.Projects {
			out = append(out, Notice{Project: p.Name, Subject: p.Name, Kind: Started, Text: startedText})
		}
	} else {
		out = append(out, w.statuses(*w.last, m, now, named)...)
	}
	out = append(out, w.fresher(now)...)
	if named {
		out = append(out, w.turnsOf(m, agents, first)...)
	}
	w.last, w.agents = &m, now
	return out
}

// judge is m's group panes that run an agent. Without named, a pane judged
// one before is still one, named as it was, and a pane seen for the first
// time is one only by the agent herdr detected in it. A pane judged before
// on the same terminal keeps its name only while this call cannot name it:
// agents failed (named false), or lists neither the pane nor its terminal.
// A poll reads pane.list before agent.list, so an exit between the two
// reads drops the agent from agent.list a poll before pane.list drops it.
// A terminal agents lists with no name runs an agent nobody named, and a
// new terminal a new agent: either is its pane's.
func (w *Watch) judge(m team.Model, agents []herdr.AgentInfo, named bool) []placed {
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
		if known && g.worker.Pane.Name == "" && g.worker.Pane.TerminalID == k.Pane.TerminalID && (!named || !lists(agents, g.worker.Pane)) {
			g.worker.Pane.Name = k.Pane.Name
		}
		out = append(out, g)
	}
	return out
}

// lists reports whether agents, agent.list's answer, lists p's agent: an
// entry for p's pane, or for the terminal p shows.
func lists(agents []herdr.AgentInfo, p team.Pane) bool {
	return slices.ContainsFunc(agents, func(a herdr.AgentInfo) bool {
		return a.PaneID == p.ID || (p.TerminalID != "" && a.TerminalID == p.TerminalID)
	})
}

// renew forgets what the last calls saw of each of now's panes whose
// terminal changed since the last call judged it, as a live handoff
// reissues it: the agent there is a new one, never seen known, with no
// unknown pending. It runs before statuses, whose two-poll rule asks
// whether a pane was known before this call.
func (w *Watch) renew(now []placed) {
	was := map[string]string{}
	for _, a := range w.agents {
		was[a.worker.Pane.ID] = a.worker.Pane.TerminalID
	}
	for _, a := range now {
		id := a.worker.Pane.ID
		if term, ok := was[id]; ok && term != a.worker.Pane.TerminalID {
			delete(w.known, id)
			delete(w.fresh, id)
			delete(w.unknown, id)
		}
	}
}

// statuses are the notices for the status changes from prev to m, of the
// panes now judges to run an agent, and for the agent panes the last call
// judged that have left: their pane closed or moved out of every group, or,
// when agent.list answered this call (named), their agent exited and left
// a shell. A pane that turns blocked at a call that cannot judge it, one
// without named, waits in untold for a call that can.
func (w *Watch) statuses(prev, m team.Model, now []placed, named bool) []Notice {
	agents := map[string]team.Worker{}
	for _, a := range now {
		agents[a.worker.Pane.ID] = a.worker
	}
	var out []Notice
	held, untold := w.unknown, w.untold
	w.unknown, w.untold = map[string]bool{}, map[string]bool{}
	for _, tr := range team.Transitions(prev, m) {
		worker, ok := agents[tr.Worker.Pane.ID]
		if tr.Boss {
			continue
		}
		if !ok {
			// A pane herdr knows only by name reads as a shell here.
			if !named && tr.To == string(team.StatusBlocked) {
				w.untold[tr.Worker.Pane.ID] = true
			}
			continue
		}
		switch {
		case tr.To == string(team.StatusBlocked):
			out = append(out, Notice{Project: tr.Project, Subject: worker.Pane.ID, Worker: WorkerName(worker), Kind: Blocked, Text: blockedText(worker)})
		case tr.To == string(team.StatusUnknown) && w.known[worker.Pane.ID]:
			w.unknown[worker.Pane.ID] = true
		case tr.From == string(team.StatusBlocked) && slices.Contains(answered, team.Status(tr.To)) && !untold[worker.Pane.ID]:
			out = append(out, Notice{Project: tr.Project, Subject: worker.Pane.ID, Worker: WorkerName(worker), Kind: Unblocked, Text: unblockedText(worker)})
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
		id := g.worker.Pane.ID
		shell[id] = !g.worker.Pane.IsAgent()
		if _, judged := agents[id]; untold[id] && !judged && !named && g.worker.Pane.Status == team.StatusBlocked {
			w.untold[id] = true
		}
	}
	for _, a := range w.agents {
		id := a.worker.Pane.ID
		isShell, there := shell[id]
		switch {
		case !there:
			out = append(out, Notice{Project: a.project, Subject: id, Worker: WorkerName(a.worker), Kind: Gone, Text: goneText(a.worker)})
		case isShell && named:
			// An agent that exits leaves its pane a shell: the worker has
			// left the team as surely as if the pane had closed, whether or
			// not the status changed with it.
			out = append(out, Notice{Project: a.project, Subject: id, Worker: WorkerName(a.worker), Kind: Gone, Text: exitedText(a.worker)})
		}
	}
	for _, a := range now {
		switch id := a.worker.Pane.ID; {
		case held[id] && a.worker.Pane.Status == team.StatusUnknown:
			out = append(out, Notice{Project: a.project, Subject: id, Worker: WorkerName(a.worker), Kind: Unknown, Text: unknownText(a.worker)})
		case untold[id] && a.worker.Pane.Status == team.StatusBlocked:
			out = append(out, Notice{Project: a.project, Subject: id, Worker: WorkerName(a.worker), Kind: Blocked, Text: blockedText(a.worker)})
		}
	}
	return out
}

// fresher counts the polls now's agent panes have not been seen known, and
// is the unknown notice for each one at its freshPolls-th, once. It runs
// after statuses, whose two-poll rule asks whether a pane was known before
// this call, and after renew, which forgets a pane whose terminal changed.
func (w *Watch) fresher(now []placed) []Notice {
	var out []Notice
	in := map[string]bool{}
	for _, a := range now {
		id := a.worker.Pane.ID
		in[id] = true
		switch {
		case w.known[id]:
		case a.worker.Pane.Status != team.StatusUnknown:
			w.known[id] = true
		default:
			w.fresh[id]++
			if w.fresh[id] == freshPolls {
				out = append(out, Notice{Project: a.project, Subject: id, Worker: WorkerName(a.worker), Kind: Unknown, Text: unknownText(a.worker)})
			}
		}
	}
	for id := range w.known {
		if !in[id] {
			delete(w.known, id)
		}
	}
	for id := range w.fresh {
		if !in[id] {
			delete(w.fresh, id)
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
		out = append(out, Notice{Project: g.project, Subject: g.worker.Pane.ID, Worker: WorkerName(g.worker), Kind: TurnEnded, Text: turnText(g.worker)})
	}
	w.turns = turns
	return out
}

// answered are the statuses a blocked worker turns to once its prompt is
// answered: told as Unblocked. One that turns unknown is told as unknown,
// and one that left as gone.
var answered = []team.Status{team.StatusWorking, team.StatusIdle, team.StatusDone}

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

package team

// Absent is what a Transition reports on the side of a poll a pane was not in
// the model: From for a pane that appeared, To for one that left (closed, or
// moved out of every place the resolver knows). It is not a Status: no pane
// in a model has it.
const Absent = "absent"

// Transition is one pane's status changing between two models: the status
// changes of D10's events.
type Transition struct {
	Project string
	// Group is the group's name; "" for the boss.
	Group string
	Boss  bool
	// Worker is the pane's worker as the newer model has it, or as the older
	// one had it for a pane that left.
	Worker Worker
	// From and To are the pane's Status before and after, or Absent.
	From, To string
}

// placed is a worker and where the model has it.
type placed struct {
	project, group string
	boss           bool
	worker         Worker
}

func (p placed) transition(from, to string) Transition {
	return Transition{Project: p.project, Group: p.group, Boss: p.boss, Worker: p.worker, From: from, To: to}
}

// placements lists every worker of m, the boss's first in each project, in
// model order. A pane in more than one place, which no resolver produces
// today, is listed once, where it is first.
func placements(m Model) []placed {
	var out []placed
	seen := map[string]bool{}
	add := func(p placed) {
		if !seen[p.worker.Pane.ID] {
			seen[p.worker.Pane.ID] = true
			out = append(out, p)
		}
	}
	for _, pr := range m.Projects {
		for _, w := range pr.Boss {
			add(placed{project: pr.Name, boss: true, worker: w})
		}
		for _, g := range pr.Groups {
			for _, w := range g.Workers {
				add(placed{project: pr.Name, group: g.Name, worker: w})
			}
		}
	}
	return out
}

// Transitions are the status changes from prev to next, keyed by pane id:
// first each pane of next whose status differs from prev's, or that prev
// did not have (From Absent), in next's order; then each pane prev had and
// next does not (To Absent), in prev's order.
//
// The logical id is carried, never compared. So a rotation reads as the old
// pane leaving and the new one appearing under the same id, and a poll that
// read no workers rows, whose ids are all provisional, changes nothing. A
// pane that moves keeping its status is no transition either.
//
// From an empty prev every pane appears: a caller diffs only between models
// it built, never against one it has not got.
func Transitions(prev, next Model) []Transition {
	before := map[string]Status{}
	for _, p := range placements(prev) {
		before[p.worker.Pane.ID] = p.worker.Pane.Status
	}
	var out []Transition
	after := map[string]bool{}
	for _, p := range placements(next) {
		after[p.worker.Pane.ID] = true
		from := Absent
		if s, had := before[p.worker.Pane.ID]; had {
			from = string(s)
		}
		if to := string(p.worker.Pane.Status); from != to {
			out = append(out, p.transition(from, to))
		}
	}
	for _, p := range placements(prev) {
		if !after[p.worker.Pane.ID] {
			out = append(out, p.transition(string(p.worker.Pane.Status), Absent))
		}
	}
	return out
}

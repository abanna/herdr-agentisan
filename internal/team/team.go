// Package team is the daemon's domain model of the team (ADR-001 D5):
// project → group → logical worker → current pane, rebuilt from herdr and
// state.db on every poll, and the `$team` token derived from it (D4).
//
// The tree does not depend on the layout. A Resolver maps herdr's layout onto
// it: which panes belong to which project and group, which are the boss's,
// and where each group's derived tokens go. Spaces is today's resolver, the ◆
// group spaces; the 8-tab project layout (NERD-5263) replaces only the
// resolver.
//
// The model holds nothing between polls. Build is a function of herdr's
// answers and the workers rows alone, so a restarted daemon rebuilds the same
// model, and a crash costs only the restart (D3).
package team

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// HandoffKey is the pane token a worker ready to be replaced carries (A5).
const HandoffKey = "handoff"

// Status is a pane's agent status as herdr reports it.
type Status string

// The statuses herdr 0.9.3 reports. Anything else, a status a newer herdr
// adds included, is StatusUnknown.
const (
	StatusWorking Status = "working"
	StatusIdle    Status = "idle"
	StatusDone    Status = "done"
	StatusBlocked Status = "blocked"
	StatusUnknown Status = "unknown"
)

func statusOf(s string) Status {
	switch st := Status(s); st {
	case StatusWorking, StatusIdle, StatusDone, StatusBlocked:
		return st
	default:
		return StatusUnknown
	}
}

// Model is the team: every project the resolver places panes in, sorted by
// name.
type Model struct {
	Projects []Project
}

// Project is one project's team. Name is the configured project's name; ""
// is the unnamed project, which the spaces belong to when the configuration
// names none.
type Project struct {
	Name string
	// Boss holds the panes in the boss's place, the ◆ boss space today.
	Boss   []Worker
	Groups []Group
}

// Group is one group of workers, such as coders, sorted by name within its
// project.
type Group struct {
	Name string
	// Targets are where the group's derived tokens go, sorted. A group has
	// one, or more when the layout puts it in several places, such as two
	// spaces with the same label.
	Targets []Target
	// Workers are sorted by target, then pane id.
	Workers []Worker
}

// Target is where a group's derived tokens go. The spaces resolver targets a
// workspace; a resolver that targets something else adds a field here, and
// the daemon learns to report to it.
type Target struct {
	WorkspaceID string
}

// Worker is one logical worker and its current pane.
type Worker struct {
	// ID is the logical id state.db's workers table binds to the pane. A
	// pane no row binds, such as a hand-started agent or a shell, is still
	// a worker: its ID is provisionally its pane id, and Bound is false.
	// herdr reissues pane ids (on a live handoff, for one), so a
	// provisional id only lasts as long as the pane's.
	ID    string
	Bound bool
	// Target is where the worker sits; zero for the boss.
	Target Target
	Pane   Pane
}

// Pane is what the model reads of a herdr pane.
type Pane struct {
	ID string
	// Agent is the agent herdr detected, such as "claude"; "" for a shell.
	Agent  string
	Status Status
	// Handoff is true when the pane carries a non-empty handoff token.
	Handoff bool
}

// Layout is herdr's answers the model is built from: one pane.list and one
// workspace.list.
type Layout struct {
	Workspaces []herdr.WorkspaceInfo
	Panes      []herdr.PaneInfo
}

// Place is one part of the layout a resolver gives a role: a group target
// and the panes in it, or panes that are the boss's.
type Place struct {
	Project string
	// Group is the group's name; "" for the boss.
	Group string
	Boss  bool
	// Target is the group's target; zero for the boss.
	Target Target
	Panes  []herdr.PaneInfo
}

// A Resolver maps herdr's layout onto the model. Panes it places nowhere are
// not part of the model.
type Resolver interface {
	Resolve(l Layout) []Place
}

// Build builds the model: the resolver places herdr's panes, and the workers
// rows give the panes they bind their logical ids. The join is by pane id
// and gives ids only; where a worker sits is the resolver's, so a bound pane
// moved into another group's space is counted where it is. A row whose pane
// herdr no longer lists has no current pane and is not in the model. When
// two rows bind one pane, the lowest logical id takes it.
func Build(l Layout, r Resolver, workers []store.Worker) Model {
	ids := map[string]string{}
	for _, w := range workers {
		if id, taken := ids[w.PaneID]; !taken || w.LogicalID < id {
			ids[w.PaneID] = w.LogicalID
		}
	}

	projects := map[string]*Project{}
	groups := map[string]map[string]*Group{}
	for _, pl := range r.Resolve(l) {
		pr, ok := projects[pl.Project]
		if !ok {
			pr = &Project{Name: pl.Project}
			projects[pl.Project], groups[pl.Project] = pr, map[string]*Group{}
		}
		ws := make([]Worker, 0, len(pl.Panes))
		for _, p := range pl.Panes {
			ws = append(ws, worker(p, pl.Target, ids))
		}
		if pl.Boss {
			pr.Boss = append(pr.Boss, ws...)
			continue
		}
		g, ok := groups[pl.Project][pl.Group]
		if !ok {
			g = &Group{Name: pl.Group}
			groups[pl.Project][pl.Group] = g
		}
		if !slices.Contains(g.Targets, pl.Target) {
			g.Targets = append(g.Targets, pl.Target)
		}
		g.Workers = append(g.Workers, ws...)
	}

	var m Model
	for _, name := range slices.Sorted(maps.Keys(projects)) {
		pr := projects[name]
		slices.SortFunc(pr.Boss, byPlace)
		for _, gname := range slices.Sorted(maps.Keys(groups[name])) {
			g := groups[name][gname]
			slices.SortFunc(g.Targets, func(a, b Target) int { return strings.Compare(a.WorkspaceID, b.WorkspaceID) })
			slices.SortFunc(g.Workers, byPlace)
			pr.Groups = append(pr.Groups, *g)
		}
		m.Projects = append(m.Projects, *pr)
	}
	return m
}

func worker(p herdr.PaneInfo, t Target, ids map[string]string) Worker {
	w := Worker{ID: p.PaneID, Target: t, Pane: Pane{
		ID: p.PaneID, Agent: p.Agent, Status: statusOf(p.AgentStatus), Handoff: p.Tokens[HandoffKey] != "",
	}}
	if id, ok := ids[p.PaneID]; ok {
		w.ID, w.Bound = id, true
	}
	return w
}

// byPlace orders workers by target, then pane id.
func byPlace(a, b Worker) int {
	return cmp.Or(strings.Compare(a.Target.WorkspaceID, b.Target.WorkspaceID), strings.Compare(a.Pane.ID, b.Pane.ID))
}

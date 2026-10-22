// Package team is the daemon's domain model of the team (ADR-001 D5):
// project → group → logical worker → current pane, rebuilt from herdr and
// state.db on every poll, and the `$team` token derived from it (D4).
//
// The tree does not depend on the layout. A Resolver maps herdr's layout onto
// it: which panes belong to which project and group, which are the boss's,
// and where each group's derived tokens go. Spaces resolves the ◆ group
// spaces (A17); Tabs resolves the team model's one workspace per team and one
// tab per worker (A19). The daemon runs one of each layout's resolvers per
// configured project, joined with Resolvers.
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
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
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

// Model is the teams: every project the resolver finds a site for or places
// panes in, sorted by name.
type Model struct {
	Projects []Project
}

// Project is one project's team. Name is the configured project's name; ""
// is the unnamed project, which the spaces belong to when the configuration
// names none.
type Project struct {
	Name string
	// Short is a tabs team's short code, which starts its tab labels; ""
	// for the spaces.
	Short string
	// Color is the project's configured colour as #rrggbb; "" when unset.
	Color string
	// Workspaces are the ids of the workspaces the team occupies, sorted,
	// whether or not any worker is there yet: a tabs team's workspace, or
	// every ◆ space, ◆ boss and ◆ herdr included.
	Workspaces []string
	// Order is the order the project's groups are shown in. Groups it does
	// not name follow by name.
	Order []string
	// Target is where the team's `$team` goes when the layout gives the
	// whole team one place, a tabs team's workspace, so a team with no
	// worker yet still has a count there; zero for the spaces, whose
	// groups each have their own.
	Target Target
	// Boss holds the panes in the boss's place: the ◆ boss space, or a tabs
	// team's <short>-boss tab.
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
	// TabID is the tab herdr lists the pane in; "" when it lists none.
	TabID string
	// TerminalID is the terminal pane.list says the pane shows, which a
	// reissued pane id does not carry over (A35); "" when it lists none.
	TerminalID string
	// Name is the name herdr knows the pane's agent by (agent.list), the
	// name agent.focus takes; "" for a shell or an agent nobody named.
	Name string
	// Label is the label the user gave the pane; "" when it has none.
	Label string
	// Agent is the agent herdr detected, such as "claude"; "" for a shell.
	Agent  string
	Status Status
	// Handoff is true when the pane carries a non-empty handoff token.
	Handoff bool
	// Ctx, Item and Stage are the pane's ctx, item and stage tokens as
	// their writers left them (ADR-001 token contract); "" when absent, and
	// Ctx "" too when herdr detects no agent in the pane (A27).
	Ctx, Item, Stage string
	// StageAt is when the stage was last reported, from its stage_at token
	// (A42); zero when absent or not a time report.ParseStageAt reads.
	StageAt time.Time
}

// IsAgent reports whether an agent runs in the pane: herdr detected one, or
// knows one by name. Anything else is a plain shell.
func (p Pane) IsAgent() bool {
	return p.Agent != "" || p.Name != ""
}

// Layout is herdr's answers the model is built from: one pane.list, one
// workspace.list, one agent.list and, when a tabs team is configured, one
// tab.list.
type Layout struct {
	Workspaces []herdr.WorkspaceInfo
	Panes      []herdr.PaneInfo
	// Agents gives the panes' agents their names. `$team` needs none, so
	// nil, as when agent.list failed, only leaves every pane unnamed.
	Agents []herdr.AgentInfo
	// Tabs gives panes their tab labels. Only Tabs reads it.
	Tabs []herdr.TabInfo
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

// Site is a project's team as a resolver finds it in the layout, whether or
// not it places any pane there. See Project for the fields.
type Site struct {
	Project    string
	Short      string
	Color      string
	Workspaces []string
	Order      []string
	Target     Target
}

// Resolution is a resolver's answer: the teams it finds and the places it
// gives their panes.
type Resolution struct {
	Sites  []Site
	Places []Place
}

// A Resolver maps herdr's layout onto the model. Panes it places nowhere are
// not part of the model.
type Resolver interface {
	Resolve(l Layout) Resolution
}

// Resolvers resolves with each resolver in turn. The daemon runs one per
// configured team; their layouts never claim the same workspace, since a
// tabs team's label is a project name and a space's starts with ◆.
type Resolvers []Resolver

// Resolve joins every resolver's sites and places.
func (rs Resolvers) Resolve(l Layout) Resolution {
	var out Resolution
	for _, r := range rs {
		res := r.Resolve(l)
		out.Sites = append(out.Sites, res.Sites...)
		out.Places = append(out.Places, res.Places...)
	}
	return out
}

// Build builds the model: the resolver finds the teams and places herdr's
// panes, and the workers rows give the panes they bind their logical ids. A
// team with a site is in the model even with no pane placed in it. A row
// binds the pane herdr lists with its terminal, never the pane at its pane
// id, which herdr can reissue (A35); a lost row, or one with no terminal,
// binds none. The join gives ids only; where a worker sits is the
// resolver's, so a bound pane moved into another group's space is counted
// where it is. A row whose terminal herdr no longer lists has no current
// pane and is not in the model. When two rows bind one terminal, the lowest
// logical id takes it.
//
// agent.list's names join by pane id the same way: an empty name names
// nothing, and when it lists one pane twice the lowest name takes it.
func Build(l Layout, r Resolver, workers []store.Worker) Model {
	ids := Bindings(workers)
	names := agentNames(l.Agents)

	projects := map[string]*Project{}
	groups := map[string]map[string]*Group{}
	project := func(name string) *Project {
		pr, ok := projects[name]
		if !ok {
			pr = &Project{Name: name}
			projects[name], groups[name] = pr, map[string]*Group{}
		}
		return pr
	}
	res := r.Resolve(l)
	for _, s := range res.Sites {
		pr := project(s.Project)
		pr.Short, pr.Color, pr.Order, pr.Target = s.Short, s.Color, s.Order, s.Target
		pr.Workspaces = append(pr.Workspaces, s.Workspaces...)
	}
	for _, pl := range res.Places {
		pr := project(pl.Project)
		ws := make([]Worker, 0, len(pl.Panes))
		for _, p := range pl.Panes {
			ws = append(ws, worker(p, pl.Target, ids, names))
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
		slices.Sort(pr.Workspaces)
		pr.Workspaces = slices.Compact(pr.Workspaces)
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

// Bindings maps each terminal the workers rows bind to the logical id that
// binds it, as Build joins them: a lost row, or one with no terminal, binds
// none, and when two rows bind one terminal the lowest logical id takes it.
func Bindings(workers []store.Worker) map[string]string {
	ids := map[string]string{}
	for _, w := range workers {
		if w.TerminalID == "" || w.Status == store.StatusLost {
			continue
		}
		if id, taken := ids[w.TerminalID]; !taken || w.LogicalID < id {
			ids[w.TerminalID] = w.LogicalID
		}
	}
	return ids
}

// agentNames gives each pane agent.list's name for its agent: an empty name
// names nothing, and when it lists one pane twice the lowest name takes it.
func agentNames(agents []herdr.AgentInfo) map[string]string {
	names := map[string]string{}
	for _, a := range agents {
		if n, taken := names[a.PaneID]; a.Name != "" && (!taken || a.Name < n) {
			names[a.PaneID] = a.Name
		}
	}
	return names
}

// worker is pane p at t, with the logical id ids gives its terminal.
func worker(p herdr.PaneInfo, t Target, ids, names map[string]string) Worker {
	w := Worker{ID: p.PaneID, Target: t, Pane: Pane{
		ID: p.PaneID, Name: names[p.PaneID], Label: p.Label, Agent: p.Agent,
		Status: statusOf(p.AgentStatus), Handoff: p.Tokens[HandoffKey] != "",
		Ctx: ctxOf(p), Item: p.Tokens[report.ItemKey], Stage: p.Tokens[report.StageKey],
		TabID: p.TabID, TerminalID: p.TerminalID,
	}}
	if id, ok := ids[p.TerminalID]; ok {
		w.ID, w.Bound = id, true
	}
	if at, ok := report.ParseStageAt(p.Tokens[report.StageAtKey]); ok {
		w.Pane.StageAt = at
	}
	return w
}

// byPlace orders workers by target, then pane id.
func byPlace(a, b Worker) int {
	return cmp.Or(strings.Compare(a.Target.WorkspaceID, b.Target.WorkspaceID), strings.Compare(a.Pane.ID, b.Pane.ID))
}

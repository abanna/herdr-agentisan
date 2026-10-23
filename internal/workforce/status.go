package workforce

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// StatusRequest is team status.
type StatusRequest struct {
	Selector
}

// Member is one pane of a team: a worker, or one of the boss's panes.
type Member struct {
	// ID is the worker's logical id: its row's, else its tab's label (a
	// tabs team), else its pane id.
	ID string `json:"id"`
	// Recorded is true when a live workers row binds the pane.
	Recorded bool   `json:"recorded"`
	PaneID   string `json:"pane_id"`
	// Agent is the agent herdr detects, such as "claude"; "" for a shell.
	Agent string `json:"agent"`
	// Provider is the agent its row records it was launched as, claude or
	// codex (ADR-001 A66); absent for a pane no row binds and a row bound
	// before migration 0008. Beside Agent, it shows a pane that now runs
	// another agent than its worker was launched as.
	Provider string `json:"provider,omitempty"`
	Status   string `json:"status"`
	// Ctx is the context used, from the ctx token; null when absent, not a
	// percentage or on a pane herdr detects no agent in (ADR-001 A27), never
	// 0 for unknown.
	Ctx   *int   `json:"ctx"`
	Item  string `json:"item"`
	Stage string `json:"stage"`
	// IdleS is how long the pane has been idle or done, in seconds, as
	// far as this daemon has seen; null when it is not at rest, or when
	// the poll does not time its team: the poll reads the teams once, at
	// start (A19), so one added since has none until the daemon restarts.
	IdleS *int64 `json:"idle_s"`
	// StageStale is set when the stage no longer says what the worker is
	// doing (ADR-001 A42): nothing reported it again in the pane's latest
	// turn, as far as this daemon has seen, or herdr detects no agent in it.
	StageStale bool `json:"stage_stale"`
	// StageAt is when the stage was last reported, from the pane's stage_at
	// token; null when absent, unreadable or ahead of the daemon's clock.
	StageAt *time.Time `json:"stage_at"`
}

// GroupStatus is one group of a team.
type GroupStatus struct {
	Name string `json:"name"`
	// Workers and Slots are the group's configured default and cap; 0 for
	// a type found in herdr but not configured.
	Workers int `json:"workers"`
	Slots   int `json:"slots"`
	// Occupied counts the panes taking a slot (see occupied); Open is
	// Slots-Occupied, never below 0.
	Occupied   int      `json:"occupied"`
	Open       int      `json:"open"`
	Configured bool     `json:"configured"`
	Targets    []string `json:"targets"`
	Members    []Member `json:"members"`
}

// UnboundWorker is a live workers row of the team that binds no pane
// (ADR-001 A35).
type UnboundWorker struct {
	ID    string `json:"id"`
	Group string `json:"group"`
	// PaneID is the pane the row was last seen on: a hint, never a pane to
	// act on.
	PaneID string `json:"pane_id"`
	// Status is the row's: lost once the team poll found no pane holding
	// it, else what it was while herdr lists its terminal nowhere, until
	// the poll re-keys or loses it.
	Status string `json:"status"`
	// Provider is the agent the row records it was launched as (A66);
	// absent for a row bound before migration 0008.
	Provider string `json:"provider,omitempty"`
}

// StatusResult is team status's answer.
type StatusResult struct {
	Project string `json:"project"`
	Layout  string `json:"layout"`
	// Workspaces are the team's workspaces in herdr.
	Workspaces []string      `json:"workspaces"`
	Groups     []GroupStatus `json:"groups"`
	Boss       []Member      `json:"boss"`
	// Unbound are the team's live rows no pane is bound to, by id.
	Unbound []UnboundWorker `json:"unbound"`
}

// Status reports a team: every group, configured or found, with its counts
// and members, and the boss's panes. Unlike the ops that change a team, it
// reports the ◆ spaces project too, when --project names it.
func (s *Service) Status(ctx context.Context, req StatusRequest) (StatusResult, error) {
	teams, err := s.teams()
	if err != nil {
		return StatusResult{}, err
	}
	l, err := s.readLayout(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	t, err := selectTeam(req.Selector, teams, l)
	if err != nil {
		return StatusResult{}, err
	}
	p, err := s.profile(t.Project)
	if err != nil {
		return StatusResult{}, err
	}
	rows, err := s.Store.Workers(ctx)
	if err != nil {
		return StatusResult{}, fmt.Errorf("read workers: %w", err)
	}
	m := team.Build(l.Layout, resolverOf(t, l.dashboardTitle), rows)
	var pr team.Project
	if i := slices.IndexFunc(m.Projects, func(p team.Project) bool { return p.Name == t.Project }); i >= 0 {
		pr = m.Projects[i]
	}
	out := StatusResult{Project: t.Project, Layout: t.Layout, Workspaces: slices.Clone(pr.Workspaces), Groups: []GroupStatus{}, Boss: []Member{}, Unbound: unbound(t.Project, l, rows)}
	if out.Workspaces == nil {
		out.Workspaces = []string{}
	}
	for _, w := range pr.Boss {
		out.Boss = append(out.Boss, s.member(w, layout{}, nil))
	}
	// A tabs team's worker is named by its tab's label when no row binds it.
	labels := l
	if t.Layout != settings.LayoutTabs {
		labels = layout{}
	}
	found := map[string]team.Group{}
	for _, g := range pr.Groups {
		found[g.Name] = g
	}
	all := configured(p)
	for name := range found {
		if !slices.Contains(all, name) {
			all = append(all, name)
		}
	}
	// The team model's own order, the spaces' v12 one included; a team
	// herdr shows no workspace of has none, and is by name.
	for _, name := range groupOrder(all, pr.Order) {
		cfg, ok := p.Groups[name]
		g := GroupStatus{Name: name, Workers: cfg.Workers, Slots: cfg.Slots, Configured: ok && !cfg.DescribesOnly, Occupied: occupied(pr, name), Targets: []string{}, Members: []Member{}}
		g.Open = max(g.Slots-g.Occupied, 0)
		for _, tg := range found[name].Targets {
			g.Targets = append(g.Targets, tg.WorkspaceID)
		}
		for _, w := range found[name].Workers {
			g.Members = append(g.Members, s.member(w, labels, rows))
		}
		out.Groups = append(out.Groups, g)
	}
	return out, nil
}

// unbound are project's rows that bind no pane in l: lost, or with a
// terminal herdr lists nowhere, rows being in logical id order.
func unbound(project string, l layout, rows []store.Worker) []UnboundWorker {
	listed := map[string]bool{}
	for _, p := range l.Panes {
		listed[p.TerminalID] = p.TerminalID != ""
	}
	out := []UnboundWorker{}
	for _, r := range rows {
		if r.Project == project && (r.Status == store.StatusLost || !listed[r.TerminalID]) {
			out = append(out, UnboundWorker{ID: r.LogicalID, Group: r.Group, PaneID: r.PaneID, Status: r.Status, Provider: r.Provider})
		}
	}
	return out
}

// member reports one worker of the model, named by its tab's label in l
// when no row binds it, with the provider its row among rows records.
func (s *Service) member(w team.Worker, l layout, rows []store.Worker) Member {
	m := Member{
		ID: w.ID, Recorded: w.Bound, PaneID: w.Pane.ID, Agent: w.Pane.Agent, Status: string(w.Pane.Status),
		Item: w.Pane.Item, Stage: w.Pane.Stage,
	}
	if i := slices.IndexFunc(rows, func(r store.Worker) bool { return w.Bound && r.LogicalID == w.ID }); i >= 0 {
		m.Provider = rows[i].Provider
	}
	if !w.Bound {
		if p, ok := l.pane(w.Pane.ID); ok {
			if label := l.tabLabel(p.TabID); label != "" {
				m.ID = label
			}
		}
	}
	// The poll's last starts gate it as they gate the snapshot's (A61).
	if n, ok := team.ParseCtx(team.CurrentCtx(w.Pane, s.Starts.Start(w.Pane.ID, w.Pane.TerminalID))); ok {
		m.Ctx = &n
	}
	if since, ok := s.Idle.Since(w.Pane.ID); ok {
		idle := max(int64(s.now().Sub(since).Seconds()), 0)
		m.IdleS = &idle
	}
	st := team.StageOf(w.Pane, s.Idle, s.now())
	m.StageStale = st.Stale
	if !st.At.IsZero() {
		at := st.At.UTC()
		m.StageAt = &at
	}
	return m
}

// occupied is how many of a type's slots are taken in a team: every pane of
// the type's group running an agent, recorded or not (a worker started by
// hand counts), and every pane a live row binds there, whose agent may still
// be starting.
func occupied(pr team.Project, typ string) int {
	n := 0
	for _, g := range pr.Groups {
		if g.Name != typ {
			continue
		}
		for _, w := range g.Workers {
			if w.Bound || w.Pane.IsAgent() {
				n++
			}
		}
	}
	return n
}

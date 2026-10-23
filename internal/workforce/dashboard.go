package workforce

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// DashboardPane names the plugin pane that runs the team dashboard: the
// plugin's id, its manifest entrypoint and that entrypoint's title, which
// herdr labels the pane it opens with (internal/plugin's ID, DashboardPane
// and DashboardTitle).
type DashboardPane struct {
	PluginID, Entrypoint, Title string
}

// Dashboard is a team's dashboard pane.
type Dashboard struct {
	PaneID string `json:"pane_id"`
	// Opened is true when this op opened it, false when it was open.
	Opened bool `json:"opened"`
}

// DashboardOutcome is what team up did about the team's dashboard. It is best
// effort, so it never fails the op: the error says why there is none, or what
// the one it opened still lacks.
type DashboardOutcome struct {
	Dashboard      *Dashboard `json:"dashboard,omitempty"`
	DashboardError string     `json:"dashboard_error,omitempty"`
}

// dashboardBudget bounds the dashboard's herdr calls, which run before the
// agents start. The split, label, swap or focus it ends is out of time, as a
// spawn's calls are; the layout's read before them, which every op shares,
// stays a herdr failure.
const dashboardBudget = 5 * time.Second

// ensureDashboard finds project's dashboard, a pane in its workspace labelled
// team.DashboardLabel or, as one prefix+d opened, with the entrypoint's title
// (team.Tabs.Dashboard, which also leaves it out of the team), or
// opens one above the boss's pane, the first agent in its tab, as the
// snapshot names the boss. herdr splits only right or down, so that is a
// split down from the boss, then a swap with it. The label comes first: a
// swap that fails still leaves a dashboard the next run finds. herdr's swap
// focuses the dashboard in the boss's tab and switches to that tab, so the
// boss is focused again, which the tab keeps for the user's next visit, and
// then the pane that had focus, when it was not the boss. Only a swap herdr
// declined, naming why, moved no focus; one that failed otherwise may have
// swapped, its reply lost, so the boss is focused after it too. It holds the
// workforce lock, so two runs open one.
func (s *Service) ensureDashboard(ctx context.Context, project string) DashboardOutcome {
	d := s.Dashboard
	if d.PluginID == "" || d.Entrypoint == "" {
		return DashboardOutcome{DashboardError: "this daemon names no dashboard pane to open"}
	}
	ctx, cancel := context.WithTimeout(ctx, dashboardBudget)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(ctx, Selector{Project: project})
	if err != nil {
		return DashboardOutcome{DashboardError: err.Error()}
	}
	if st.workspace == "" {
		return DashboardOutcome{DashboardError: "the team has no workspace labelled " + project}
	}
	label := team.DashboardLabel(st.team.Short)
	dash := team.Tabs{Short: st.team.Short, DashboardTitle: d.Title}
	if i := slices.IndexFunc(st.layout.Panes, func(p herdr.PaneInfo) bool {
		return p.WorkspaceID == st.workspace && dash.Dashboard(p)
	}); i >= 0 {
		return DashboardOutcome{Dashboard: &Dashboard{PaneID: st.layout.Panes[i].PaneID}}
	}
	i := slices.IndexFunc(st.model.Boss, func(w team.Worker) bool { return w.Pane.IsAgent() })
	if i < 0 {
		return DashboardOutcome{DashboardError: "no agent runs in the " + st.resolver().BossTab() + " tab to open the dashboard above"}
	}
	boss := st.model.Boss[i].Pane.ID
	p, err := s.Herdr.OpenPluginSplit(ctx, herdr.PluginSplit{PluginID: d.PluginID, Entrypoint: d.Entrypoint, TargetPaneID: boss, Direction: "down"})
	if err != nil {
		return DashboardOutcome{DashboardError: callFailed(ctx, "open the dashboard", err).Error()}
	}
	var errs []error
	if err := s.Herdr.RenamePane(ctx, p.PaneID, label); err != nil {
		errs = append(errs, callFailed(ctx, "label the dashboard "+label, err))
	}
	var back []string
	swapErr := s.Herdr.SwapPanes(ctx, p.PaneID, boss)
	if swapErr != nil {
		errs = append(errs, callFailed(ctx, "move the dashboard above the boss; it is below", swapErr))
	}
	if !errors.Is(swapErr, herdr.ErrSwapDeclined) {
		back = append(back, boss)
	}
	if focused := focusedPane(st.layout.Panes); focused != "" && !slices.Contains(back, focused) {
		back = append(back, focused)
	}
	for _, id := range back {
		if _, err := s.Herdr.FocusPane(ctx, id); err != nil {
			errs = append(errs, callFailed(ctx, "give focus back to "+id, err))
		}
	}
	out := DashboardOutcome{Dashboard: &Dashboard{PaneID: p.PaneID, Opened: true}}
	if len(errs) > 0 {
		out.DashboardError = strings.ReplaceAll(errors.Join(errs...).Error(), "\n", "; ")
	}
	return out
}

// focusedPane is the pane with the user's focus; "" when none has it.
func focusedPane(panes []herdr.PaneInfo) string {
	i := slices.IndexFunc(panes, func(p herdr.PaneInfo) bool { return p.Focused })
	if i < 0 {
		return ""
	}
	return panes[i].PaneID
}

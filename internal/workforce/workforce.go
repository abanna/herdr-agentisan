// Package workforce is the boss's team commands (ADR-001 D11, A23), run by
// the daemon on a project's team (A19): team status here; worker retire,
// worker spawn and team up follow. On a tabs team each worker is one tab in
// the team's workspace, labelled with its logical id, <short>-<type>-<n>,
// which is also its herdr agent name; the workers table binds that id to
// its pane (I1).
//
// The service never acts on its own (D1): it answers only when an op asks,
// and every op reads the configuration afresh, so an edit to
// herdr-agentisan.toml applies to the next one.
package workforce

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// Sentinel errors, one per refusal the boss can act on. The daemon answers
// each with its own code (daemon/workforce.go).
var (
	// ErrBadRequest means the request itself is wrong, such as no way to
	// tell which team is meant.
	ErrBadRequest = errors.New("bad request")
	// ErrUnknownProject means --project names no configured project.
	ErrUnknownProject = errors.New("no such project")
	// ErrConfig means herdr-agentisan.toml does not resolve for the project.
	ErrConfig = errors.New("the configuration does not resolve")
	// ErrUnknownWorker means retire's or handoff ready's target names no
	// worker or pane.
	ErrUnknownWorker = errors.New("no such worker")
	// ErrNotAWorker means retire's target is the boss's, not a worker's, or
	// handoff ready's is no agent pane in a team's group.
	ErrNotAWorker = errors.New("not a worker")
	// ErrPaneChanged means retire's pane no longer shows the terminal it
	// read, which herdr lists elsewhere, or handoff ready's no longer does:
	// nothing was closed or written, and a retry reads the panes afresh.
	ErrPaneChanged = errors.New("the pane changed since it was read")
	// ErrHandoffPending means retire would end a row whose handoff is still
	// pending, which would strand the handoff and hold the worker's inbox.
	ErrHandoffPending = errors.New("the worker's handoff is pending")
	// ErrHandoffTimeout means the worker wrote no handoff in time; its pane
	// stays open.
	ErrHandoffTimeout = errors.New("no handoff in time")
	// ErrHerdr means a herdr call failed.
	ErrHerdr = errors.New("herdr call failed")
)

// DefaultHandoffPoll is how often retire --after-handoff looks for the
// worker's handoff token; Service.HandoffPoll overrides it.
const DefaultHandoffPoll = 2 * time.Second

// Service runs the workforce ops. The daemon holds one; its lock serializes
// every change to a team.
type Service struct {
	// Herdr reaches the daemon's own server. The daemon gives one whose
	// Dialed hook proves the server's identity on every call (A3).
	Herdr herdr.Client
	Store *store.Store
	// ConfigDir holds herdr-agentisan.toml.
	ConfigDir string
	// LookupEnv and Stat are settings.Load's; nil means the real ones.
	LookupEnv func(string) (string, bool)
	Stat      func(string) (fs.FileInfo, error)
	// Idle is the poll's idle clock, for team status.
	Idle *team.IdleClock
	// Now is the clock rows are stamped and idle times read with; nil means
	// time.Now.
	Now func() time.Time
	// HandoffPoll overrides DefaultHandoffPoll.
	HandoffPoll time.Duration

	mu sync.Mutex
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the caller wraps with what it waited for
	case <-t.C:
		return nil
	}
}

// ignoreGone is err, unless it says the row is not live any more: another op
// ended it first, which is no failure of this one.
func ignoreGone(err error) error {
	if errors.Is(err, store.ErrNoWorker) {
		return nil
	}
	return err
}

// Selector picks the team an op acts on: Project when set, else the tabs
// team whose workspace holds CallerPane, else the only tabs team.
type Selector struct {
	Project string `json:"project,omitempty"`
	// CallerPane is the current id of the pane the command runs in, as
	// FindCallerPane confirmed it, or "": the op looks it up as is.
	CallerPane string `json:"caller_pane,omitempty"`
}

// FindCallerPane is the current id of the pane the command runs in, or ""
// when nothing confirms one: HERDR_PANE_ID counts only as report.ResolvePane
// confirms it, by a process in the command's own lineage, since herdr can
// reissue a closed workspace's ids to new panes. A failing herdr is ErrHerdr.
func FindCallerPane(ctx context.Context, h report.PaneFinder, pane report.Pane) (string, error) {
	if pane.PaneID == "" || pane.Lineage == nil {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, herdr.CallTimeout)
	defer cancel()
	id, err := report.ResolvePane(ctx, h, pane)
	switch {
	case errors.Is(err, report.ErrPaneUnresolved), errors.Is(err, report.ErrAmbiguousPane):
		return "", nil
	case err != nil:
		return "", herdrFailed("find the caller's pane", err)
	}
	return id, nil
}

// SelectorFor is the team a command means: project when set, without asking
// herdr; else the caller's pane, as FindCallerPane confirms it.
func SelectorFor(ctx context.Context, h report.PaneFinder, project string, pane report.Pane) (Selector, error) {
	if project != "" {
		return Selector{Project: project}, nil
	}
	caller, err := FindCallerPane(ctx, h, pane)
	return Selector{CallerPane: caller}, err
}

// layout is herdr's answers one op works from.
type layout struct {
	team.Layout
}

// readLayout lists everything an op needs from herdr.
func (s *Service) readLayout(ctx context.Context) (layout, error) {
	panes, err := s.Herdr.ListPanes(ctx)
	if err != nil {
		return layout{}, herdrFailed("list panes", err)
	}
	workspaces, err := s.Herdr.ListWorkspaces(ctx)
	if err != nil {
		return layout{}, herdrFailed("list workspaces", err)
	}
	tabs, err := s.Herdr.ListTabs(ctx)
	if err != nil {
		return layout{}, herdrFailed("list tabs", err)
	}
	agents, err := s.Herdr.ListAgents(ctx)
	if err != nil {
		return layout{}, herdrFailed("list agents", err)
	}
	return layout{team.Layout{Workspaces: workspaces, Panes: panes, Tabs: tabs, Agents: agents}}, nil
}

func herdrFailed(what string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrHerdr, what, err)
}

// pane is the pane with id, if herdr lists it.
func (l layout) pane(id string) (herdr.PaneInfo, bool) {
	i := slices.IndexFunc(l.Panes, func(p herdr.PaneInfo) bool { return p.PaneID == id })
	if i < 0 {
		return herdr.PaneInfo{}, false
	}
	return l.Panes[i], true
}

// tabLabel is the label of the tab with id; "" when herdr lists none.
func (l layout) tabLabel(id string) string {
	i := slices.IndexFunc(l.Tabs, func(t herdr.TabInfo) bool { return t.TabID == id })
	if i < 0 {
		return ""
	}
	return l.Tabs[i].Label
}

// teams reads every configured team, as the daemon's poll does, but afresh.
func (s *Service) teams() ([]settings.Team, error) {
	teams, err := settings.Teams(s.ConfigDir)
	if err != nil {
		return nil, configFailed(err)
	}
	return teams, nil
}

// configFailed is ErrConfig, keeping settings' own sentinel in the chain:
// settings.ErrBypassesAgentisan stays a D12 refusal of its own.
func configFailed(err error) error {
	return fmt.Errorf("%w: %w", ErrConfig, err)
}

// selectTeam picks the team sel names among teams. Only an explicit project
// may name the ◆ spaces project; the defaults look for a tabs team.
func selectTeam(sel Selector, teams []settings.Team, l layout) (settings.Team, error) {
	if sel.Project != "" {
		i := slices.IndexFunc(teams, func(t settings.Team) bool { return t.Project == sel.Project })
		if i < 0 {
			return settings.Team{}, fmt.Errorf("%w: %q; configured: %s", ErrUnknownProject, sel.Project, names(teams))
		}
		return teams[i], nil
	}
	var tabs []settings.Team
	for _, t := range teams {
		if t.Layout == settings.LayoutTabs {
			tabs = append(tabs, t)
		}
	}
	if p, ok := l.pane(sel.CallerPane); ok {
		for _, t := range tabs {
			if id, _ := tabsOf(t).Workspace(l.Workspaces); id == p.WorkspaceID {
				return t, nil
			}
		}
	}
	if len(tabs) == 1 {
		return tabs[0], nil
	}
	return settings.Team{}, fmt.Errorf("%w: pass --project: the caller's pane is in no team's workspace, and the tabs teams are %s", ErrBadRequest, names(tabs))
}

func names(teams []settings.Team) string {
	if len(teams) == 0 {
		return "none"
	}
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		out = append(out, t.Project)
	}
	return strings.Join(out, ", ")
}

func tabsOf(t settings.Team) team.Tabs {
	return team.Tabs{Project: t.Project, Short: t.Short, Color: t.Color}
}

// profile resolves the project's profile, repo and D12 checks included.
func (s *Service) profile(project string) (*settings.Profile, error) {
	res, err := settings.Load(settings.LoadOptions{ConfigDir: s.ConfigDir, Project: project, LookupEnv: s.LookupEnv, Stat: s.Stat})
	if err != nil {
		return nil, configFailed(err)
	}
	return res.Profile, nil
}

// groupOrder is the order groups are reported in: those order names first,
// in that order, then the rest by name.
func groupOrder(names, order []string) []string {
	rest := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return slices.Contains(order, n) })
	slices.Sort(rest)
	out := make([]string, 0, len(names))
	for _, n := range order {
		if slices.Contains(names, n) {
			out = append(out, n)
		}
	}
	return append(out, rest...)
}

// configured is the profile's worker types, by name: every group but one
// that only describes, which is no worker type (A36).
func configured(p *settings.Profile) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(p.Groups)) {
		if !p.Groups[name].DescribesOnly {
			out = append(out, name)
		}
	}
	return out
}

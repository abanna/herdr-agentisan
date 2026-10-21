package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// TeamOptions turn on the team poll (D3, D4, D5): on start and every poll
// after, the daemon rebuilds the team model from herdr and state.db, pushes
// `$team` to every group target and tabs team and the ctx display to every
// agent pane (A28), and caches every team's snapshot, which the snapshot op
// serves (A18, A24). `daemon run` always sets them.
type TeamOptions struct {
	// ConfigDir holds herdr-agentisan.toml (HERDR_PLUGIN_CONFIG_DIR). The
	// poll reads every project's team from it once, when it starts (A19): a
	// team added later is found after a daemon restart. The workforce ops
	// read it afresh each time (A23), and group descriptions alone are
	// read again whenever the file changes (A36).
	ConfigDir string
	// Events receive each status change between two polls (D10, A21); nil
	// emits none. `daemon run` builds them with SetupEvents.
	Events *telemetry.Events
	// GitHub is the header's GitHub collection (A29), for every project
	// the same file gives a github repo.
	GitHub GitHubOptions
	// procLocks, when set, stands in for /proc/locks in counting the test
	// slots (tests).
	procLocks string
}

// teamConfig is what the poll reads of the configuration: the project the ◆
// spaces belong to, with its colour, and every tabs team.
type teamConfig struct {
	spaces team.Spaces
	tabs   []team.Tabs
}

func teamConfigOf(teams []settings.Team) teamConfig {
	var c teamConfig
	for _, t := range teams {
		if t.Layout == settings.LayoutSpaces {
			c.spaces = team.Spaces{Project: t.Project, Color: t.Color}
			continue
		}
		c.tabs = append(c.tabs, team.Tabs{Project: t.Project, Short: t.Short, Color: t.Color})
	}
	return c
}

// resolver resolves the ◆ spaces and every tabs team.
func (c teamConfig) resolver() team.Resolvers {
	rs := make(team.Resolvers, 0, 1+len(c.tabs))
	rs = append(rs, c.spaces)
	for _, t := range c.tabs {
		rs = append(rs, t)
	}
	return rs
}

// teamPoll is what one team poll works with besides herdr and state.db. Only
// the poll's goroutine touches it.
type teamPoll struct {
	cfg teamConfig
	// warned holds each tabs team's ignored workspaces as last logged.
	warned map[string]string
	// stages times each pane's stage across polls (A18).
	stages *team.StageClock
	// idle times each pane's rest across polls, for team status (A23).
	idle  *team.IdleClock
	cache *snapshotCache
	// header is each project's GitHub state, which the collector keeps
	// (A29).
	header *headerCache
	// models are each agent pane's launch model (A39).
	models *launchModels
	// descriptions are each project's group descriptions, re-read when the
	// shared file changes (A36).
	descriptions *groupDescriptions
	// slots counts the test slots (A31).
	slots *slotCounter
	// bind keeps the workers rows bound to their panes (A35).
	bind *binder
}

// startTeam starts the team poll when o.Team is set, and beside it the
// header's GitHub collection and the agent view's sync (A28 addendum); the
// poll caches each snapshot in cache, times each pane's rest in idle, gives
// views each model, and sets ready once it has reconciled the workers rows
// (A35). The function it returns stops all three and waits for them, so
// once it returns nothing more is pushed, the view is cleared unless the
// server is gone, and no gh is left running.
func (o Options) startTeam(ctx context.Context, st *store.Store, ident Identity, cache *snapshotCache, idle *team.IdleClock, views *viewSync, ready *atomic.Bool) (stop func()) {
	if o.Team == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	header := newHeaderCache(o.Team.GitHub)
	var wg sync.WaitGroup
	wg.Go(func() { o.collectGitHub(ctx, header) })
	wg.Go(func() { o.pollTeam(ctx, st, ident, cache, header, idle, views, ready) })
	wg.Go(func() { o.syncView(ctx, views, ident) })
	return func() {
		cancel()
		wg.Wait()
	}
}

// pollTeam pushes `$team` and the ctx displays now and every poll until ctx
// ends or the daemon's herdr server goes. Nothing clears the tokens when it
// stops: every push carries team.TTL, so they expire within about 9 s, after
// a crash just as after a clean stop.
//
// The teams, with the ◆ spaces' project and its colour, are read once, here;
// only the group descriptions are read again, at a poll that finds the file
// changed (A36).
// A configuration that does not resolve is logged, the spaces fall back to
// the unnamed project, with no colour, and no tabs team is found: the
// spaces' `$team` does not depend on the project, and a typo in the file
// must not stop $team, the snapshot, focus and Back. `config resolve` names
// the error.
//
// Every call the poll makes proves, once connected and before it sends
// anything, that the socket still names the daemon's own server (A3): a
// read answered by the replacement, or a push landing on it, would be the
// next daemon's business. So the poll is a client of its own, whose Dialed
// hook checks the identity; a failed check stops the poll (ErrHerdrGone).
func (o Options) pollTeam(ctx context.Context, st *store.Store, ident Identity, cache *snapshotCache, header *headerCache, idle *team.IdleClock, views *viewSync, ready *atomic.Bool) {
	teams, err := settings.Teams(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Error().Err(err).Msg("herdr-agentisan.toml does not resolve; the ◆ spaces belong to the unnamed project, and no tabs team is found, until it is fixed and the daemon restarts")
		teams = nil
	}
	cfg := teamConfigOf(teams)
	tabs := make([]string, 0, len(cfg.tabs))
	for _, t := range cfg.tabs {
		tabs = append(tabs, t.Project)
	}
	o.Logger.Info().Str("project", cfg.spaces.Project).Str("color", cfg.spaces.Color).Strs("tabs_teams", tabs).Msg("team poll running")
	tp := teamPoll{
		cfg: cfg, warned: map[string]string{}, stages: &team.StageClock{}, idle: idle, cache: cache, header: header,
		models: newLaunchModels(), descriptions: newGroupDescriptions(o.Team.ConfigDir),
		slots: o.newSlotCounter(), bind: &binder{ready: ready},
	}
	onServer := o.onServer(ident)
	c := herdr.Client{SocketPath: o.HerdrSocket, Dialed: onServer}
	tick := time.NewTicker(o.Poll)
	defer tick.Stop()
	// last is the last model a poll built: the baseline status events are
	// diffed against. Nil until a poll lists both panes and workspaces, so
	// the first poll after a start emits nothing, and a poll that fails a
	// list never becomes the baseline (A21).
	var last *team.Model
	for {
		// Checked here as well as on every dial: a socket that is gone fails
		// the dial itself, before Dialed runs, and a failed dial is only
		// ErrUnavailable, which the poll logs and retries. This stops it at
		// once instead.
		err := onServer()
		var built *team.Model
		tp.descriptions.refresh(o.Logger)
		if err == nil {
			built, err = o.pushTeam(ctx, c, st, tp)
		}
		if err != nil {
			o.Logger.Info().Err(err).Msg("team poll stops: the daemon's herdr server is gone or was replaced")
			return
		}
		if built != nil {
			if last != nil && o.Team.Events != nil {
				o.Team.emitTransitions(ctx, *last, *built)
			}
			last = built
			views.built(*built)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pushTeam is one poll: it reads state.db's workers rows; lists panes,
// workspaces, with a tabs team configured tabs, and agents; reconciles the
// rows with the panes (A35), unless the rows or the agents could not be read;
// builds the model with the rows; caches every team's snapshot, each with its
// project's GitHub header (A29), the test slots, counted once for the poll
// (A31), and group descriptions (A36), and the workspaces each team holds
// (A24); pushes each group target and tabs team its `$team`; and pushes every
// agent pane its ctx display. A failed pane, workspace or tab list costs the
// poll; a failed agent list costs only the snapshots, which `$team` does not
// need, so the last ones built with names stay served, teams and index alike,
// and the ctx display of a pane herdr knows only by name; a failed push is
// logged and the others go on. It returns ErrHerdrGone, and pushes nothing
// more, once a call finds the socket no longer names the daemon's server.
// Each row carries its pane's launch model, which herdr is asked for only
// when the pane changed (A39).
// The model it returns is nil when a pane, workspace or tab list failed and
// no model was built; agent names change neither which panes it holds nor
// their statuses, so a failed agent list still gives the status events their
// model.
func (o Options) pushTeam(ctx context.Context, c herdr.Client, st *store.Store, tp teamPoll) (*team.Model, error) {
	// The rows are read before the panes: the workforce commands write a
	// row only once its pane exists, so every row read is one whose pane,
	// unless it closed, the list after holds, and the reconcile never takes
	// a pane opened between the two reads for one that is gone (A35).
	rows, rowsErr := st.Workers(ctx)
	if rowsErr != nil {
		// The counts need no logical id: push them, provisional ids and all.
		o.warnUnlessStopping(ctx, rowsErr, "team poll: read workers")
		rows = nil
	}
	panes, err := c.ListPanes(ctx)
	if err != nil {
		return nil, o.teamCallFailed(ctx, err, "team poll: list panes")
	}
	workspaces, err := c.ListWorkspaces(ctx)
	if err != nil {
		return nil, o.teamCallFailed(ctx, err, "team poll: list workspaces")
	}
	var tabs []herdr.TabInfo
	if len(tp.cfg.tabs) > 0 {
		if tabs, err = c.ListTabs(ctx); err != nil {
			return nil, o.teamCallFailed(ctx, err, "team poll: list tabs")
		}
	}
	o.warnIgnored(tp.cfg.tabs, workspaces, tp.warned)
	agents, agentsErr := c.ListAgents(ctx)
	if agentsErr != nil {
		if err := o.teamCallFailed(ctx, agentsErr, "team poll: list agents; the snapshot keeps its last names"); err != nil {
			return nil, err
		}
	}
	l := team.Layout{Workspaces: workspaces, Panes: panes, Agents: agents, Tabs: tabs}
	pp := paneProcesses{} // one process_info ask per pane this poll
	// A candidate pane can be the one herdr names a worker by, so a failed
	// agent list proves and loses nothing this poll.
	if rowsErr == nil && agentsErr == nil {
		if rows, err = o.reconcile(ctx, c, pp, st, tp.bind, l, rows); err != nil {
			return nil, err
		}
	}
	m := team.Build(l, tp.cfg.resolver(), rows)
	now := o.Now()
	// A pane's rest is pane.list's status, so a failed agent list does not
	// stop the idle clock.
	tp.idle.Observe(m, now)
	if agentsErr == nil {
		tp.stages.Observe(m, now)
		models, err := tp.models.refresh(ctx, c, pp, agentPanes(m, panes), o.Hooks.StartTime, now, o.Logger)
		if err != nil {
			return nil, err
		}
		// The slots are the machine's: one count for every team, so a list
		// that changes mid-poll cannot give two teams two counts.
		slots := o.countSlots(tp.slots)
		fill := func(project string, s *snapshot.Snapshot) {
			tp.header.fill(project, s, now)
			setSlots(s, slots)
			team.FillModels(s, models)
			team.Describe(s, tp.descriptions.of(project))
		}
		if err := tp.cache.store(m, tp.cfg.spaces.Project, now, tp.stages, tp.idle, fill); err != nil {
			o.Logger.Error().Err(err).Msg("team poll: cache the snapshots; a team too large to serve keeps its last one")
		}
	}
	for _, t := range team.Tokens(m) {
		// Once ctx ends, each push fails before it dials.
		err := c.ReportWorkspaceMetadata(ctx, herdr.WorkspaceMetadata{
			WorkspaceID: t.Target.WorkspaceID,
			Source:      report.Source,
			Tokens:      map[string]string{team.Key: t.Value},
			TTLMillis:   uint64(team.TTL / time.Millisecond),
		})
		if errors.Is(err, ErrHerdrGone) {
			return nil, fmt.Errorf("push $team to %s: %w", t.Target.WorkspaceID, err)
		}
		if err != nil && ctx.Err() == nil {
			o.Logger.Warn().Err(err).Str("workspace", t.Target.WorkspaceID).Msg("push $team")
		}
	}
	if err := o.pushCtx(ctx, c, m, l); err != nil {
		return nil, err
	}
	return &m, nil
}

// pushCtx pushes every agent pane in l its ctx display (A28) and, when a
// team in m places it, its place in the team, which the agent view sorts by
// (A28 addendum), with `$team`'s source and TTL. A refused push, such as to
// a pane closed since the list, is logged and the others go on; it returns
// ErrHerdrGone, and pushes nothing more, once a call finds the socket no
// longer names the daemon's server.
func (o Options) pushCtx(ctx context.Context, c herdr.Client, m team.Model, l team.Layout) error {
	for _, pt := range team.SidebarTokens(m, l) {
		// Once ctx ends, each push fails before it dials.
		err := c.ReportPaneMetadata(ctx, herdr.PaneMetadata{
			PaneID:    pt.PaneID,
			Source:    report.Source,
			Tokens:    pt.Tokens,
			TTLMillis: uint64(team.TTL / time.Millisecond),
		})
		if errors.Is(err, ErrHerdrGone) {
			return fmt.Errorf("push the ctx display to %s: %w", pt.PaneID, err)
		}
		if err != nil && ctx.Err() == nil {
			o.Logger.Warn().Err(err).Str("pane", pt.PaneID).Msg("push the ctx display")
		}
	}
	return nil
}

// warnIgnored logs a tabs team's other workspaces labelled with its name,
// which are not part of the model, when they change rather than every poll.
func (o Options) warnIgnored(tabs []team.Tabs, workspaces []herdr.WorkspaceInfo, warned map[string]string) {
	for _, t := range tabs {
		_, ignored := t.Workspace(workspaces)
		key := strings.Join(ignored, "\x00")
		if key == warned[t.Project] {
			continue
		}
		warned[t.Project] = key
		if key != "" {
			o.Logger.Warn().Str("project", t.Project).Strs("ignored", ignored).
				Msg("team poll: more than one workspace has the team's label; only the first in the sidebar is the team")
		}
	}
}

// teamCallFailed is ErrHerdrGone for a call that found another server, and
// otherwise logs err and lets the next poll try again.
func (o Options) teamCallFailed(ctx context.Context, err error, msg string) error {
	if errors.Is(err, ErrHerdrGone) {
		return fmt.Errorf("%s: %w", msg, err)
	}
	o.warnUnlessStopping(ctx, err, msg)
	return nil
}

// warnUnlessStopping logs err unless the daemon is stopping, which ends every
// call in flight.
func (o Options) warnUnlessStopping(ctx context.Context, err error, msg string) {
	if ctx.Err() == nil {
		o.Logger.Warn().Err(err).Msg(msg)
	}
}

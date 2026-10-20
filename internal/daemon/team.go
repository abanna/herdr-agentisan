package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// TeamOptions turn on the team poll (D3, D4, D5): on start and every poll
// after, the daemon rebuilds the team model from herdr and state.db, pushes
// `$team` to every group target and tabs team, and caches the snapshot the
// snapshot op serves (A18). `daemon run` always sets them.
type TeamOptions struct {
	// ConfigDir holds herdr-agentisan.toml (HERDR_PLUGIN_CONFIG_DIR). The
	// poll reads every project's team from it once, when it starts (A19): a
	// team added later is found after a daemon restart.
	ConfigDir string
	// Events receive each status change between two polls (D10, A21); nil
	// emits none. `daemon run` builds them with SetupEvents.
	Events *telemetry.Events
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
	cache  *snapshotCache
}

// startTeam starts the team poll when o.Team is set; it caches each poll's
// snapshot in cache. The function it returns stops the poll and waits for
// it, so once it returns nothing more is pushed.
func (o Options) startTeam(ctx context.Context, st *store.Store, ident Identity, cache *snapshotCache) (stop func()) {
	if o.Team == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { o.pollTeam(ctx, st, ident, cache) })
	return func() {
		cancel()
		wg.Wait()
	}
}

// pollTeam pushes `$team` now and every poll until ctx ends or the daemon's
// herdr server goes. Nothing clears the tokens when it stops: every push
// carries team.TTL, so they expire within about 9 s, after a crash just as
// after a clean stop.
//
// The teams, with the ◆ spaces' project and its colour, are read once, here.
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
func (o Options) pollTeam(ctx context.Context, st *store.Store, ident Identity, cache *snapshotCache) {
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
	tp := teamPoll{cfg: cfg, warned: map[string]string{}, stages: &team.StageClock{}, cache: cache}
	onServer := func() error {
		if !o.sameServer(ident) {
			return fmt.Errorf("%w: the socket now names another server, or none", ErrHerdrGone)
		}
		return nil
	}
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
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pushTeam is one poll: it lists panes, workspaces, with a tabs team
// configured tabs, and agents; builds the model with state.db's workers
// rows; caches the snapshot of it; and pushes each group target and tabs
// team its `$team`. A failed pane, workspace or tab list costs the poll; a
// failed agent list costs only the snapshot, which `$team` does not need, so
// the last one built with names stays served; a failed push is logged and
// the others go on. It returns ErrHerdrGone, and pushes nothing more, once a
// call finds the socket no longer names the daemon's server.
// The model it returns is nil when a pane, workspace or tab list failed and
// no model was built; agent names change neither which panes it holds nor
// their statuses, so a failed agent list still gives the status events their
// model.
func (o Options) pushTeam(ctx context.Context, c herdr.Client, st *store.Store, tp teamPoll) (*team.Model, error) {
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
	rows, err := st.Workers(ctx)
	if err != nil {
		// The counts need no logical id: push them, provisional ids and all.
		o.warnUnlessStopping(ctx, err, "team poll: read workers")
		rows = nil
	}
	m := team.Build(team.Layout{Workspaces: workspaces, Panes: panes, Agents: agents, Tabs: tabs}, tp.cfg.resolver(), rows)
	if agentsErr == nil {
		now := o.Now()
		tp.stages.Observe(m, now)
		snap := team.Snapshot(m, tp.cfg.spaces.Project, now, tp.stages)
		if err := tp.cache.store(snap); err != nil {
			o.Logger.Error().Err(err).Msg("team poll: cache the snapshot; the last one stays served")
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
	return &m, nil
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

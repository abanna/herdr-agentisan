package daemon

import (
	"context"
	"errors"
	"fmt"
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
// after, the daemon rebuilds the team model from herdr and state.db and
// pushes `$team` to every group target. `daemon run` always sets them.
type TeamOptions struct {
	// ConfigDir holds herdr-agentisan.toml (HERDR_PLUGIN_CONFIG_DIR). The
	// poll reads [spaces] project from it once, when it starts.
	ConfigDir string
	// Events receive each status change between two polls (D10, A21); nil
	// emits none. `daemon run` builds them with SetupEvents.
	Events *telemetry.Events
}

// startTeam starts the team poll when o.Team is set. The function it returns
// stops the poll and waits for it, so once it returns nothing more is pushed.
func (o Options) startTeam(ctx context.Context, st *store.Store, ident Identity) (stop func()) {
	if o.Team == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { o.pollTeam(ctx, st, ident) })
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
// The project the ◆ spaces belong to is read once, here. A configuration
// that does not resolve is logged and the spaces fall back to the unnamed
// project: `$team` does not depend on the project, and a typo in the file
// must not stop $team, focus and Back. `config resolve` names the error.
//
// Every call the poll makes proves, once connected and before it sends
// anything, that the socket still names the daemon's own server (A3): a
// read answered by the replacement, or a push landing on it, would be the
// next daemon's business. So the poll is a client of its own, whose Dialed
// hook checks the identity; a failed check stops the poll (ErrHerdrGone).
func (o Options) pollTeam(ctx context.Context, st *store.Store, ident Identity) {
	project, err := settings.SpacesProject(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Error().Err(err).Msg("herdr-agentisan.toml does not resolve; the ◆ spaces belong to the unnamed project until it is fixed and the daemon restarts")
	}
	o.Logger.Info().Str("project", project).Msg("team poll running")
	resolver := team.Spaces{Project: project}
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
			built, err = o.pushTeam(ctx, c, st, resolver)
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

// pushTeam is one poll: it lists panes and workspaces, builds the model with
// state.db's workers rows, and pushes each group target its `$team`. A
// failed list costs the poll; a failed push is logged and the others go on.
// It returns ErrHerdrGone, and pushes nothing more, once a call finds the
// socket no longer names the daemon's server. The model it returns is nil
// when a list failed and no model was built.
func (o Options) pushTeam(ctx context.Context, c herdr.Client, st *store.Store, r team.Resolver) (*team.Model, error) {
	panes, err := c.ListPanes(ctx)
	if err != nil {
		return nil, o.teamCallFailed(ctx, err, "team poll: list panes")
	}
	workspaces, err := c.ListWorkspaces(ctx)
	if err != nil {
		return nil, o.teamCallFailed(ctx, err, "team poll: list workspaces")
	}
	rows, err := st.Workers(ctx)
	if err != nil {
		// The counts need no logical id: push them, provisional ids and all.
		o.warnUnlessStopping(ctx, err, "team poll: read workers")
		rows = nil
	}
	model := team.Build(team.Layout{Workspaces: workspaces, Panes: panes}, r, rows)
	for _, t := range team.Tokens(model) {
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
	return &model, nil
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

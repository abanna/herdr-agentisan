package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// binder is what the team poll's reconcile keeps across polls (ADR-001 A35).
type binder struct {
	// ready is set once a reconcile has run since the daemon started. The
	// workforce ops refuse not_ready until then: before it, a worker whose
	// terminal a live handoff renewed is not yet re-keyed, and would read
	// as a pane no row binds.
	ready *atomic.Bool
	// warnedNoTerminal is set once a pane listed with no terminal id has
	// been logged; herdr 0.9.3 lists every pane with one.
	warnedNoTerminal bool
}

// reconcile keeps every live workers row bound to its own pane, from one
// poll's layout l and rows read before it (A35). It rebinds a row whose pane
// moved, reads a bound pane's shell for a row without one, re-keys a row
// whose terminal herdr renewed to the candidate pane holding the same shell
// (pid and start time), and marks lost a row no pane can be shown to hold.
// A pane herdr cannot describe leaves its row unverified, for the next poll
// to try again. Each write is a compare-and-set on the row as read, so a
// row the workforce commands changed meanwhile is left to the next poll.
//
// It returns the rows to build the model with, read again when it wrote any,
// and stops only on ErrHerdrGone. The shells it reads are asked through pp,
// the poll's process_info replies.
func (o Options) reconcile(ctx context.Context, c herdr.Client, pp paneProcesses, st *store.Store, b *binder, l team.Layout, rows []store.Worker) ([]store.Worker, error) {
	if !b.warnedNoTerminal {
		for _, p := range l.Panes {
			if p.TerminalID == "" {
				b.warnedNoTerminal = true
				o.Logger.Warn().Str("pane", p.PaneID).Msg("team poll: herdr lists a pane with no terminal id; no workers row can bind it")
				break
			}
		}
	}
	wrote := false
	for _, ck := range team.Reconcile(l, rows) {
		w, err := o.reconcileRow(ctx, c, pp, st, ck)
		if err != nil {
			return nil, err
		}
		wrote = wrote || w
	}
	b.ready.Store(true)
	if !wrote {
		return rows, nil
	}
	fresh, err := st.Workers(ctx)
	if err != nil {
		o.warnUnlessStopping(ctx, err, "team poll: read workers again after the reconcile")
		return rows, nil
	}
	return fresh, nil
}

// reconcileRow does ck's work, and reports whether it wrote the row.
func (o Options) reconcileRow(ctx context.Context, c herdr.Client, pp paneProcesses, st *store.Store, ck team.Check) (bool, error) {
	r := ck.Row
	log := o.Logger.With().Str("worker", r.LogicalID).Int64("generation", r.Generation).Logger()
	wrote := false
	if ck.Moved() && o.wrote(ctx, log, st.RebindWorker(ctx, r, ck.Pane), "rebind") {
		log.Info().Str("from", r.PaneID).Str("pane", ck.Pane).Msg("team poll: worker rebound; its pane moved")
		wrote = true
	}
	if ck.NeedsShell() && o.Hooks.StartTime != nil {
		sh, err := o.shellOf(ctx, c, pp, log, ck.Pane)
		if err != nil {
			return wrote, err
		}
		if sh.pid != 0 && o.wrote(ctx, log, st.SetWorkerShell(ctx, r, sh.pid, sh.start), "record the shell of") {
			wrote = true
		}
	}
	if ck.Pane != "" {
		return wrote, nil
	}
	unverified := false
	for _, p := range ck.Candidates {
		sh, err := o.shellOf(ctx, c, pp, log, p.PaneID)
		if err != nil {
			return wrote, err
		}
		switch {
		case !sh.known:
			unverified = true
		case sh.pid == r.ShellPID && sh.start == r.ShellStart:
			if o.wrote(ctx, log, st.RekeyWorker(ctx, r, p.PaneID, p.TerminalID), "re-key") {
				log.Info().Str("from", r.PaneID).Str("pane", p.PaneID).Str("terminal", p.TerminalID).
					Msg("team poll: worker re-keyed; herdr renewed its terminal, and this pane holds its shell")
				wrote = true
			}
			return wrote, nil
		default:
			log.Warn().Str("pane", p.PaneID).Msg("team poll: a pane that may hold the worker runs another shell")
		}
	}
	if unverified || r.HandoffPending {
		return wrote, nil
	}
	if o.wrote(ctx, log, st.MarkWorkerLost(ctx, r), "mark lost") {
		log.Warn().Str("pane", r.PaneID).
			Msg("team poll: worker lost; no pane is proven to hold it, so it binds none until the boss retires it or spawns its id again")
		wrote = true
	}
	return wrote, nil
}

// wrote reports whether a poll write landed. A row changed since it was
// read is the next poll's; a failed write is logged and tried again then.
func (o Options) wrote(ctx context.Context, log zerolog.Logger, err error, what string) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrWorkerChanged):
		log.Debug().Err(err).Msg("team poll: the row changed since it was read")
	case ctx.Err() == nil:
		log.Warn().Err(err).Msgf("team poll: %s the worker", what)
	}
	return false
}

// shell is a pane's shell as the poll reads it.
type shell struct {
	pid   uint32
	start uint64
	// known is false when herdr could not describe the pane, or knew no
	// shell pid yet: whose shell it holds is unknown this poll. A shell
	// whose start time cannot be read has exited, so it is known, and is no
	// row's: its pid is 0.
	known bool
}

// shellOf reads pane's shell: its pid from herdr, through the poll's replies
// pp, its start time from the process table. It fails only with
// ErrHerdrGone.
func (o Options) shellOf(ctx context.Context, c herdr.Client, pp paneProcesses, log zerolog.Logger, pane string) (shell, error) {
	info, err := pp.ask(ctx, c, pane)
	if errors.Is(err, ErrHerdrGone) {
		return shell{}, fmt.Errorf("team poll: read the shell of %s: %w", pane, err)
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Warn().Err(err).Str("pane", pane).Msg("team poll: read a pane's shell; the worker stays unverified until the next poll")
		}
		return shell{}, nil
	}
	if info.ShellPID == 0 || o.Hooks.StartTime == nil {
		return shell{}, nil
	}
	start, err := o.Hooks.StartTime(int(info.ShellPID))
	if err != nil {
		return shell{known: true}, nil //nolint:nilerr // an exited shell is no row's, which is an answer
	}
	return shell{pid: info.ShellPID, start: start, known: true}, nil
}

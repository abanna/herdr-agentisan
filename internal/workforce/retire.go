package workforce

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// RetireRequest is worker retire: Target is a logical id or a pane id. It
// needs no team: both are unique on the server.
type RetireRequest struct {
	Target       string `json:"target"`
	AfterHandoff bool   `json:"after_handoff,omitempty"`
}

// Retire outcomes.
const (
	// OutcomeRetired is a worker whose pane this op closed.
	OutcomeRetired = "retired"
	// OutcomeGone is a worker whose pane was gone before it could be
	// closed.
	OutcomeGone = "gone"
	// OutcomeUnbound is a live row bound to no pane, lost or with its
	// terminal listed nowhere (A35): retired, with nothing closed. A row
	// with a shell that a pane herdr lists is proven to run is refused
	// instead (unkeyed, NERD-5534).
	OutcomeUnbound = "unbound"
	// OutcomeAlreadyRetired is a logical id retired before.
	OutcomeAlreadyRetired = "already_retired"
)

// RetireResult is worker retire's answer.
type RetireResult struct {
	// ID is the worker's logical id; "" for a pane started by hand, which
	// has none.
	ID     string `json:"id,omitempty"`
	PaneID string `json:"pane_id,omitempty"`
	// Changed is false when the worker was retired already.
	Changed bool   `json:"changed"`
	Outcome string `json:"outcome"`
	// HandoffFailed is the logical id of the worker whose pending handoff
	// this retire failed, its target being the replacement's pane: the
	// rotation is abandoned, and the worker stays on its own pane with its
	// inbox released (A33 part 2a).
	HandoffFailed string `json:"handoff_failed,omitempty"`
	// MuteCleared is set when retiring the row ended an active notice mute.
	MuteCleared bool `json:"mute_cleared,omitempty"`
}

// target is a resolved retire target: a pane, the terminal it showed, and
// the live row bound to it, if any. A live row bound to no pane has no pane
// id. unlisted is a pane id herdr lists nowhere that a pending handoff names
// as its replacement.
type target struct {
	id, paneID, terminal string
	row                  *store.Worker
	unlisted             bool
}

// Retire closes a worker's pane and marks its row retired. The target is a
// logical id or a pane id. A live row's id names the pane herdr lists with
// the row's terminal, since the row decides who a worker is (A35); a row
// bound to no pane is retired with nothing closed. A worker's tab label
// names its agent pane only for an id no live row has. Whichever names the
// pane, the row bound to its terminal is the one retired, and a pane in a
// team's <short>-boss tab is refused (ErrNotAWorker), as is AfterHandoff
// for a debugger's row bound to a pane, which can write no handoff
// (ErrBadRequest). With AfterHandoff it first waits, without the workforce
// lock, until the pane with the target's terminal carries a non-empty
// handoff token, polling every HandoffPoll; a terminal herdr stops listing
// while it waits is the worker gone, and ctx ending first is
// ErrHandoffTimeout, with the pane left open. The pane is closed only once pane.get shows it still has that
// terminal. Retiring what is retired already answers Changed false. Under
// the workforce lock, finish keeps A33 part 2a's rule: no row is ended while
// its handoff is pending, and retiring a pending handoff's replacement fails
// that handoff. Nor is a worker a herdr live handoff left running, before
// the team poll re-keyed its row, ended or closed as gone, unbound or a
// pane no row binds (ErrPaneChanged, NERD-5534); a row whose pane retire
// did not close is ended only while it binds the terminal retire read, and
// one whose proven pane it closed is retired by logical id and generation.
func (s *Service) Retire(ctx context.Context, req RetireRequest) (RetireResult, error) {
	if strings.TrimSpace(req.Target) == "" {
		return RetireResult{}, fmt.Errorf("%w: retire needs a logical id or a pane id", ErrBadRequest)
	}
	tg, done, err := s.resolveTarget(ctx, req.Target)
	if err != nil || done != nil {
		return deref(done), err
	}
	if req.AfterHandoff && tg.row != nil && tg.paneID != "" && tg.row.Group == settings.DebuggerType {
		return RetireResult{}, fmt.Errorf("%w: %s is a debugger, which writes no handoff (A32); retire it without --after-handoff", ErrBadRequest, tg.id)
	}
	var gone bool
	if req.AfterHandoff && tg.paneID != "" && !tg.unlisted {
		pane, g, err := s.waitHandoff(ctx, tg)
		if err != nil {
			return RetireResult{}, err
		}
		if !g {
			tg.paneID = pane
		}
		gone = g
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finish(ctx, tg, gone)
}

// finish retires tg under the workforce lock, under which a handoff begins,
// so the handoffs it reads afresh hold until it returns (A33 part 2a). A row
// whose handoff is pending is never ended: ErrHandoffPending, with nothing
// closed; nor is one rebound since retire read it (unmoved). A pane that is a
// pending handoff's replacement fails that handoff first, then closes, so a
// close that errors leaves an ordinary pane for the retry and never an inbox
// held. gone is the pane gone while retire waited for its handoff.
func (s *Service) finish(ctx context.Context, tg target, gone bool) (RetireResult, error) {
	h, replaced, err := s.handoffOf(ctx, tg)
	if err != nil {
		return RetireResult{}, err
	}
	if err := s.unmoved(ctx, tg); err != nil {
		return RetireResult{}, err
	}
	if tg.unlisted && !replaced {
		return RetireResult{}, fmt.Errorf("%w: %q is no worker's logical id and no pane herdr lists", ErrUnknownWorker, tg.paneID)
	}
	if replaced {
		if err := s.Store.FailHandoff(ctx, h.ID, s.now()); err != nil {
			return RetireResult{}, fmt.Errorf("fail %s's handoff to %s: %w", h.LogicalID, h.NewPane, err)
		}
		s.handoffStep(ctx, HandoffStep{ID: h.LogicalID, Project: h.Project, Group: h.Group, PaneID: tg.paneID, OldPane: h.OldPane, Step: StepFailed})
	}
	// The row's mute is held while it ends, and cleared only once it has: a
	// poll that sees the closed pane tells its departure with the mute not
	// silencing it, and a retire that fails leaves the mute (NERD-5730).
	if tg.row != nil && s.HoldWorkerMute != nil {
		defer s.HoldWorkerMute(*tg.row)()
	}
	res, err := s.retireTarget(ctx, tg, gone, replaced)
	if err != nil {
		return RetireResult{}, err
	}
	if replaced {
		res.HandoffFailed = h.LogicalID
	}
	if tg.row != nil && s.ClearWorkerMute != nil {
		res.MuteCleared = s.ClearWorkerMute(*tg.row)
	}
	return res, nil
}

// retireTarget closes tg's pane, once proven, and ends its row: retired,
// or gone for a pane that went first. Neither a row whose terminal herdr
// lists nowhere nor a pane no row binds is ended or closed while a herdr live
// handoff the team poll has not re-keyed may have left the worker running
// there (unkeyed). A pending handoff's replacement, which retire has just
// failed the handoff of, is matched before that check, so it is closed.
func (s *Service) retireTarget(ctx context.Context, tg target, gone, replaced bool) (RetireResult, error) {
	switch {
	case tg.paneID == "":
		if err := s.unkeyed(ctx, tg.row.LogicalID, ""); err != nil {
			return RetireResult{}, err
		}
		if err := s.endRow(ctx, tg.row, store.StatusRetired); err != nil {
			return RetireResult{}, err
		}
		return RetireResult{ID: tg.id, Changed: true, Outcome: OutcomeUnbound}, nil
	case tg.unlisted || gone:
		return s.markGone(ctx, tg)
	case tg.row == nil && !replaced:
		if err := s.unkeyed(ctx, "", tg.paneID); err != nil {
			return RetireResult{}, err
		}
	}
	gone, err := s.closeProven(ctx, tg)
	if err != nil {
		return RetireResult{}, err
	}
	if gone {
		return s.markGone(ctx, tg)
	}
	if err := s.endClosed(ctx, tg.row); err != nil {
		return RetireResult{}, err
	}
	return RetireResult{ID: tg.id, PaneID: tg.paneID, Changed: true, Outcome: OutcomeRetired}, nil
}

// handoffOf reads the pending handoffs afresh and applies A33 part 2a's
// rule to tg. A row with one pending is refused (ErrHandoffPending). A pane
// no row binds may be a pending handoff's replacement, which it returns, by
// replacementOf's match, the ack's.
func (s *Service) handoffOf(ctx context.Context, tg target) (store.Handoff, bool, error) {
	pending, err := s.Store.PendingHandoffs(ctx)
	if err != nil {
		return store.Handoff{}, false, fmt.Errorf("read pending handoffs: %w", err)
	}
	if tg.row != nil {
		return store.Handoff{}, false, heldBy(pending, tg.row.LogicalID)
	}
	return replacementOf(pending, tg.paneID, tg.terminal, func(terminal string) (bool, error) {
		panes, err := s.Herdr.ListPanes(ctx)
		if err != nil {
			return false, herdrFailed("list panes", err)
		}
		_, listed := byTerminal(panes, terminal)
		return listed, nil
	})
}

// unmoved re-reads tg's row under the workforce lock. A row rebound since
// retire read it, while it waited without the lock, is another session's
// now: a handoff completed meanwhile, or the team poll's re-key after a
// herdr live handoff renewed its terminal (A35). Nothing of it is closed or
// ended (ErrPaneChanged), and a retry reads it afresh. A row retired
// meanwhile is left to endRow, which ignores it. A pane no row bound when
// retire read it is read again too (bound): a live row may bind its terminal
// now.
func (s *Service) unmoved(ctx context.Context, tg target) error {
	row := tg.row
	if row == nil {
		return s.bound(ctx, tg)
	}
	now, err := s.Store.WorkerRow(ctx, row.LogicalID)
	if errors.Is(err, store.ErrNoWorker) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read worker %s: %w", row.LogicalID, err)
	}
	if now.RetiredAt.IsZero() && (now.Generation != row.Generation || now.TerminalID != row.TerminalID) {
		return fmt.Errorf("%w: %s moved to %s since retire read it; nothing was closed", ErrPaneChanged, row.LogicalID, now.PaneID)
	}
	return nil
}

// bound refuses a pane no row bound when retire read it, whose terminal a
// live row binds under the workforce lock (NERD-5534): the team poll
// re-keyed a worker onto it after a herdr live handoff, while retire waited
// for its handoff or for the lock. The pane is that worker's now, so it is
// refused as the worker would be: ErrHandoffPending while its handoff is
// pending, else ErrPaneChanged, with nothing closed or ended, and a retry
// retires it as that worker.
func (s *Service) bound(ctx context.Context, tg target) error {
	if tg.terminal == "" {
		return nil
	}
	rows, err := s.Store.Workers(ctx)
	if err != nil {
		return fmt.Errorf("read workers: %w", err)
	}
	id := team.Bindings(rows)[tg.terminal]
	if id == "" {
		return nil
	}
	pending, err := s.Store.PendingHandoffs(ctx)
	if err != nil {
		return fmt.Errorf("read pending handoffs: %w", err)
	}
	if err := heldBy(pending, id); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is bound to %s since retire read it; nothing was closed or ended, and a retry retires it as %s", ErrPaneChanged, tg.paneID, id, id)
}

// heldBy is ErrHandoffPending, naming the replacement, while id has a
// handoff pending.
func heldBy(pending []store.Handoff, id string) error {
	i := slices.IndexFunc(pending, func(h store.Handoff) bool { return h.LogicalID == id })
	if i < 0 {
		return nil
	}
	next := pending[i].NewPane
	return fmt.Errorf("%w: %s is being replaced by %s and stays until that handoff completes or fails; worker retire %s abandons it", ErrHandoffPending, id, next, next)
}

func deref(r *RetireResult) RetireResult {
	if r == nil {
		return RetireResult{}
	}
	return *r
}

// closeProven closes tg's pane once pane.get shows it still has tg's
// terminal. gone is true when herdr lists that terminal nowhere any more. A
// pane that now shows another terminal while tg's is listed elsewhere, one
// that moved since it was read, closes nothing: ErrPaneChanged.
func (s *Service) closeProven(ctx context.Context, tg target) (gone bool, err error) {
	p, err := s.Herdr.GetPane(ctx, tg.paneID)
	switch {
	case err == nil && p.TerminalID == tg.terminal:
		err = s.Herdr.ClosePane(ctx, tg.paneID)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, herdr.ErrPaneNotFound) {
			return false, herdrFailed("close "+tg.paneID, err)
		}
	case err != nil && !errors.Is(err, herdr.ErrPaneNotFound):
		return false, herdrFailed("get "+tg.paneID, err)
	}
	panes, err := s.Herdr.ListPanes(ctx)
	if err != nil {
		return false, herdrFailed("list panes", err)
	}
	if _, listed := byTerminal(panes, tg.terminal); listed {
		return false, fmt.Errorf("%w: %s no longer shows terminal %s, which herdr lists elsewhere; nothing was closed", ErrPaneChanged, tg.paneID, tg.terminal)
	}
	return true, nil
}

// byTerminal is the pane herdr lists with terminal, if any.
func byTerminal(panes []herdr.PaneInfo, terminal string) (herdr.PaneInfo, bool) {
	i := slices.IndexFunc(panes, func(p herdr.PaneInfo) bool { return p.TerminalID == terminal })
	if i < 0 {
		return herdr.PaneInfo{}, false
	}
	return panes[i], true
}

// endRow ends row, if there is one, with status, while it still binds the
// terminal retire read it with: a compare-and-set, as the team poll's writes
// are (A35). A row another op ended or rebound at another generation first
// is left as it is; one the poll re-keyed since, after a herdr live handoff,
// is left live (ErrPaneChanged).
func (s *Service) endRow(ctx context.Context, row *store.Worker, status string) error {
	if row == nil {
		return nil
	}
	err := ignoreGone(s.Store.EndWorker(ctx, *row, status, s.now()))
	if errors.Is(err, store.ErrHandoffPending) {
		return fmt.Errorf("%w: %w", ErrHandoffPending, err)
	}
	if errors.Is(err, store.ErrWorkerChanged) {
		return fmt.Errorf("%w: %s was rebound to another terminal since retire read it, as the team poll's re-key after a herdr live handoff does; its row is left live: %w", ErrPaneChanged, row.LogicalID, err)
	}
	if err != nil {
		return fmt.Errorf("record %s %s: %w", row.LogicalID, status, err)
	}
	return nil
}

// endClosed retires row, if there is one, once retire has closed the pane
// pane.get proved showed its terminal. The pane is closed, so the worker
// went with it whatever the row binds now: a re-key landing between the
// proof and this write (a herdr live handoff in those milliseconds, which
// A35 accepts) still retires it, by logical id and generation, rather than
// leave a row bound to a closed pane for the poll to mark lost. A row retired
// or bound at another generation meanwhile is left as it is.
func (s *Service) endClosed(ctx context.Context, row *store.Worker) error {
	if row == nil {
		return nil
	}
	err := ignoreGone(s.Store.SetWorkerStatus(ctx, *row, store.StatusRetired, s.now()))
	if errors.Is(err, store.ErrHandoffPending) {
		return fmt.Errorf("%w: %w", ErrHandoffPending, err)
	}
	if err != nil {
		return fmt.Errorf("record %s retired: %w", row.LogicalID, err)
	}
	return nil
}

// markGone records a worker whose pane went before retire closed it, unless
// a herdr live handoff may have left it running on another terminal
// (unkeyed).
func (s *Service) markGone(ctx context.Context, tg target) (RetireResult, error) {
	if tg.row != nil {
		if err := s.unkeyed(ctx, tg.row.LogicalID, ""); err != nil {
			return RetireResult{}, err
		}
	}
	if err := s.endRow(ctx, tg.row, store.StatusGone); err != nil {
		return RetireResult{}, err
	}
	return RetireResult{ID: tg.id, PaneID: tg.paneID, Changed: true, Outcome: OutcomeGone}, nil
}

// unkeyed refuses to end or close what a herdr live handoff the team poll
// has not re-keyed yet may have left running (NERD-5534). A live handoff
// renews a pane's terminal and keeps its id and shell, and the poll re-keys
// the row a pass later (A35). Until then herdr lists the row's terminal
// nowhere, so the pane reads as gone, or, named by its id, as a pane no row
// binds.
//
// It reads the rows, then the layout, as the poll does, and judges them as
// the poll would (team.Reconcile): a live row with a shell whose terminal
// herdr lists nowhere, and a candidate pane, of the poll's own, whose shell
// has the row's pid and start time, is the worker, still running. id limits
// the rows to that worker's; pane, when set, limits the candidates to that
// pane, and a row whose handoff is pending is refused as A33 part 2a refuses
// it, ErrHandoffPending. Otherwise it is ErrPaneChanged, with nothing closed
// or ended, and a retry once the poll has re-keyed the row retires it. A
// lost row, or one with no shell, has nothing to prove and none to re-key:
// it is left to A35's rule.
func (s *Service) unkeyed(ctx context.Context, id, pane string) error {
	rows, err := s.Store.Workers(ctx)
	if err != nil {
		return fmt.Errorf("read workers: %w", err)
	}
	l, err := s.readLayout(ctx)
	if err != nil {
		return err
	}
	for _, ck := range team.Reconcile(l.Layout, rows) {
		r := ck.Row
		if ck.Pane != "" || r.ShellPID == 0 || (id != "" && r.LogicalID != id) {
			continue
		}
		for _, p := range ck.Candidates {
			if pane != "" && p.PaneID != pane {
				continue
			}
			holds, err := s.holdsShell(ctx, p.PaneID, r)
			if err != nil {
				return err
			}
			if !holds {
				continue
			}
			if pane != "" {
				pending, err := s.Store.PendingHandoffs(ctx)
				if err != nil {
					return fmt.Errorf("read pending handoffs: %w", err)
				}
				if err := heldBy(pending, r.LogicalID); err != nil {
					return err
				}
			}
			return fmt.Errorf("%w: herdr lists %s's terminal %s nowhere, and %s runs its shell: a herdr live handoff the team poll has not re-keyed yet; nothing was closed or ended. Retry after the poll's next pass; the live handoff erased the pane's tokens, so with --after-handoff the worker must run handoff ready again first", ErrPaneChanged, r.LogicalID, r.TerminalID, p.PaneID)
		}
	}
	return nil
}

// holdsShell reports whether pane is proven to run r's shell: herdr's shell
// pid for it is r's, and so is that process's start time, read with the
// team poll's own hook (A35). Only a proof refuses retire. A pane gone holds
// nothing. So, unproven, does a pane herdr answers for without describing
// its shell (an error body, a reply the schema does not allow, or no shell
// pid yet), a service with no StartTime hook, and a shell whose start cannot
// be read, which has exited: retire then keeps A35's rule for it, as for a
// row with no shell, and a pane herdr never describes cannot block it. Only
// a call that did not get herdr's answer (herdr unreachable, a call or the
// op's budget timing out, the daemon stopping) is ErrHerdr, for a retry;
// the daemon answers one past the op's own budget as timeout (A23).
func (s *Service) holdsShell(ctx context.Context, pane string, r store.Worker) (bool, error) {
	info, err := s.Herdr.PaneProcessInfo(ctx, pane)
	if errors.Is(err, herdr.ErrAPI) || errors.Is(err, herdr.ErrProtocol) {
		return false, nil // herdr answered without describing the pane's shell: unproven, not a failure
	}
	if err != nil {
		return false, herdrFailed("read the shell of "+pane, err)
	}
	if info.ShellPID == 0 || info.ShellPID != r.ShellPID || s.StartTime == nil {
		return false, nil
	}
	start, err := s.StartTime(int(info.ShellPID))
	if err != nil {
		return false, nil //nolint:nilerr // a shell whose start cannot be read has exited, and is no row's (A35)
	}
	return start == r.ShellStart, nil
}

// resolveTarget finds the pane to close, changing no row. done is set when
// the worker is retired already. A row whose handoff is pending is refused
// here too, so --after-handoff never waits only to be refused.
func (s *Service) resolveTarget(ctx context.Context, name string) (target, *RetireResult, error) {
	teams, err := s.teams()
	if err != nil {
		return target{}, nil, err
	}
	l, err := s.readLayout(ctx)
	if err != nil {
		return target{}, nil, err
	}
	rows, err := s.Store.Workers(ctx)
	if err != nil {
		return target{}, nil, fmt.Errorf("read workers: %w", err)
	}
	pending, err := s.Store.PendingHandoffs(ctx)
	if err != nil {
		return target{}, nil, fmt.Errorf("read pending handoffs: %w", err)
	}
	bound := team.Bindings(rows)
	liveRow := func(id string) *store.Worker {
		i := slices.IndexFunc(rows, func(r store.Worker) bool { return r.LogicalID == id })
		if i < 0 {
			return nil
		}
		return &rows[i]
	}

	// A live row's pane comes first; then, for an id no live row has, the
	// agent pane of the tab labelled with it; then a pane id.
	if r := liveRow(name); r != nil {
		tg := target{id: name, row: r}
		if p, ok := byTerminal(l.Panes, r.TerminalID); ok && bound[r.TerminalID] == name {
			tg.paneID, tg.terminal = p.PaneID, p.TerminalID
		}
		return tg, nil, heldBy(pending, name)
	}
	pane, byLabel := workerPane(name, teams, l)
	if !byLabel {
		var done *RetireResult
		pane, done, err = s.paneByID(ctx, name, l)
		if errors.Is(err, ErrUnknownWorker) && slices.ContainsFunc(pending, func(h store.Handoff) bool { return h.NewPane == name }) {
			return target{paneID: name, unlisted: true}, nil, nil
		}
		if err != nil || done != nil {
			return target{}, done, err
		}
	}
	label := l.tabLabel(pane.TabID)
	for _, t := range teams {
		if ws, _ := tabsOf(t).Workspace(l.Workspaces); ws == pane.WorkspaceID && label == tabsOf(t).BossTab() {
			return target{}, nil, fmt.Errorf("%w: %s is in %s, the boss's tab", ErrNotAWorker, pane.PaneID, label)
		}
	}
	tg := target{id: bound[pane.TerminalID], paneID: pane.PaneID, terminal: pane.TerminalID}
	if tg.id != "" {
		tg.row = liveRow(tg.id)
		return tg, nil, heldBy(pending, tg.id)
	}
	if byLabel {
		tg.id = name
	}
	return tg, nil, nil
}

// paneByID finds the pane name names when no tab is labelled with it: none
// for a logical id retired before (done), else the pane with that id, or the
// one herdr resolves a stale id to.
func (s *Service) paneByID(ctx context.Context, name string, l layout) (herdr.PaneInfo, *RetireResult, error) {
	if row, err := s.Store.WorkerRow(ctx, name); err == nil {
		return herdr.PaneInfo{}, &RetireResult{ID: name, PaneID: row.PaneID, Outcome: OutcomeAlreadyRetired}, nil
	} else if !errors.Is(err, store.ErrNoWorker) {
		return herdr.PaneInfo{}, nil, fmt.Errorf("read worker %s: %w", name, err)
	}
	if p, ok := l.pane(name); ok {
		return p, nil, nil
	}
	if info, err := s.Herdr.PaneProcessInfo(ctx, name); err == nil {
		if p, ok := l.pane(info.PaneID); ok {
			return p, nil, nil
		}
	}
	return herdr.PaneInfo{}, nil, fmt.Errorf("%w: %q is no worker's logical id and no pane herdr lists", ErrUnknownWorker, name)
}

// workerPane is the pane of the worker whose tab is labelled id in a tabs
// team's workspace: its first agent pane, else its first pane.
func workerPane(id string, teams []settings.Team, l layout) (herdr.PaneInfo, bool) {
	for _, t := range teams {
		r := tabsOf(t)
		if _, ok := r.Type(id); !ok {
			continue
		}
		ws, _ := r.Workspace(l.Workspaces)
		for _, tab := range l.Tabs {
			if tab.WorkspaceID != ws || tab.Label != id {
				continue
			}
			first, agent := -1, -1
			for i, p := range l.Panes {
				if p.TabID != tab.TabID {
					continue
				}
				if first < 0 {
					first = i
				}
				if agent < 0 && p.Agent != "" {
					agent = i
				}
			}
			if agent >= 0 {
				return l.Panes[agent], true
			}
			if first >= 0 {
				return l.Panes[first], true
			}
		}
	}
	return herdr.PaneInfo{}, false
}

// waitHandoff waits for the handoff token of the pane with tg's terminal
// and returns that pane's id now, which a move can have changed. gone is
// true when herdr stops listing the terminal first. An unreachable herdr or
// a call that ran out of time is polled again until ctx ends.
func (s *Service) waitHandoff(ctx context.Context, tg target) (pane string, gone bool, err error) {
	for {
		panes, err := s.Herdr.ListPanes(ctx)
		switch {
		case err == nil:
			p, ok := byTerminal(panes, tg.terminal)
			if !ok {
				return "", true, nil
			}
			if p.Tokens[team.HandoffKey] != "" {
				return p.PaneID, false, nil
			}
		case ctx.Err() == nil && !errors.Is(err, herdr.ErrUnavailable) && !errors.Is(err, context.DeadlineExceeded):
			return "", false, herdrFailed("list panes", err)
		}
		if err := sleep(ctx, orDefault(s.HandoffPoll, DefaultHandoffPoll)); err != nil {
			return "", false, fmt.Errorf("%w: %s wrote no handoff; its pane stays open: %w", ErrHandoffTimeout, tg.paneID, err)
		}
	}
}

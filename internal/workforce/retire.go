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
	// terminal listed nowhere (A35): retired, with nothing closed.
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
// team's <short>-boss tab is refused (ErrNotAWorker). With AfterHandoff it
// first waits, without the workforce lock, until the pane with the target's
// terminal carries a non-empty handoff token, polling every HandoffPoll; a
// terminal herdr stops listing while it waits is the worker gone, and ctx
// ending first is ErrHandoffTimeout, with the pane left open. The pane is
// closed only once pane.get shows it still has that terminal. Retiring what
// is retired already answers Changed false.
func (s *Service) Retire(ctx context.Context, req RetireRequest) (RetireResult, error) {
	if strings.TrimSpace(req.Target) == "" {
		return RetireResult{}, fmt.Errorf("%w: retire needs a logical id or a pane id", ErrBadRequest)
	}
	tg, done, err := s.resolveTarget(ctx, req.Target)
	if err != nil || done != nil {
		return deref(done), err
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
// closed. A pane that is a pending handoff's replacement fails that handoff
// first, then closes, so a close that errors leaves an ordinary pane for the
// retry and never an inbox held. gone is the pane gone while retire waited
// for its handoff.
func (s *Service) finish(ctx context.Context, tg target, gone bool) (RetireResult, error) {
	h, replaced, err := s.handoffOf(ctx, tg)
	if err != nil {
		return RetireResult{}, err
	}
	if tg.unlisted && !replaced {
		return RetireResult{}, fmt.Errorf("%w: %q is no worker's logical id and no pane herdr lists", ErrUnknownWorker, tg.paneID)
	}
	if replaced {
		if err := s.Store.FailHandoff(ctx, h.ID, s.now()); err != nil {
			return RetireResult{}, fmt.Errorf("fail %s's handoff to %s: %w", h.LogicalID, h.NewPane, err)
		}
	}
	res, err := s.retireTarget(ctx, tg, gone)
	if err != nil {
		return RetireResult{}, err
	}
	if replaced {
		res.HandoffFailed = h.LogicalID
	}
	return res, nil
}

// retireTarget closes tg's pane, once proven, and ends its row: retired,
// or gone for a pane that went first.
func (s *Service) retireTarget(ctx context.Context, tg target, gone bool) (RetireResult, error) {
	switch {
	case tg.paneID == "":
		if err := s.endRow(ctx, tg.row, store.StatusRetired); err != nil {
			return RetireResult{}, err
		}
		return RetireResult{ID: tg.id, Changed: true, Outcome: OutcomeUnbound}, nil
	case tg.unlisted || gone:
		return s.markGone(ctx, tg)
	}
	gone, err := s.closeProven(ctx, tg)
	if err != nil {
		return RetireResult{}, err
	}
	if gone {
		return s.markGone(ctx, tg)
	}
	if err := s.endRow(ctx, tg.row, store.StatusRetired); err != nil {
		return RetireResult{}, err
	}
	return RetireResult{ID: tg.id, PaneID: tg.paneID, Changed: true, Outcome: OutcomeRetired}, nil
}

// handoffOf reads the pending handoffs afresh and applies A33 part 2a's
// rule to tg. A row with one pending is refused (ErrHandoffPending). A pane
// no row binds may be a pending handoff's replacement, which it returns,
// found by the replacement's terminal, else by its pane id while herdr lists
// the recorded terminal nowhere: a herdr live handoff renews a terminal and
// keeps the pane id. A reissued pane id whose recorded terminal herdr lists
// elsewhere is no replacement.
func (s *Service) handoffOf(ctx context.Context, tg target) (store.Handoff, bool, error) {
	pending, err := s.Store.PendingHandoffs(ctx)
	if err != nil {
		return store.Handoff{}, false, fmt.Errorf("read pending handoffs: %w", err)
	}
	if tg.row != nil {
		return store.Handoff{}, false, heldBy(pending, tg.row.LogicalID)
	}
	if i := slices.IndexFunc(pending, func(h store.Handoff) bool { return tg.terminal != "" && h.NewTerminal == tg.terminal }); i >= 0 {
		return pending[i], true, nil
	}
	i := slices.IndexFunc(pending, func(h store.Handoff) bool { return h.NewPane == tg.paneID })
	if i < 0 {
		return store.Handoff{}, false, nil
	}
	if pending[i].NewTerminal != "" {
		panes, err := s.Herdr.ListPanes(ctx)
		if err != nil {
			return store.Handoff{}, false, herdrFailed("list panes", err)
		}
		if _, listed := byTerminal(panes, pending[i].NewTerminal); listed {
			return store.Handoff{}, false, nil
		}
	}
	return pending[i], true, nil
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

// endRow ends row, if there is one, with status; a row another op ended or
// rebound first is left as it is.
func (s *Service) endRow(ctx context.Context, row *store.Worker, status string) error {
	if row == nil {
		return nil
	}
	err := ignoreGone(s.Store.SetWorkerStatus(ctx, *row, status, s.now()))
	if errors.Is(err, store.ErrHandoffPending) {
		return fmt.Errorf("%w: %w", ErrHandoffPending, err)
	}
	if err != nil {
		return fmt.Errorf("record %s %s: %w", row.LogicalID, status, err)
	}
	return nil
}

// markGone records a worker whose pane went before retire closed it.
func (s *Service) markGone(ctx context.Context, tg target) (RetireResult, error) {
	if err := s.endRow(ctx, tg.row, store.StatusGone); err != nil {
		return RetireResult{}, err
	}
	return RetireResult{ID: tg.id, PaneID: tg.paneID, Changed: true, Outcome: OutcomeGone}, nil
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
		if t.Layout != settings.LayoutTabs {
			continue
		}
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
		if t.Layout != settings.LayoutTabs {
			continue
		}
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

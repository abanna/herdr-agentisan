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
}

// target is a resolved retire target: a pane, the terminal it showed, and
// the live row bound to it, if any.
type target struct {
	id, paneID, terminal string
	row                  *store.Worker
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
	if req.AfterHandoff {
		pane, gone, err := s.waitHandoff(ctx, tg)
		if err != nil {
			return RetireResult{}, err
		}
		if gone {
			return s.markGone(ctx, tg)
		}
		tg.paneID = pane
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if err := ignoreGone(s.Store.SetWorkerStatus(ctx, *row, status, s.now())); err != nil {
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

// resolveTarget finds the pane to close. done is set when there is nothing
// to close: the worker is retired already, or its row is bound to no pane.
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
		if p, ok := byTerminal(l.Panes, r.TerminalID); ok && bound[r.TerminalID] == name {
			return target{id: name, paneID: p.PaneID, terminal: p.TerminalID, row: r}, nil, nil
		}
		if r.HandoffPending {
			return target{}, nil, fmt.Errorf("%w: %s is bound to no pane while its handoff is pending; retire it once the handoff completes or fails", ErrHandoffPending, name)
		}
		if err := s.endRow(ctx, r, store.StatusRetired); err != nil {
			return target{}, nil, err
		}
		return target{}, &RetireResult{ID: name, Changed: true, Outcome: OutcomeUnbound}, nil
	}
	pane, byLabel := workerPane(name, teams, l)
	if !byLabel {
		var done *RetireResult
		if pane, done, err = s.paneByID(ctx, name, l); err != nil || done != nil {
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
	} else if byLabel {
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
// true when herdr stops listing the terminal first.
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
		case ctx.Err() == nil && !errors.Is(err, herdr.ErrUnavailable):
			return "", false, herdrFailed("list panes", err)
		}
		if err := sleep(ctx, orDefault(s.HandoffPoll, DefaultHandoffPoll)); err != nil {
			return "", false, fmt.Errorf("%w: %s wrote no handoff; its pane stays open: %w", ErrHandoffTimeout, tg.paneID, err)
		}
	}
}

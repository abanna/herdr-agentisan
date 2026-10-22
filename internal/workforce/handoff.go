package workforce

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// The handoff token the daemon writes (ADR-001 A5, A33).
const (
	// HandoffMark is the handoff token's value on a worker ready to be
	// replaced: the value hready.sh writes, so a mark either one left reads
	// the same.
	HandoffMark = "⟳ READY TO RESTART"
	// HandoffTTL is herdr's longest TTL: a mark nobody acts on expires after
	// a day.
	HandoffTTL = 24 * time.Hour
)

// HandoffReadyRequest is handoff ready: mark a worker's pane ready to be
// replaced, or, with Clear, take the mark off.
type HandoffReadyRequest struct {
	// Target is a worker's logical id, a tabs team's worker tab label or a
	// pane id; "" is the caller's own pane.
	Target string `json:"target,omitempty"`
	Clear  bool   `json:"clear,omitempty"`
	// CallerPane is the current id of the pane the command runs in, as
	// FindCallerPane confirmed it, or "". It is read only with no Target.
	CallerPane string `json:"caller_pane,omitempty"`
	// Worker is the command's HERDR_AGENTISAN_WORKER, or "". With no Target
	// and no pane confirmed, it names the worker if a live row has that id.
	Worker string `json:"worker,omitempty"`
}

// HandoffReadyResult is handoff ready's answer: the worker as the team model
// names it, and its pane's mark.
type HandoffReadyResult struct {
	// ID is the worker's logical id, or its pane id while no row binds it.
	ID      string `json:"id"`
	Bound   bool   `json:"bound"`
	Project string `json:"project"`
	Group   string `json:"group"`
	PaneID  string `json:"pane_id"`
	// Ready is whether the pane carries the mark now.
	Ready bool `json:"ready"`
	// Changed is false when the pane already carried that value.
	Changed bool `json:"changed"`
}

// HandoffReady writes the handoff token on a worker's agent pane, or clears
// it, from one read of herdr's layout and the workers rows (A35). The target
// is resolved without changing any row: a live row's id names the pane herdr
// lists with its terminal, and a row bound to no pane is ErrUnknownWorker;
// then a tabs team's worker tab label, its agent pane; then a pane id, or the
// pane herdr resolves a stale one to. With no target it is the caller's
// confirmed pane, else the pane of the live row req.Worker names, else
// ErrBadRequest. The pane must be an agent pane in a team's group: the
// boss's, a shell and a pane in no team's group are ErrNotAWorker. It writes
// only once pane.get shows the pane still has the terminal it read, else
// ErrPaneChanged, since retire --after-handoff trusts the mark. The token is
// written even when the pane carries its value already, so the daemon's
// write replaces one hready.sh left and renews its TTL; Changed says whether
// the value differed.
func (s *Service) HandoffReady(ctx context.Context, req HandoffReadyRequest) (HandoffReadyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	teams, err := s.teams()
	if err != nil {
		return HandoffReadyResult{}, err
	}
	teams = withSpaces(teams)
	l, err := s.readLayout(ctx)
	if err != nil {
		return HandoffReadyResult{}, err
	}
	rows, err := s.Store.Workers(ctx)
	if err != nil {
		return HandoffReadyResult{}, fmt.Errorf("read workers: %w", err)
	}
	pane, err := s.handoffPane(ctx, req, teams, l, rows)
	if err != nil {
		return HandoffReadyResult{}, err
	}
	res, err := member(pane, teamsIn(teams, l, rows))
	if err != nil {
		return HandoffReadyResult{}, err
	}
	if err := s.provePane(ctx, pane); err != nil {
		return HandoffReadyResult{}, err
	}
	m := herdr.PaneMetadata{PaneID: pane.PaneID, Source: report.Source, Tokens: map[string]string{team.HandoffKey: HandoffMark}, TTLMillis: uint64(HandoffTTL / time.Millisecond)}
	if req.Clear {
		m.Tokens[team.HandoffKey], m.TTLMillis = "", 0
	}
	err = s.Herdr.ReportPaneMetadata(ctx, m)
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		return HandoffReadyResult{}, fmt.Errorf("%w: %s closed before its handoff token was written; nothing was written", ErrPaneChanged, pane.PaneID)
	case err != nil:
		return HandoffReadyResult{}, herdrFailed("write the handoff token on "+pane.PaneID, err)
	}
	res.Ready = !req.Clear
	res.Changed = pane.Tokens[team.HandoffKey] != m.Tokens[team.HandoffKey]
	return res, nil
}

// handoffPane is the pane req means in l (see HandoffReady).
func (s *Service) handoffPane(ctx context.Context, req HandoffReadyRequest, teams []settings.Team, l layout, rows []store.Worker) (herdr.PaneInfo, error) {
	if req.Target == "" {
		if p, ok := l.pane(req.CallerPane); ok {
			return p, nil
		}
		if req.Worker == "" || !slices.ContainsFunc(rows, func(r store.Worker) bool { return r.LogicalID == req.Worker }) {
			return herdr.PaneInfo{}, fmt.Errorf("%w: name a worker or a pane: no pane of the caller is confirmed, and no live worker has the id in HERDR_AGENTISAN_WORKER (%q)", ErrBadRequest, req.Worker)
		}
		return rowPane(req.Worker, l, rows)
	}
	name := req.Target
	if slices.ContainsFunc(rows, func(r store.Worker) bool { return r.LogicalID == name }) {
		return rowPane(name, l, rows)
	}
	if p, ok := workerPane(name, teams, l); ok {
		return p, nil
	}
	if p, ok := l.pane(name); ok {
		return p, nil
	}
	info, err := s.Herdr.PaneProcessInfo(ctx, name)
	switch {
	case err == nil:
		if p, ok := l.pane(info.PaneID); ok {
			return p, nil
		}
	case !errors.Is(err, herdr.ErrPaneNotFound):
		return herdr.PaneInfo{}, herdrFailed("resolve pane "+name, err)
	}
	return herdr.PaneInfo{}, fmt.Errorf("%w: %q is no live worker's logical id, no worker's tab and no pane herdr lists", ErrUnknownWorker, name)
}

// rowPane is the pane herdr lists with the terminal id's live row binds.
func rowPane(id string, l layout, rows []store.Worker) (herdr.PaneInfo, error) {
	i := slices.IndexFunc(rows, func(r store.Worker) bool { return r.LogicalID == id })
	if p, ok := byTerminal(l.Panes, rows[i].TerminalID); ok && team.Bindings(rows)[rows[i].TerminalID] == id {
		return p, nil
	}
	return herdr.PaneInfo{}, fmt.Errorf("%w: %s is bound to no pane herdr lists", ErrUnknownWorker, id)
}

// member is pane as a team's model holds it: a worker in a group, with an
// agent, else ErrNotAWorker.
func member(pane herdr.PaneInfo, teams []placed) (HandoffReadyResult, error) {
	for _, t := range teams {
		if slices.ContainsFunc(t.pr.Boss, func(w team.Worker) bool { return w.Pane.ID == pane.PaneID }) {
			return HandoffReadyResult{}, fmt.Errorf("%w: %s is in the boss's place; a boss hands off by its own protocol", ErrNotAWorker, pane.PaneID)
		}
		for _, g := range t.pr.Groups {
			i := slices.IndexFunc(g.Workers, func(w team.Worker) bool { return w.Pane.ID == pane.PaneID })
			if i < 0 {
				continue
			}
			w := g.Workers[i]
			if w.Pane.Agent == "" {
				return HandoffReadyResult{}, fmt.Errorf("%w: %s is a shell: herdr detects no agent in it", ErrNotAWorker, pane.PaneID)
			}
			return HandoffReadyResult{ID: w.ID, Bound: w.Bound, Project: t.pr.Name, Group: g.Name, PaneID: pane.PaneID}, nil
		}
	}
	return HandoffReadyResult{}, fmt.Errorf("%w: %s is in no team's group", ErrNotAWorker, pane.PaneID)
}

// provePane is nil once pane.get shows pane still has the terminal it was
// read with; a pane gone, showing another terminal, or listed with none is
// ErrPaneChanged.
func (s *Service) provePane(ctx context.Context, pane herdr.PaneInfo) error {
	if pane.TerminalID == "" {
		return fmt.Errorf("%w: herdr lists no terminal for %s, so nothing proves it is the pane read; nothing was written", ErrPaneChanged, pane.PaneID)
	}
	p, err := s.Herdr.GetPane(ctx, pane.PaneID)
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		return fmt.Errorf("%w: %s is gone since it was read; nothing was written", ErrPaneChanged, pane.PaneID)
	case err != nil:
		return herdrFailed("get "+pane.PaneID, err)
	case p.TerminalID != pane.TerminalID:
		return fmt.Errorf("%w: %s no longer shows terminal %s; nothing was written", ErrPaneChanged, pane.PaneID, pane.TerminalID)
	}
	return nil
}

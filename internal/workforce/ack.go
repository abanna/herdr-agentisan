package workforce

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// The handoff ack's refusals (A33 part 2b-1).
var (
	// ErrNoHandoff means the pane is no pending handoff's replacement.
	ErrNoHandoff = errors.New("no pending handoff for the pane")
	// ErrHandoffChanged means the replacement is not the session the
	// handoff began onto any more: herdr lists its pane nowhere, or a live
	// handoff renewed its terminal and its agent lost the replacement's
	// name. The boss abandons it with worker retire and rotates again.
	ErrHandoffChanged = errors.New("the handoff's replacement changed")
	// ErrStageMismatch means the replacement does not report the item and
	// stage the old pane does (I2).
	ErrStageMismatch = errors.New("the replacement's item and stage do not match")
)

// The ack's reading of the old pane's item and stage (Q2 a).
const (
	// OldMatched is an old pane whose item and stage the replacement's
	// equal.
	OldMatched = "matched"
	// OldUnread is an old pane gone, or missing its item or stage, as a
	// herdr live handoff leaves one: nothing to compare.
	OldUnread = "unread"
)

// The steps of a handoff a Service reports (A33): what the boss's commands
// did to it, not the token handoff ready writes.
const (
	// StepCompleted is the ack completing a handoff.
	StepCompleted = "completed"
	// StepFailed is a handoff ended without completing: worker retire
	// abandoning it by closing its replacement.
	StepFailed = "failed"
)

// HandoffStep is one step of a worker's handoff.
type HandoffStep struct {
	// ID is the worker's logical id; Project and Group are its row's.
	ID, Project, Group string
	// PaneID is the replacement's pane; OldPane the one the worker was on.
	PaneID, OldPane string
	// Step is StepCompleted or StepFailed.
	Step string
	// Old is OldMatched or OldUnread on StepCompleted, else "".
	Old string
}

// nextName is the herdr name a replacement runs under until its ack takes
// the worker's id (D7).
func nextName(id string) string { return id + "-next" }

// HandoffAckRequest is handoff ack: Target is the replacement's pane id, or
// "" for the caller's own pane.
type HandoffAckRequest struct {
	Target string `json:"target,omitempty"`
	// CallerPane is the current id of the pane the command runs in, as
	// FindCallerPane confirmed it, or "". It is read only with no Target.
	CallerPane string `json:"caller_pane,omitempty"`
}

// HandoffAckResult is handoff ack's answer: the worker, the pane it was on
// and the replacement it is bound to now, with the item and stage the
// replacement reported.
type HandoffAckResult struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Group   string `json:"group"`
	// OldPane is "" on an ack repeated after the handoff completed.
	OldPane   string `json:"old_pane,omitempty"`
	NewPane   string `json:"new_pane"`
	HandoffID int64  `json:"handoff_id,omitempty"`
	Item      string `json:"item,omitempty"`
	Stage     string `json:"stage,omitempty"`
	// Old is OldMatched or OldUnread; "" on a repeated ack.
	Old string `json:"old,omitempty"`
	// Changed is false for an ack repeated after the handoff completed.
	Changed bool `json:"changed"`
}

// HandoffAck is D7 step 3, under the workforce lock (A33 part 2b-1): the
// replacement acknowledges its worker's handoff and the daemon completes
// it. The pane is req.Target, else the caller's confirmed pane, never one
// guessed from HERDR_AGENTISAN_WORKER, which the old pane shares
// (ErrBadRequest). The replacement must host an agent (replacementAgent)
// and still show the terminal read; I2's gate passes (gate); then the
// names move (moveNames) before the handoff completes onto that terminal.
// Every step can be run again; once completed, a repeat changes nothing
// (ackedAlready).
func (s *Service) HandoffAck(ctx context.Context, req HandoffAckRequest) (HandoffAckResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := req.Target
	if name == "" {
		name = req.CallerPane
	}
	if name == "" {
		return HandoffAckResult{}, fmt.Errorf("%w: name the replacement's pane: no pane of the caller is confirmed", ErrBadRequest)
	}
	rows, err := s.Store.Workers(ctx)
	if err != nil {
		return HandoffAckResult{}, fmt.Errorf("read workers: %w", err)
	}
	pending, err := s.Store.PendingHandoffs(ctx)
	if err != nil {
		return HandoffAckResult{}, fmt.Errorf("read pending handoffs: %w", err)
	}
	panes, err := s.Herdr.ListPanes(ctx)
	if err != nil {
		return HandoffAckResult{}, herdrFailed("list panes", err)
	}
	agents, err := s.Herdr.ListAgents(ctx)
	if err != nil {
		return HandoffAckResult{}, herdrFailed("list agents", err)
	}
	l := layout{Layout: team.Layout{Panes: panes, Agents: agents}}
	pane, listed := l.pane(name)
	if !listed {
		if slices.ContainsFunc(pending, func(h store.Handoff) bool { return h.NewPane == name }) {
			return HandoffAckResult{}, fmt.Errorf("%w: herdr lists %s, a pending handoff's replacement, nowhere; worker retire %s abandons it", ErrHandoffChanged, name, name)
		}
		return HandoffAckResult{}, fmt.Errorf("%w: herdr lists no pane %q", ErrUnknownWorker, name)
	}
	h, ok, err := replacementOf(pending, pane.PaneID, pane.TerminalID, func(t string) (bool, error) { _, ok := byTerminal(panes, t); return ok, nil })
	if err != nil {
		return HandoffAckResult{}, err
	}
	if !ok {
		return s.ackedAlready(ctx, pane, l, rows, pending)
	}
	agent, err := replacementAgent(h, pane, l)
	if err != nil {
		return HandoffAckResult{}, err
	}
	now, err := s.Herdr.GetPane(ctx, pane.PaneID)
	switch {
	case errors.Is(err, herdr.ErrPaneNotFound):
		return HandoffAckResult{}, fmt.Errorf("%w: %s is gone since it was read; nothing was renamed", ErrPaneChanged, pane.PaneID)
	case err != nil:
		return HandoffAckResult{}, herdrFailed("get "+pane.PaneID, err)
	case now.TerminalID != pane.TerminalID:
		return HandoffAckResult{}, fmt.Errorf("%w: %s no longer shows terminal %s; nothing was renamed", ErrPaneChanged, pane.PaneID, pane.TerminalID)
	}
	res := HandoffAckResult{ID: h.LogicalID, Project: h.Project, Group: h.Group, NewPane: pane.PaneID, HandoffID: h.ID, Changed: true}
	res.Item, res.Stage = now.Tokens[report.ItemKey], now.Tokens[report.StageKey]
	i := slices.IndexFunc(rows, func(r store.Worker) bool { return r.LogicalID == h.LogicalID })
	if i < 0 {
		return HandoffAckResult{}, fmt.Errorf("%w: %s has no live row to complete onto", ErrNoHandoff, h.LogicalID)
	}
	// The store refuses to complete onto a bound terminal; refused here, it
	// is refused before the names move, so a retry never meets it past them.
	if j := slices.IndexFunc(rows, func(r store.Worker) bool { return r.TerminalID == pane.TerminalID }); j >= 0 {
		return HandoffAckResult{}, fmt.Errorf("%w: %s's terminal %s is bound to %s; nothing was renamed", ErrPaneChanged, pane.PaneID, pane.TerminalID, rows[j].LogicalID)
	}
	old, oldListed := byTerminal(panes, rows[i].TerminalID)
	if oldListed {
		res.OldPane = old.PaneID
	}
	if res.Old, err = gate(res, old, oldListed); err != nil {
		return HandoffAckResult{}, err
	}
	if err := s.moveNames(ctx, h.LogicalID, rows[i].TerminalID, agent, l); err != nil {
		return HandoffAckResult{}, err
	}
	if err := s.Store.CompleteHandoffOnto(ctx, h.ID, pane.TerminalID, s.now()); err != nil {
		return HandoffAckResult{}, fmt.Errorf("complete %s's handoff to %s: %w", h.LogicalID, pane.PaneID, err)
	}
	s.handoffStep(ctx, HandoffStep{ID: res.ID, Project: res.Project, Group: res.Group, PaneID: res.NewPane, OldPane: res.OldPane, Step: StepCompleted, Old: res.Old})
	return res, nil
}

// replacementOf is the pending handoff whose replacement is the pane paneID
// showing terminal ("" for none read): found by its recorded terminal, else
// by its pane id while listed says herdr lists the recorded terminal
// nowhere, since a herdr live handoff renews a terminal and keeps the pane id
// (A35). A pane id whose recorded terminal herdr lists elsewhere was
// reissued, and is no replacement. listed runs at most once.
func replacementOf(pending []store.Handoff, paneID, terminal string, listed func(string) (bool, error)) (store.Handoff, bool, error) {
	if i := slices.IndexFunc(pending, func(h store.Handoff) bool { return terminal != "" && h.NewTerminal == terminal }); i >= 0 {
		return pending[i], true, nil
	}
	i := slices.IndexFunc(pending, func(h store.Handoff) bool { return h.NewPane == paneID })
	if i < 0 {
		return store.Handoff{}, false, nil
	}
	if pending[i].NewTerminal != "" {
		elsewhere, err := listed(pending[i].NewTerminal)
		if err != nil || elsewhere {
			return store.Handoff{}, false, err
		}
	}
	return pending[i], true, nil
}

// replacementAgent is the agent herdr names in pane, h's replacement. A pane
// with none is ErrNotAWorker. One whose terminal a live handoff renewed must
// still run the replacement's agent, which its herdr name proves (UQ3),
// else ErrHandoffChanged.
func replacementAgent(h store.Handoff, pane herdr.PaneInfo, l layout) (herdr.AgentInfo, error) {
	i := slices.IndexFunc(l.Agents, func(a herdr.AgentInfo) bool { return a.TerminalID == pane.TerminalID })
	if i < 0 {
		return herdr.AgentInfo{}, fmt.Errorf("%w: %s hosts no agent herdr names", ErrNotAWorker, pane.PaneID)
	}
	a := l.Agents[i]
	if pane.TerminalID != h.NewTerminal && a.Name != nextName(h.LogicalID) && a.Name != h.LogicalID {
		return herdr.AgentInfo{}, fmt.Errorf("%w: %s shows a new terminal, and its agent is named %q, not %s: it may not be the session %s's handoff began onto; worker retire %s abandons it",
			ErrHandoffChanged, pane.PaneID, a.Name, nextName(h.LogicalID), h.LogicalID, pane.PaneID)
	}
	return a, nil
}

// gate is I2's (Q2 a): the replacement's item and stage, both set, and the
// old pane's when it reports both. It returns OldMatched or OldUnread.
func gate(res HandoffAckResult, old herdr.PaneInfo, listed bool) (string, error) {
	oldItem, oldStage := old.Tokens[report.ItemKey], old.Tokens[report.StageKey]
	pairs := fmt.Sprintf("new item %q, stage %q; old item %q, stage %q", res.Item, res.Stage, oldItem, oldStage)
	if res.Item == "" || res.Stage == "" {
		return "", fmt.Errorf("%w: %s: the replacement reports no item or no stage; re-report the handed-off pair and ack again", ErrStageMismatch, pairs)
	}
	if !listed || oldItem == "" || oldStage == "" {
		return OldUnread, nil
	}
	if res.Item != oldItem || res.Stage != oldStage {
		return "", fmt.Errorf("%w: %s; re-report the old pane's pair and ack again", ErrStageMismatch, pairs)
	}
	return OldMatched, nil
}

// moveNames gives the replacement, agent, the worker id's herdr name: it
// clears the name off the old agent, the one on the row's terminal, then
// renames agent. A replacement with the name already, as a crash past the
// rename leaves it, renames nothing. The name held by any other agent is
// ErrPaneChanged, with nothing renamed: a live handoff the poll has not
// re-keyed yet looks so, and the ack can be run again once it has. A clear
// whose agent went meanwhile has nothing to clear. A rename that fails puts
// the old name back, best effort.
func (s *Service) moveNames(ctx context.Context, id, oldTerminal string, agent herdr.AgentInfo, l layout) error {
	cleared := ""
	if i := slices.IndexFunc(l.Agents, func(a herdr.AgentInfo) bool { return a.Name == id }); i >= 0 {
		holder := l.Agents[i]
		switch holder.TerminalID {
		case agent.TerminalID:
			return nil
		case oldTerminal:
		default:
			return fmt.Errorf("%w: the agent named %s is on %s, neither %s's pane nor its replacement; nothing was renamed", ErrPaneChanged, id, holder.PaneID, id)
		}
		_, err := s.Herdr.RenameAgent(ctx, holder.PaneID, "")
		switch {
		case err == nil:
			cleared = holder.PaneID
		case !errors.Is(err, herdr.ErrAgentNotFound) && !errors.Is(err, herdr.ErrPaneNotFound):
			return herdrFailed("clear the name of "+holder.PaneID, err)
		}
	}
	if _, err := s.Herdr.RenameAgent(ctx, agent.PaneID, id); err != nil {
		if cleared != "" {
			// Best effort: the rename's error is the one to answer.
			_, _ = s.Herdr.RenameAgent(ctx, cleared, id)
		}
		return herdrFailed("rename "+agent.PaneID+" "+id+"; the handoff stays pending", err)
	}
	return nil
}

// ackedAlready answers an ack on a pane no pending handoff names: Changed
// false for the worker's bound pane, its agent named the id, that a handoff
// completed onto, as a crash past the complete leaves it (Q4); else
// ErrNoHandoff, naming the replacement when the pane is a worker's own pane
// whose handoff is pending, the old session acking by mistake.
func (s *Service) ackedAlready(ctx context.Context, pane herdr.PaneInfo, l layout, rows []store.Worker, pending []store.Handoff) (HandoffAckResult, error) {
	id := team.Bindings(rows)[pane.TerminalID]
	if id == "" {
		return HandoffAckResult{}, fmt.Errorf("%w: %s is no pending handoff's replacement", ErrNoHandoff, pane.PaneID)
	}
	if i := slices.IndexFunc(pending, func(h store.Handoff) bool { return h.LogicalID == id }); i >= 0 {
		return HandoffAckResult{}, fmt.Errorf("%w: %s is %s's own pane; its replacement is %s, which acks", ErrNoHandoff, pane.PaneID, id, pending[i].NewPane)
	}
	row := rows[slices.IndexFunc(rows, func(r store.Worker) bool { return r.LogicalID == id })]
	named := slices.ContainsFunc(l.Agents, func(a herdr.AgentInfo) bool { return a.TerminalID == pane.TerminalID && a.Name == id })
	done, err := s.Store.HandoffAcknowledgedOnto(ctx, id, row.Generation, pane.PaneID)
	if err != nil {
		return HandoffAckResult{}, fmt.Errorf("read %s's handoffs: %w", id, err)
	}
	if !named || !done {
		return HandoffAckResult{}, fmt.Errorf("%w: %s is %s's pane, which no handoff completed onto", ErrNoHandoff, pane.PaneID, id)
	}
	return HandoffAckResult{ID: id, Project: row.Project, Group: row.Group, NewPane: pane.PaneID}, nil
}

// handoffStep reports step to HandoffSteps, if set.
func (s *Service) handoffStep(ctx context.Context, step HandoffStep) {
	if s.HandoffSteps != nil {
		s.HandoffSteps(ctx, step)
	}
}

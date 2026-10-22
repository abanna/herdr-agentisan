package workforce

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// Send's refusals (ADR-001 A30).
var (
	// ErrUnknownRecipient means send's recipient is no live worker and no
	// configured team's boss, or is boss from a sender in no team.
	ErrUnknownRecipient = errors.New("no such recipient")
	// ErrUnknownSender means nothing tells who is sending: no pane of the
	// caller is confirmed and no live row has its HERDR_AGENTISAN_WORKER,
	// so a notice about the message would have nowhere to go.
	ErrUnknownSender = errors.New("no sender can be found")
)

// BossAlias is the recipient that means the sender's own team's boss (UQ2).
const BossAlias = "boss"

// WorkerEnv names the variable a spawned worker's shell carries its logical
// id in (A23).
const WorkerEnv = "HERDR_AGENTISAN_WORKER"

// SendRequest is send: Body, from the command's caller to To.
type SendRequest struct {
	// To is a worker's logical id, a team's boss by the name herdr knows it
	// by, or BossAlias.
	To   string `json:"to"`
	Body string `json:"body"`
	// CallerPane is the current id of the pane the command runs in, as
	// FindCallerPane confirmed it, or "".
	CallerPane string `json:"caller_pane,omitempty"`
	// Worker is the command's HERDR_AGENTISAN_WORKER, or "". It names the
	// sender only when no pane is confirmed and a live row has that id.
	Worker string `json:"worker,omitempty"`
}

// Party is one end of a message: its kind, worker, boss or pane, and id.
type Party struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// SendResult is send's answer: the message queued, and whom the daemon took
// it to be to and from.
type SendResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
	To    Party  `json:"to"`
	From  Party  `json:"from"`
	// Queued is when the message was queued, its row's created time: its
	// queued event's time, not part of the answer.
	Queued time.Time `json:"-"`
}

// Send queues req.Body from the caller to req.To, both read from one read of
// herdr's layout and the workers rows (A35: an op acts only on what it read
// together). The sender is, in order (B4): the worker a live row binds to
// the caller pane's terminal; a team's boss, on a pane in that team's boss
// place; else the pane itself, by its terminal (B3), since herdr reissues
// pane ids but never terminals. With no pane confirmed it is the worker
// req.Worker names, if a live row has that id, else ErrUnknownSender. The
// recipient BossAlias is the sender's own team's boss.
func (s *Service) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	if req.To == "" {
		return SendResult{}, fmt.Errorf("%w: no recipient", ErrBadRequest)
	}
	teams, err := s.teams()
	if err != nil {
		return SendResult{}, err
	}
	teams = withSpaces(teams)
	l, err := s.readLayout(ctx)
	if err != nil {
		return SendResult{}, err
	}
	rows, err := s.Store.Workers(ctx)
	if err != nil {
		return SendResult{}, fmt.Errorf("read workers: %w", err)
	}
	from, home, err := sender(req, teamsIn(teams, l, rows, s.Dashboard.Title), l, rows)
	if err != nil {
		return SendResult{}, err
	}
	to, err := recipient(req.To, home, teams, rows)
	if err != nil {
		return SendResult{}, err
	}
	m, err := s.Store.Send(ctx, to, from, req.Body, s.now())
	if errors.Is(err, store.ErrNotBound) {
		return SendResult{}, fmt.Errorf("%w: %w", ErrUnknownRecipient, err)
	}
	if err != nil {
		return SendResult{}, fmt.Errorf("queue the message: %w", err)
	}
	return SendResult{ID: m.ID, State: m.State, To: Party(m.To), From: Party(m.From), Queued: m.CreatedAt}, nil
}

// withSpaces is teams with the ◆ spaces, which belong to the unnamed project
// when the configuration names none, as the team poll resolves them: so a
// pane in ◆ boss sends as the boss the dispatcher types to.
func withSpaces(teams []settings.Team) []settings.Team {
	if slices.ContainsFunc(teams, func(t settings.Team) bool { return t.Layout == settings.LayoutSpaces }) {
		return teams
	}
	return append(slices.Clone(teams), settings.Team{Layout: settings.LayoutSpaces})
}

// placed is a configured team and its project in the model one read builds.
type placed struct {
	settings.Team
	pr team.Project
}

// teamsIn places every team in l, title being the dashboard pane's label
// when prefix+d opened it (resolverOf).
func teamsIn(teams []settings.Team, l layout, rows []store.Worker, title string) []placed {
	out := make([]placed, 0, len(teams))
	for _, t := range teams {
		p := placed{Team: t}
		m := team.Build(l.Layout, resolverOf(t, title), rows)
		if i := slices.IndexFunc(m.Projects, func(pr team.Project) bool { return pr.Name == t.Project }); i >= 0 {
			p.pr = m.Projects[i]
		}
		out = append(out, p)
	}
	return out
}

// sender is who sends, and the team it belongs to, nil for none (see Send).
func sender(req SendRequest, teams []placed, l layout, rows []store.Worker) (store.Address, *settings.Team, error) {
	pane, ok := l.pane(req.CallerPane)
	if !ok || pane.TerminalID == "" {
		i := slices.IndexFunc(rows, func(r store.Worker) bool { return req.Worker != "" && r.LogicalID == req.Worker })
		if i < 0 {
			return store.Address{}, nil, fmt.Errorf("%w: run send from a herdr pane: no pane of the caller is confirmed, and no live worker has the id in HERDR_AGENTISAN_WORKER (%q)", ErrUnknownSender, req.Worker)
		}
		return store.Address{Kind: store.KindWorker, ID: rows[i].LogicalID}, teamOf(teams, rows[i].Project), nil
	}
	if i := slices.IndexFunc(rows, func(r store.Worker) bool { return r.TerminalID == pane.TerminalID }); i >= 0 {
		return store.Address{Kind: store.KindWorker, ID: rows[i].LogicalID}, teamOf(teams, rows[i].Project), nil
	}
	for i, t := range teams {
		if slices.ContainsFunc(t.pr.Boss, func(w team.Worker) bool { return w.Pane.ID == pane.PaneID }) {
			return store.Address{Kind: store.KindBoss, ID: BossOf(t.Team)}, &teams[i].Team, nil
		}
	}
	return store.Address{Kind: store.KindPane, ID: pane.TerminalID}, teamOfPane(teams, pane), nil
}

// teamOf is project's team, nil when none is configured.
func teamOf(teams []placed, project string) *settings.Team {
	for i := range teams {
		if teams[i].Project == project {
			return &teams[i].Team
		}
	}
	return nil
}

// teamOfPane is the team whose workspaces hold pane, nil for none.
func teamOfPane(teams []placed, pane herdr.PaneInfo) *settings.Team {
	for i := range teams {
		if slices.Contains(teams[i].pr.Workspaces, pane.WorkspaceID) {
			return &teams[i].Team
		}
	}
	return nil
}

// recipient is the address to means (UQ2): BossAlias, the sender's home
// team's boss; a configured team's boss by its name; else a worker, which
// needs a live row (UQ1).
func recipient(to string, home *settings.Team, teams []settings.Team, rows []store.Worker) (store.Address, error) {
	if to == BossAlias {
		if home == nil {
			return store.Address{}, fmt.Errorf("%w: %s is the sender's own team's boss, and the sender is in no team; name the boss, such as <short>-boss", ErrUnknownRecipient, BossAlias)
		}
		return store.Address{Kind: store.KindBoss, ID: BossOf(*home)}, nil
	}
	if slices.ContainsFunc(teams, func(t settings.Team) bool { return BossOf(t) == to }) {
		return store.Address{Kind: store.KindBoss, ID: to}, nil
	}
	if !slices.ContainsFunc(rows, func(r store.Worker) bool { return r.LogicalID == to }) {
		return store.Address{}, fmt.Errorf("%w: no live worker has the id %q, and no team's boss is named so; a worker not spawned with worker spawn has none, so use herdr agent prompt", ErrUnknownRecipient, to)
	}
	return store.Address{Kind: store.KindWorker, ID: to}, nil
}

// BossOf is the name herdr knows t's boss by, which messages to it are
// typed to (A30): <short>-boss for a tabs team, boss for the ◆ spaces (A7).
func BossOf(t settings.Team) string {
	if t.Layout == settings.LayoutSpaces {
		return team.SpacesBoss
	}
	return tabsOf(t).BossTab()
}

// resolverOf is the resolver that finds t in herdr's layout. A tabs team's
// leaves out its dashboard pane, whose label is team.DashboardLabel or, when
// prefix+d opened it, title (team.Tabs.Dashboard).
func resolverOf(t settings.Team, title string) team.Resolver {
	if t.Layout == settings.LayoutSpaces {
		return team.Spaces{Project: t.Project, Color: t.Color}
	}
	r := tabsOf(t)
	r.DashboardTitle = title
	return r
}

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// RelocateEvery is how often a TeamSource finds its own pane again: a pane
// can be moved to another workspace at any time (ADR-001 A24). The polls in
// between ask the daemon alone, which never calls herdr.
const RelocateEvery = 10 * time.Second

// ErrNoTeamHere means the dashboard has no team to show: the daemon knows no
// team where it runs, or it cannot tell where it runs. It draws no board
// then, never another team's.
var ErrNoTeamHere = errors.New("no team here")

// ErrNotATeamsWorkspace is the one ErrNoTeamHere ADR-001 A24 names: the
// dashboard runs in a workspace the daemon knows no team of.
var ErrNotATeamsWorkspace = fmt.Errorf("%w: the workspace is no team's", ErrNoTeamHere)

// noPane is the footer's line when no herdr pane runs the dashboard.
const noPane = "this dashboard runs in no herdr pane; pass --project"

// placeError is a place the dashboard shows no team for. Its message is the
// footer's whole line; it matches ErrNoTeamHere, or kind when set (which
// wraps it), and what caused it.
type placeError struct {
	msg  string
	kind error
	err  error
}

func (e *placeError) Error() string { return e.msg }

func (e *placeError) Unwrap() []error {
	kind := e.kind
	if kind == nil {
		kind = ErrNoTeamHere
	}
	if e.err == nil {
		return []error{kind}
	}
	return []error{kind, e.err}
}

// Panes is the slice of the herdr client a Locator asks.
type Panes interface {
	ListPanes(ctx context.Context) ([]herdr.PaneInfo, error)
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
}

// Locator finds the workspace of the herdr pane the dashboard runs in, by
// the dashboard's own pid (ADR-001 A24). The environment herdr gave the
// pane's shell is only a hint: the pane may have moved since, and herdr
// reissues ids after a live handoff, so an id naming another pane now is
// not trusted unless that pane runs the pid.
type Locator struct {
	Herdr Panes
	// PID is the dashboard's process id.
	PID int
	// PaneID is HERDR_PANE_ID: the pane asked first until one is found.
	PaneID string
	// WorkspaceID is HERDR_WORKSPACE_ID, used only when herdr cannot be
	// asked.
	WorkspaceID string

	mu sync.Mutex
	// found is the pane the last Locate found, asked first next time.
	found string
}

// Place is where a Locator found the dashboard.
type Place struct {
	WorkspaceID string
	// PaneID is the dashboard's pane; "" when herdr could not be asked.
	PaneID string
	// Note says why WorkspaceID is a guess; "" when herdr confirmed it.
	Note string
}

// Locate lists herdr's panes and asks them, the pane found last (or
// PaneID) first, which runs the pid: as the pane's shell, its foreground
// job, or one of its foreground processes. That pane's workspace is the
// place. The first pane asked is believed alone, so a settled dashboard
// costs herdr two calls; any other is believed only when no second pane
// claims the pid. With no such pane there is no place, and no fallback:
// the footer says so. When herdr cannot be asked, the place is
// WorkspaceID, with a note, and without it there is none. A herdr that
// answers the list with an error was asked: there is no place then either.
func (l *Locator) Locate(ctx context.Context) (Place, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	panes, err := l.Herdr.ListPanes(ctx)
	switch {
	case errors.Is(err, herdr.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded):
		return l.unasked(err)
	case err != nil:
		return Place{}, &placeError{msg: fmt.Sprintf("herdr cannot say where this dashboard runs (%v); pass --project", err), err: err}
	}
	first := l.found
	if first == "" {
		first = l.PaneID
	}
	order := slices.Clone(panes)
	if i := slices.IndexFunc(order, func(p herdr.PaneInfo) bool { return p.PaneID == first }); i > 0 {
		order[0], order[i] = order[i], order[0]
	}
	var hits []herdr.PaneInfo
	for i, p := range order {
		info, err := l.Herdr.PaneProcessInfo(ctx, p.PaneID)
		switch {
		case errors.Is(err, herdr.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded):
			return l.unasked(err)
		case err != nil || !runs(info, l.PID):
			continue // closed since the list, or someone else's
		}
		hits = append(hits, p)
		if i == 0 && p.PaneID == first {
			break
		}
	}
	if len(hits) != 1 {
		l.found = ""
		return Place{}, &placeError{msg: noPane}
	}
	l.found = hits[0].PaneID
	return Place{WorkspaceID: hits[0].WorkspaceID, PaneID: hits[0].PaneID}, nil
}

// unasked is the place when herdr could not be asked: WorkspaceID's, with
// a note, or none.
func (l *Locator) unasked(err error) (Place, error) {
	if l.WorkspaceID == "" {
		return Place{}, &placeError{msg: "herdr cannot say where this dashboard runs, and HERDR_WORKSPACE_ID is unset; pass --project", err: err}
	}
	return Place{WorkspaceID: l.WorkspaceID, Note: fmt.Sprintf("herdr not answering: the team of HERDR_WORKSPACE_ID (%s)", l.WorkspaceID)}, nil
}

// runs reports whether pid runs in the pane info describes.
func runs(info herdr.ProcessInfo, pid int) bool {
	if pid <= 0 {
		return false
	}
	is := func(p uint32) bool { return int64(p) == int64(pid) }
	return is(info.ShellPID) || is(info.ForegroundProcessGroupID) ||
		slices.ContainsFunc(info.ForegroundProcesses, func(p herdr.Process) bool { return is(p.PID) })
}

// TeamSource is the dashboard's snapshot.Source for its own team (ADR-001
// A24): the daemon's snapshot of Project's team when set; else of the team
// of the workspace Locator finds the dashboard in, which the daemon maps.
// It finds that place on its first poll and every RelocateEvery after, and
// at once when the daemon answers no_team for a place not searched since it
// last served the team: the pane may have moved. A place that stays outside
// every team is searched only every RelocateEvery, so the polls in between
// never call herdr. Polls never overlap: the dashboard asks for the next one
// only after the last has answered.
type TeamSource struct {
	// Socket is the daemon's socket.
	Socket string
	// Project, when set, is the team shown, and nothing is located.
	Project string
	Locator *Locator
	// Now is the clock a place ages by; nil is time.Now.
	Now func() time.Time
	// Every is how often the place is found again; zero is RelocateEvery.
	Every time.Duration

	mu    sync.Mutex
	place Place
	err   error     // why the last search found no place
	at    time.Time // when the last search ran; zero before the first
	// searched is set by a search and cleared by a served snapshot.
	searched bool
}

// Snapshot asks the daemon for the team's snapshot. A place with no team,
// or none at all, is ErrNoTeamHere, with the footer's line as its message.
func (s *TeamSource) Snapshot(ctx context.Context) (snapshot.Snapshot, error) {
	if s.Project != "" {
		snap, err := s.ask(ctx, daemon.Selector{Project: s.Project})
		if errors.Is(err, daemon.ErrNoTeam) {
			return snapshot.Snapshot{}, &placeError{msg: fmt.Sprintf("herdr shows no team %s", s.Project), err: err}
		}
		return snap, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	start := s.now()
	if s.at.IsZero() || !start.Before(s.at.Add(s.every())) {
		s.locate(ctx, start)
	}
	if s.err != nil {
		return snapshot.Snapshot{}, s.err
	}
	snap, err := s.ask(ctx, daemon.Selector{WorkspaceID: s.place.WorkspaceID})
	if errors.Is(err, daemon.ErrNoTeam) && !s.searched {
		s.locate(ctx, start)
		if s.err != nil {
			return snapshot.Snapshot{}, s.err
		}
		snap, err = s.ask(ctx, daemon.Selector{WorkspaceID: s.place.WorkspaceID})
	}
	if errors.Is(err, daemon.ErrNoTeam) {
		return snapshot.Snapshot{}, &placeError{msg: fmt.Sprintf("this workspace (%s) is not a team's; pass --project", s.place.WorkspaceID), kind: ErrNotATeamsWorkspace, err: err}
	}
	if err == nil {
		s.searched = false
	}
	return snap, err
}

// Note is the footer's note on the place, such as herdr not answering.
func (s *TeamSource) Note() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.place.Note
}

// locate finds the place again at now. A search that finds none is kept,
// and answered, until the next is due, so a dashboard outside any pane
// does not ask herdr every poll. It takes at most half of what is left of
// ctx's time: a herdr that hangs then leaves the daemon the rest, and the
// footer blames herdr, not the daemon.
func (s *TeamSource) locate(ctx context.Context, now time.Time) {
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Until(deadline)/2)
		defer cancel()
	}
	s.place, s.err = s.Locator.Locate(ctx)
	s.at, s.searched = now, true
}

// ask is one snapshot op. Its errors are SnapshotSource's, each already the
// footer's one line naming what failed.
func (s *TeamSource) ask(ctx context.Context, sel daemon.Selector) (snapshot.Snapshot, error) {
	return daemon.SnapshotSource{Socket: s.Socket, Selector: sel}.Snapshot(ctx) //nolint:wrapcheck // the footer's line as SnapshotSource words it
}

func (s *TeamSource) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *TeamSource) every() time.Duration {
	if s.Every <= 0 {
		return RelocateEvery
	}
	return s.Every
}

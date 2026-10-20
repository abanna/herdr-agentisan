package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// The snapshot op's refusal codes.
const (
	// codeNotReady is the snapshot op's answer before the team poll has
	// built a snapshot of the team asked for.
	codeNotReady = "not_ready"
	// codeNoTeam is its answer for a team the model does not hold (A24).
	codeNoTeam = "no_team"
)

// maxSnapshot bounds an encoded snapshot: it must fit one answer line, with
// room for the {"ok":true,"data":…} around it.
const maxSnapshot = maxMessage - 1024

var (
	// ErrNotReady means the daemon has no snapshot of the team asked for
	// yet: its team poll has not finished a poll with agent names, or is
	// off, or the team's snapshot has never fit one answer line. A daemon
	// refusal with code not_ready matches it.
	ErrNotReady = errors.New("the daemon has no snapshot yet")
	// ErrNoTeam means the team model holds no team the snapshot op was
	// asked for: no project of that name, no team that workspace belongs
	// to, or, asked for neither, no ◆ space (A24). A daemon refusal with
	// code no_team matches it.
	ErrNoTeam = errors.New("no such team")
	// ErrSnapshotTooLarge means a snapshot would not fit one answer line, so
	// the team poll keeps serving the team's last one that did.
	ErrSnapshotTooLarge = errors.New("snapshot too large to serve")
)

// Selector picks the team the snapshot op answers with (ADR-001 A24): the
// project Project names, else the team whose workspaces include
// WorkspaceID, else the ◆ spaces' project. The daemon does the mapping, so
// a dashboard holds no team logic of its own.
type Selector struct {
	Project     string `json:"project,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// teams is one poll's view of every team: its snapshot, encoded, and which
// team each workspace belongs to. The cache swaps it whole, so the op never
// pairs one poll's index with another poll's snapshots.
type teams struct {
	// snaps holds every team in the model by project; nil for a team whose
	// snapshot has never fit one answer line.
	snaps map[string]json.RawMessage
	// teamOf is the project each team workspace belongs to.
	teamOf map[string]string
	// spaces is the ◆ spaces' project, which an op selecting nothing gets.
	spaces string
}

// snapshotCache is the snapshot op's state: the last poll's teams, each
// snapshot encoded once, so the op answers without calling herdr (D3).
type snapshotCache struct {
	mu  sync.Mutex
	cur *teams
}

// store serves every team in m from now on, each snapshot built at at with
// clock and then given to fill, when set, with its project (the GitHub
// header, A29), and spaces to an op that selects nothing. A team whose
// snapshot would not fit one answer line keeps its last one, and the others
// are unaffected: the error is ErrSnapshotTooLarge, naming each such team. A
// team gone from m is gone from the op.
func (c *snapshotCache) store(m team.Model, spaces string, at time.Time, clock *team.StageClock, fill func(project string, s *snapshot.Snapshot)) error {
	c.mu.Lock()
	prev := c.cur
	c.mu.Unlock()
	next := &teams{snaps: make(map[string]json.RawMessage, len(m.Projects)), teamOf: map[string]string{}, spaces: spaces}
	var errs []error
	for _, p := range m.Projects {
		snap := team.Snapshot(m, p.Name, at, clock)
		if fill != nil {
			fill(p.Name, &snap)
		}
		raw, err := encodeSnapshot(snap)
		if err != nil {
			errs = append(errs, fmt.Errorf("team %q: %w", p.Name, err))
			if prev != nil {
				raw = prev.snaps[p.Name]
			}
		}
		next.snaps[p.Name] = raw
		for _, ws := range p.Workspaces {
			next.teamOf[ws] = p.Name
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = next
	return errors.Join(errs...)
}

// encodeSnapshot is s as the op serves it, or ErrSnapshotTooLarge when that
// would not fit one answer line.
func encodeSnapshot(s snapshot.Snapshot) (json.RawMessage, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode the snapshot: %w", err)
	}
	if len(raw) > maxSnapshot {
		return nil, fmt.Errorf("%w: %d bytes, at most %d", ErrSnapshotTooLarge, len(raw), maxSnapshot)
	}
	return raw, nil
}

// load is the snapshot of the team sel selects: ErrNotReady before the
// first poll, or for a team whose snapshot has never fit, and ErrNoTeam for
// a team the last poll did not find.
func (c *snapshotCache) load(sel Selector) (json.RawMessage, error) {
	c.mu.Lock()
	t := c.cur
	c.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("%w: the team poll has not built one", ErrNotReady)
	}
	project := t.spaces
	switch {
	case sel.Project != "":
		project = sel.Project
	case sel.WorkspaceID != "":
		p, ok := t.teamOf[sel.WorkspaceID]
		if !ok {
			return nil, fmt.Errorf("%w: workspace %s is no team's", ErrNoTeam, sel.WorkspaceID)
		}
		project = p
	}
	raw, ok := t.snaps[project]
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: herdr shows no team %q", ErrNoTeam, project)
	case raw == nil:
		return nil, fmt.Errorf("%w: team %q is too large to serve", ErrNotReady, project)
	}
	return raw, nil
}

// snapshotCode is the refusal code the snapshot op answers err with.
func snapshotCode(err error) string {
	if errors.Is(err, ErrNoTeam) {
		return codeNoTeam
	}
	return codeNotReady
}

// Snapshot asks the daemon at socket for the ◆ spaces' team's snapshot:
// SnapshotOf, selecting nothing.
func Snapshot(ctx context.Context, socket string) (snapshot.Snapshot, error) {
	return SnapshotOf(ctx, socket, Selector{})
}

// SnapshotOf asks the daemon at socket for the snapshot of the team sel
// selects, decoded as strictly as snapshot.Decode decodes a file. Data that
// breaks the contract is ErrProtocol, wrapping snapshot's own sentinel; a
// team the daemon does not know is a refusal matching ErrNoTeam.
func SnapshotOf(ctx context.Context, socket string, sel Selector) (snapshot.Snapshot, error) {
	data, err := callArgs(ctx, socket, "snapshot", sel)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	s, err := snapshot.Decode(bytes.NewReader(data))
	if err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("%w: snapshot: %w", ErrProtocol, err)
	}
	return s, nil
}

// SnapshotSource is the dashboard's snapshot.Source on the daemon socket:
// each poll is one snapshot op, bounded by the caller's context and the
// socket's own timeout, so a missing or wedged daemon costs one poll and
// never the dashboard. Its errors are one line naming what failed:
// "daemon not running: …", "daemon starting: …", "daemon not answering: …".
type SnapshotSource struct {
	// Socket is the daemon's socket, Paths.Socket.
	Socket string
	// Selector picks the team; zero is the ◆ spaces' team.
	Selector Selector
}

// Snapshot asks the daemon for the selected team's snapshot.
func (s SnapshotSource) Snapshot(ctx context.Context) (snapshot.Snapshot, error) {
	snap, err := SnapshotOf(ctx, s.Socket, s.Selector)
	switch {
	case err == nil:
		return snap, nil
	case errors.Is(err, ErrNotReady):
		return snapshot.Snapshot{}, fmt.Errorf("daemon starting: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return snapshot.Snapshot{}, fmt.Errorf("daemon not answering: %w", err)
	case errors.Is(err, context.Canceled):
		return snapshot.Snapshot{}, fmt.Errorf("daemon snapshot cancelled: %w", err)
	case errors.Is(err, ErrUnavailable):
		return snapshot.Snapshot{}, fmt.Errorf("daemon not running: %w", err)
	default:
		return snapshot.Snapshot{}, fmt.Errorf("daemon snapshot: %w", err)
	}
}

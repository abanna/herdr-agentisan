package notice

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/team"
)

// A mute's length (A44): an hour unless the boss asks otherwise, and at
// most a day.
const (
	DefaultMute = time.Hour
	MaxMute     = 24 * time.Hour
)

var (
	// ErrUnknownWorker means no team's worker has the name.
	ErrUnknownWorker = errors.New("no team's worker has that name")
	// ErrMuteLength means a mute was asked for less than nothing or longer
	// than MaxMute.
	ErrMuteLength = errors.New("a mute lasts more than 0s and at most 24h")
)

// Mute is a worker muted until a time. Pane names the pane an unbound
// agent's mute was set on, so two mutes under one name can be told apart; a
// bound worker's has none.
type Mute struct {
	Worker string    `json:"worker"`
	Pane   string    `json:"pane,omitempty"`
	Until  time.Time `json:"until"`
}

// Mutes are the workers whose notices their boss does not want for a while
// (A44): every kind a team can skip, so never the context ceiling (D7).
// They are held in memory, so a restarted daemon has none. The socket's
// ops change them while the team poll reads them, so they are safe for
// concurrent use. A nil Mutes mutes no one.
type Mutes struct {
	mu      sync.Mutex
	entries map[muteScope]muteEntry
	resolve func(context.Context, string, team.Worker) (generation int64, bound bool, err error)
}

// A mute is stored under what it was set for (NERD-5730): a bound worker's
// row, by logical ID and generation, which follows the row through a handoff,
// or an unbound pane's name and terminal. Never the bare name, which a bound
// row and an unbound agent can share.
type muteScope struct {
	bound      bool
	name       string
	generation int64
	terminal   string
}

// scopeOf is what a notice about w, or a mute of w, is stored under.
func scopeOf(w team.Worker) muteScope {
	if w.Bound {
		return muteScope{bound: true, name: w.ID, generation: w.Generation}
	}
	return muteScope{name: WorkerName(w), terminal: w.Pane.TerminalID}
}

type muteEntry struct {
	pane  string
	until time.Time
}

// NewMutes is a Mutes with no one muted. When supplied, resolve verifies
// the selected worker against the live bindings and can link a replacement
// pane to its pending handoff's generation.
func NewMutes(resolve func(context.Context, string, team.Worker) (generation int64, bound bool, err error)) *Mutes {
	return &Mutes{entries: map[muteScope]muteEntry{}, resolve: resolve}
}

// selectWorker is the worker a name selects, for mute and unmute alike: a
// bound worker with that logical ID wins, else the first worker of m named
// so.
func selectWorker(m team.Model, name string) (team.Worker, bool) {
	workers := groupWorkers(m)
	i := slices.IndexFunc(workers, func(p placed) bool { return p.worker.Bound && p.worker.ID == name })
	if i < 0 {
		i = slices.IndexFunc(workers, func(p placed) bool { return WorkerName(p.worker) == name })
	}
	if i < 0 {
		return team.Worker{}, false
	}
	return workers[i].worker, true
}

// Mute mutes worker from now for d, DefaultMute when d is 0. worker is the
// WorkerName of a worker in one of m's groups: a name no worker has is
// ErrUnknownWorker, so a typo is never a mute that silences nothing.
// A bound logical ID takes precedence over a provisional name.
// The resolver also refuses a stale selection that would mute another row.
// Muting a worker again sets its end anew, and no other entry's.
func (s *Mutes) Mute(ctx context.Context, m team.Model, worker string, d time.Duration, now time.Time) (Mute, error) {
	switch {
	case d == 0:
		d = DefaultMute
	case d < 0 || d > MaxMute:
		return Mute{}, fmt.Errorf("%w: got %s", ErrMuteLength, d)
	}
	w, ok := selectWorker(m, worker)
	if !ok {
		return Mute{}, fmt.Errorf("%w: %q", ErrUnknownWorker, worker)
	}
	scope := scopeOf(w)
	until := now.Add(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Retire and confirmed loss clear the mute after changing the row, under
	// this same lock. Check here so an old model cannot restore their mute.
	if s.resolve != nil {
		generation, bound, err := s.resolve(ctx, worker, w)
		if err != nil {
			return Mute{}, fmt.Errorf("check mute for worker %q: %w", worker, err)
		}
		if bound {
			scope = muteScope{bound: true, name: worker, generation: generation}
		}
	}
	e := muteEntry{until: until}
	if !scope.bound {
		e.pane = w.Pane.ID
	}
	s.entries[scope] = e
	return Mute{Worker: worker, Pane: e.pane, Until: until}, nil
}

// ClearWorker clears this row's mute, and no other entry's, and reports
// whether it was active.
func (s *Mutes) ClearWorker(worker string, generation int64, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := muteScope{bound: true, name: worker, generation: generation}
	m, ok := s.entries[key]
	delete(s.entries, key)
	return ok && now.Before(m.until)
}

// Unmute ends the mute worker selects in m, as Mute selects it (the bound
// row wins), and reports whether one was in force at now. It also ends every
// unbound mute under that name whose terminal no unbound worker of m carries:
// a closed pane's, or one that is now a row's, which nothing else could end.
// A namesake still in m keeps its own. With no model there is nothing to
// select from, so the caller refuses. A worker with none is no refusal.
func (s *Mutes) Unmute(m team.Model, worker string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	named := func(keep func(muteScope) bool) []muteScope {
		return slices.DeleteFunc(slices.Collect(maps.Keys(s.entries)), func(k muteScope) bool { return k.name != worker || !keep(k) })
	}
	keys := named(func(k muteScope) bool { return k.bound })
	if w, found := selectWorker(m, worker); found && !w.Bound {
		if _, held := s.entries[scopeOf(w)]; held {
			keys = []muteScope{scopeOf(w)}
		}
	}
	carried := map[string]bool{}
	for _, p := range groupWorkers(m) {
		carried[p.worker.Pane.TerminalID] = carried[p.worker.Pane.TerminalID] || !p.worker.Bound
	}
	keys = append(keys, named(func(k muteScope) bool { return !k.bound && !carried[k.terminal] })...)
	var inForce bool
	for _, k := range keys {
		inForce = inForce || now.Before(s.entries[k].until)
		delete(s.entries, k)
	}
	return inForce
}

// List is every mute in force at now, by worker name, a bound worker's
// first; it forgets those that have ended.
func (s *Mutes) List(now time.Time) []Mute {
	s.mu.Lock()
	defer s.mu.Unlock()
	type row struct {
		k muteScope
		Mute
	}
	var rows []row
	for k, e := range s.entries {
		if now.Before(e.until) {
			rows = append(rows, row{k, Mute{Worker: k.name, Pane: e.pane, Until: e.until}})
		} else {
			delete(s.entries, k)
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Or(cmp.Compare(a.Worker, b.Worker), -cmp.Compare(btoi(a.k.bound), btoi(b.k.bound)), cmp.Compare(a.Pane, b.Pane), cmp.Compare(a.k.terminal, b.k.terminal))
	})
	out := make([]Mute, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Mute)
	}
	return out
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Muted reports whether n is silenced at now: it is about a worker muted
// until after now, and of a kind a team can skip.
func (s *Mutes) Muted(n Notice, now time.Time) bool {
	if s == nil || n.Worker == "" || !n.Kind.Skippable() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.entries[n.scope]
	return ok && now.Before(m.until)
}

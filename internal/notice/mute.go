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

// Mute is a worker muted until a time.
type Mute struct {
	Worker string    `json:"worker"`
	Until  time.Time `json:"until"`
}

// Mutes are the workers whose notices their boss does not want for a while
// (A44): every kind a team can skip, so never the context ceiling (D7).
// Only a worker bound to a row can be muted (NERD-5730). They are held in
// memory, so a restarted daemon has none. The socket's ops change them while
// the team poll reads them, so they are safe for concurrent use. A nil Mutes
// mutes no one.
type Mutes struct {
	mu      sync.Mutex
	entries map[muteScope]muteEntry
	ending  map[muteScope]bool // rows a retire holds (Hold)
	live    func(ctx context.Context, worker string) (generation int64, err error)
}

// muteScope is the row a mute was set for: its logical ID and generation,
// which a handoff keeps and a reused ID does not.
type muteScope struct {
	name       string
	generation int64
}

// scopeOf is what a notice about w is muted under: w's row, or nothing for
// an unbound pane, which no mute names.
func scopeOf(w team.Worker) muteScope {
	if !w.Bound {
		return muteScope{}
	}
	return muteScope{name: w.ID, generation: w.Generation}
}

type muteEntry struct {
	until    time.Time
	departed bool // its row ended: only its Gone passes, and nothing lists or ends it
}

// NewMutes is a Mutes with no one muted. When supplied, live gives a row's
// current generation from the bindings, so a mute from a stale model is
// refused.
func NewMutes(live func(ctx context.Context, worker string) (generation int64, err error)) *Mutes {
	return &Mutes{entries: map[muteScope]muteEntry{}, ending: map[muteScope]bool{}, live: live}
}

// selectWorker is the bound worker of m with logical ID name. Any other worker
// of that name is an unbound agent, which cannot be muted.
func selectWorker(m team.Model, name string) (team.Worker, error) {
	workers := groupWorkers(m)
	if i := slices.IndexFunc(workers, func(p placed) bool { return p.worker.Bound && p.worker.ID == name }); i >= 0 {
		return workers[i].worker, nil
	}
	if slices.ContainsFunc(workers, func(p placed) bool { return WorkerName(p.worker) == name }) {
		return team.Worker{}, fmt.Errorf("%w: %q is no bound worker's logical id; only a worker bound to a row can be muted", ErrUnknownWorker, name)
	}
	return team.Worker{}, fmt.Errorf("%w: %q", ErrUnknownWorker, name)
}

// Mute mutes worker from now for d, DefaultMute when d is 0. worker is the
// logical ID of a bound worker in one of m's groups: any other name is
// ErrUnknownWorker, so a typo is never a mute that silences nothing, and an
// unbound agent is refused. The bindings also refuse a stale selection whose
// row has changed. Muting a worker again sets its end anew.
func (s *Mutes) Mute(ctx context.Context, m team.Model, worker string, d time.Duration, now time.Time) (Mute, error) {
	switch {
	case d == 0:
		d = DefaultMute
	case d < 0 || d > MaxMute:
		return Mute{}, fmt.Errorf("%w: got %s", ErrMuteLength, d)
	}
	w, err := selectWorker(m, worker)
	if err != nil {
		return Mute{}, err
	}
	until := now.Add(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Retire and confirmed loss end the row's mute under this same lock. Check
	// here so an old model cannot restore a mute after them.
	if s.live != nil {
		generation, err := s.live(ctx, worker)
		if err != nil {
			return Mute{}, fmt.Errorf("check mute for worker %q: %w", worker, err)
		}
		if generation != w.Generation {
			return Mute{}, fmt.Errorf("%w: %q is bound to another row now", ErrUnknownWorker, worker)
		}
	}
	scope := scopeOf(w)
	if s.ending[scope] {
		return Mute{}, fmt.Errorf("%w: %q is being retired", ErrUnknownWorker, worker)
	}
	s.entries[scope] = muteEntry{until: until}
	return Mute{Worker: worker, Until: until}, nil
}

// Hold lets a bound row's Gone notice through its mute, and refuses a new
// mute of the row, until release (NERD-5730): a retire holds it while it ends
// the row, so no poll drops the departure and no mute outlives the row. A
// retire that fails releases it and the mute stands.
func (s *Mutes) Hold(worker string, generation int64) (release func()) {
	k := muteScope{name: worker, generation: generation}
	s.mu.Lock()
	s.ending[k] = true
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.ending, k)
		s.mu.Unlock()
	}
}

// ClearWorker ends this row's mute, and no other entry's, and reports
// whether it was active. The row's Gone notice passes from then on, and every
// other kind stays muted until the mute would have ended.
func (s *Mutes) ClearWorker(worker string, generation int64, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := muteScope{name: worker, generation: generation}
	m, ok := s.entries[key]
	active := ok && !m.departed && now.Before(m.until)
	if ok {
		// Kept, not deleted: a notice a poll sampled under the mute stays muted.
		m.departed = true
		s.entries[key] = m
	}
	return active
}

// Active reports whether the row's mute is in force at now.
func (s *Mutes) Active(worker string, generation int64, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.entries[muteScope{name: worker, generation: generation}]
	return ok && !m.departed && now.Before(m.until)
}

// Unmute ends worker's mute, and reports whether one was in force at now.
func (s *Mutes) Unmute(worker string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var inForce bool
	for k, e := range s.entries {
		if k.name == worker && !e.departed {
			inForce = inForce || now.Before(e.until)
			delete(s.entries, k)
		}
	}
	return inForce
}

// List is every mute in force at now, by worker name; it forgets those
// that have ended.
func (s *Mutes) List(now time.Time) []Mute {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Mute{}
	byRow := func(a, b muteScope) int {
		return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.generation, b.generation))
	}
	for _, k := range slices.SortedFunc(maps.Keys(s.entries), byRow) {
		switch e := s.entries[k]; {
		case !now.Before(e.until):
			delete(s.entries, k)
		case !e.departed:
			out = append(out, Mute{Worker: k.name, Until: e.until})
		}
	}
	return out
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
	return ok && (n.Kind != Gone || !s.ending[n.scope] && !m.departed) && now.Before(m.until)
}

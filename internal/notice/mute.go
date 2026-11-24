package notice

import (
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
// They are held in memory, so a restarted daemon has none. The socket's
// ops change them while the team poll reads them, so they are safe for
// concurrent use. A nil Mutes mutes no one.
type Mutes struct {
	mu      sync.Mutex
	entries map[string]muteEntry
	resolve func(context.Context, string, team.Worker) (generation int64, bound bool, err error)
}

// A logical ID and its generation identify the row of a bound worker
// (NERD-5730). An unbound worker's mute uses only its provisional name.
type muteScope struct {
	bound      bool
	generation int64
}

type muteEntry struct {
	scope muteScope
	until time.Time
}

// NewMutes is a Mutes with no one muted. When supplied, resolve verifies
// the selected worker against the live bindings and can link a replacement
// pane to its pending handoff's generation.
func NewMutes(resolve func(context.Context, string, team.Worker) (generation int64, bound bool, err error)) *Mutes {
	return &Mutes{entries: map[string]muteEntry{}, resolve: resolve}
}

// Mute mutes worker from now for d, DefaultMute when d is 0. worker is the
// WorkerName of a worker in one of m's groups: a name no worker has is
// ErrUnknownWorker, so a typo is never a mute that silences nothing.
// The resolver also refuses a stale selection that would mute another row.
// Muting a worker again sets its end anew.
func (s *Mutes) Mute(ctx context.Context, m team.Model, worker string, d time.Duration, now time.Time) (Mute, error) {
	switch {
	case d == 0:
		d = DefaultMute
	case d < 0 || d > MaxMute:
		return Mute{}, fmt.Errorf("%w: got %s", ErrMuteLength, d)
	}
	workers := groupWorkers(m)
	i := slices.IndexFunc(workers, func(p placed) bool { return WorkerName(p.worker) == worker })
	if i < 0 {
		return Mute{}, fmt.Errorf("%w: %q", ErrUnknownWorker, worker)
	}
	w := workers[i].worker
	scope := muteScope{bound: w.Bound, generation: w.Generation}
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
		scope = muteScope{bound: bound, generation: generation}
	}
	s.entries[worker] = muteEntry{scope: scope, until: until}
	return Mute{Worker: worker, Until: until}, nil
}

// ClearWorker clears this generation's mute and reports whether it was active.
func (s *Mutes) ClearWorker(worker string, generation int64, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.entries[worker]
	if m.scope != (muteScope{bound: true, generation: generation}) {
		return false
	}
	delete(s.entries, worker)
	return now.Before(m.until)
}

// Unmute ends worker's mute, and reports whether one was in force at now.
func (s *Mutes) Unmute(worker string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.entries[worker]
	delete(s.entries, worker)
	return ok && now.Before(m.until)
}

// List is every mute in force at now, by worker name; it forgets those
// that have ended.
func (s *Mutes) List(now time.Time) []Mute {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Mute{}
	for _, w := range slices.Sorted(maps.Keys(s.entries)) {
		if m := s.entries[w]; now.Before(m.until) {
			out = append(out, Mute{Worker: w, Until: m.until})
		} else {
			delete(s.entries, w)
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
	m, ok := s.entries[n.Worker]
	return ok && m.scope == n.scope && now.Before(m.until)
}

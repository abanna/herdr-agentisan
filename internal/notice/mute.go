package notice

import (
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
	mu    sync.Mutex
	until map[string]time.Time
}

// NewMutes is a Mutes with no one muted.
func NewMutes() *Mutes {
	return &Mutes{until: map[string]time.Time{}}
}

// Mute mutes worker from now for d, DefaultMute when d is 0. worker is the
// WorkerName of a worker in one of m's groups: a name no worker has is
// ErrUnknownWorker, so a typo is never a mute that silences nothing.
// Muting a worker again sets its end anew.
func (s *Mutes) Mute(m team.Model, worker string, d time.Duration, now time.Time) (Mute, error) {
	switch {
	case d == 0:
		d = DefaultMute
	case d < 0 || d > MaxMute:
		return Mute{}, fmt.Errorf("%w: got %s", ErrMuteLength, d)
	}
	if !slices.ContainsFunc(groupWorkers(m), func(p placed) bool { return WorkerName(p.worker) == worker }) {
		return Mute{}, fmt.Errorf("%w: %q", ErrUnknownWorker, worker)
	}
	until := now.Add(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.until[worker] = until
	return Mute{Worker: worker, Until: until}, nil
}

// Unmute ends worker's mute, and reports whether one was in force at now.
func (s *Mutes) Unmute(worker string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.until[worker]
	delete(s.until, worker)
	return ok && now.Before(until)
}

// List is every mute in force at now, by worker name; it forgets those
// that have ended.
func (s *Mutes) List(now time.Time) []Mute {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Mute{}
	for _, w := range slices.Sorted(maps.Keys(s.until)) {
		if until := s.until[w]; now.Before(until) {
			out = append(out, Mute{Worker: w, Until: until})
		} else {
			delete(s.until, w)
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
	until, ok := s.until[n.Worker]
	return ok && now.Before(until)
}

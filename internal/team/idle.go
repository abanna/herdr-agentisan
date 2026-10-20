package team

import (
	"sync"
	"time"
)

// IdleClock remembers since when each pane has been idle or done, across
// polls (ADR-001 A23): herdr reports a pane's status but not since when. The
// daemon's poll observes every model it builds; `team status` asks. Idle and
// done are one rest, so a pane going from one to the other keeps its start.
// It is safe for concurrent use, and a nil clock knows nothing.
type IdleClock struct {
	mu    sync.Mutex
	since map[string]time.Time
}

// resting reports whether a pane is at rest.
func resting(p Pane) bool { return p.Status == StatusIdle || p.Status == StatusDone }

// Observe records m at now: a pane newly at rest starts resting at now; one
// still at rest keeps its start; one working, blocked or unknown, or not in
// m, is forgotten, so its next rest starts again.
func (c *IdleClock) Observe(m Model, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := map[string]time.Time{}
	note := func(p Pane) {
		if !resting(p) {
			return
		}
		if since, ok := c.since[p.ID]; ok {
			next[p.ID] = since
			return
		}
		next[p.ID] = now
	}
	for _, pr := range m.Projects {
		for _, w := range pr.Boss {
			note(w.Pane)
		}
		for _, g := range pr.Groups {
			for _, w := range g.Workers {
				note(w.Pane)
			}
		}
	}
	c.since = next
}

// Since is when the pane started resting, and whether this clock has seen it
// at rest. After a daemon restart it is no earlier than the first poll.
func (c *IdleClock) Since(paneID string) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	since, ok := c.since[paneID]
	return since, ok
}

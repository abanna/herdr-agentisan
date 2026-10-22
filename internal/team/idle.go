package team

import (
	"sync"
	"time"
)

// IdleClock remembers since when each pane has been idle or done, across
// polls (ADR-001 A23): herdr reports a pane's status but not since when. The
// daemon's poll observes every model it builds; `team status` asks. Idle and
// done are one rest, so a pane going from one to the other keeps its start.
// It also remembers each pane's latest working stretch, which tells an old
// stage from a current one (Stretch, A42). It is safe for concurrent use,
// and a nil clock knows nothing.
type IdleClock struct {
	mu    sync.Mutex
	since map[string]time.Time
	turns map[string]turn
}

// turn is what the clock knows of one pane's working stretches.
type turn struct {
	// rest is the last poll that saw the pane at rest, and restStage the
	// stage it held then; rest is zero when the clock first saw it at work.
	rest      time.Time
	restStage string
	// rests counts the polls in a row that saw the pane at rest; it is zero
	// from the poll that sees the pane working or blocked until the one that
	// sees it at rest again.
	rests int
	// from is when the latest stretch the clock saw begin began: the last
	// poll that saw the pane at rest before it, since the stretch began
	// somewhere between that poll and the next. stage is the stage the pane
	// held then. from is zero until the clock sees a rest end.
	from  time.Time
	stage string
}

// settled is how many polls in a row must see a pane at rest before the
// clock calls its stretch ended: the second, not the first, since a Stop
// hook's report can land just after the poll that first sees the turn's end
// (A42), and judging it at that poll would flash its stage old for a poll.
const settled = 2

// resting reports whether a pane is at rest.
func resting(p Pane) bool { return p.Status == StatusIdle || p.Status == StatusDone }

// Observe records m at now: a pane newly at rest starts resting at now; one
// still at rest keeps its start; one working, blocked or unknown, or not in
// m, is forgotten, so its next rest starts again. A pane at rest for a
// second poll after it worked has ended a stretch, and one working or
// blocked after a rest has begun one; one unknown, or not in m, is forgotten
// there too, so the clock times a stretch only when it saw both its ends.
func (c *IdleClock) Observe(m Model, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := map[string]time.Time{}
	turns := map[string]turn{}
	note := func(p Pane) {
		if t, ok := c.turns[p.ID].next(p, now); ok {
			turns[p.ID] = t
		}
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
	c.since, c.turns = next, turns
}

// next is t once p is seen at now, and false when the clock forgets p. A
// zero t is a pane the clock has not seen.
func (t turn) next(p Pane, now time.Time) (turn, bool) {
	switch {
	case resting(p):
		t.rest, t.restStage, t.rests = now, p.Stage, t.rests+1
	case p.Status == StatusWorking || p.Status == StatusBlocked:
		// rest holds still while the pane works, so every poll of a stretch
		// finds the same start.
		if !t.rest.IsZero() {
			t.from, t.stage = t.rest, t.restStage
		}
		t.rests = 0
	default:
		return turn{}, false
	}
	return t, true
}

// Stretch is when the pane's latest working stretch began, as far as this
// clock has seen, and the stage the pane held as it began. ok is false
// unless the clock saw that stretch begin, after a rest, and end: the pane
// was at rest at the last settled polls, two in a row. After a daemon
// restart a pane needs a rest, a stretch and two polls at rest again.
func (c *IdleClock) Stretch(paneID string) (from time.Time, stage string, ok bool) {
	if c == nil {
		return time.Time{}, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, known := c.turns[paneID]
	if !known || t.rests < settled || t.from.IsZero() {
		return time.Time{}, "", false
	}
	return t.from, t.stage, true
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

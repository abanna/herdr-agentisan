package team_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/team"
)

// modelOf is one project whose boss and one group hold panes at statuses.
func modelOf(boss team.Status, workers map[string]team.Status) team.Model {
	g := team.Group{Name: "coder"}
	for id, st := range workers {
		g.Workers = append(g.Workers, team.Worker{ID: id, Pane: team.Pane{ID: id, Status: st}})
	}
	return team.Model{Projects: []team.Project{{
		Name:   "ha",
		Boss:   []team.Worker{{ID: "boss", Pane: team.Pane{ID: "w14:p1", Status: boss}}},
		Groups: []team.Group{g},
	}}}
}

// TestIdleClock: a pane's rest starts the first poll it is seen idle or done
// and holds across idle and done; working, blocked, unknown or leaving the
// model ends it, and the next rest starts again. The boss is timed too.
func TestIdleClock(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	var c team.IdleClock

	c.Observe(modelOf(team.StatusIdle, map[string]team.Status{"a": team.StatusIdle, "b": team.StatusWorking, "c": team.StatusDone}), at(0))
	c.Observe(modelOf(team.StatusWorking, map[string]team.Status{"a": team.StatusDone, "b": team.StatusIdle, "c": team.StatusBlocked}), at(3))
	c.Observe(modelOf(team.StatusIdle, map[string]team.Status{"a": team.StatusIdle, "b": team.StatusIdle, "c": team.StatusIdle, "d": team.StatusUnknown}), at(6))

	for pane, want := range map[string]any{"a": at(0), "b": at(3), "c": at(6), "w14:p1": at(6), "d": nil, "nope": nil} {
		got, ok := c.Since(pane)
		if want == nil {
			assert.False(t, ok, pane)
			continue
		}
		assert.True(t, ok, pane)
		assert.Equal(t, want, got, pane)
	}

	c.Observe(modelOf(team.StatusIdle, map[string]team.Status{}), at(9))
	_, ok := c.Since("a")
	assert.False(t, ok, "a pane gone from the model is forgotten")
	c.Observe(modelOf(team.StatusIdle, map[string]team.Status{"a": team.StatusIdle}), at(12))
	got, _ := c.Since("a")
	assert.Equal(t, at(12), got, "and its next rest starts again")
}

// TestIdleClockNilAndConcurrent: a nil clock knows nothing and observes
// nothing; the poll observing while a status op asks is safe (-race).
func TestIdleClockNilAndConcurrent(t *testing.T) {
	t.Parallel()
	var nilClock *team.IdleClock
	nilClock.Observe(modelOf(team.StatusIdle, nil), time.Now())
	_, ok := nilClock.Since("w14:p1")
	assert.False(t, ok)

	var c team.IdleClock
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() { c.Observe(modelOf(team.StatusIdle, nil), time.Unix(int64(i), 0)) })
		wg.Go(func() { c.Since("w14:p1") })
	}
	wg.Wait()
	_, ok = c.Since("w14:p1")
	assert.True(t, ok)
}

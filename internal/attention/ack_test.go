package attention_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/attention"
)

// acked is s acknowledged last at ack.
func acked(s attention.State, ack time.Duration) attention.State {
	s.LastAck = at(ack)
	return s
}

// TestAckIsAReaction (A74, rule 3): the user acknowledging is a reaction,
// as a read is: with old posts left, the count resets and the next alert
// comes Every after the acknowledgement, not at rule 4's next gap.
func TestAckIsAReaction(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		if d < 30*m {
			return posts(2, 0)
		}
		return acked(posts(1, m), 30*m)
	}
	got := run(defaults(), 50*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 35, 40, 50), times(got))
	assert.Equal(t, []int{1, 2, 3, 1, 2, 3}, ns(got))
}

// TestPartialAckKeepsTheEpisode (A74): acknowledging one of three posts
// leaves the episode going for the other two; a new episode would alert
// at once, the two being past After.
func TestPartialAckKeepsTheEpisode(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		if d < 30*m {
			return posts(3, 0)
		}
		return acked(posts(2, m), 30*m)
	}
	got := run(defaults(), 40*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 35, 40), times(got))
}

func TestAckQuietIntervalSurvivesRestart(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	state := acked(posts(1, 0), 8*m)
	_, ok := tr.Observe("app", defaults(), state, at(10*m))
	require.False(t, ok, "an acknowledgement at 8 quiets the reminder until 13")
	a, ok := tr.Observe("app", defaults(), state, at(13*m))
	require.True(t, ok, "the remaining post is due when the quiet interval ends")
	assert.Equal(t, 1, a.N)
}

func TestOpeningAckTimeIsCappedAtThePoll(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	state := acked(posts(1, 0), 20*m)
	var alerts []time.Duration
	for d := 10 * m; d <= 30*m; d += m {
		if _, ok := tr.Observe("app", defaults(), state, at(d)); ok {
			alerts = append(alerts, d)
			tr.Shown("app", true, at(d))
		}
	}
	assert.Equal(t, mins(15, 20, 30), alerts)
}

func TestAckQuietIntervalSurvivesAnEmptyBoard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                  string
		before, during, after attention.State
	}{
		{"partial acknowledgement then append", postsWithHeld(), posts(1, 0), postsWithHeld()},
		{"full acknowledgement then append", held(1, 0), boss(), held(1, 0)},
		{"full acknowledgement then drain", held(1, 0), boss(), posts(1, 0)},
		{"full acknowledgement then worker return", held(1, 0), boss(), held(1, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			state := func(d time.Duration) attention.State {
				switch {
				case d < 12*m:
					return c.before
				case d < 13*m:
					return acked(c.during, 12*m)
				default:
					return acked(c.after, 12*m)
				}
			}
			assert.Equal(t, mins(10, 17), times(run(defaults(), 20*m, m, state, nil)))
		})
	}
}

func TestBossReadQuietIntervalEndsWithTheEpisode(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		switch {
		case d < 12*m:
			return posts(1, 0)
		case d < 13*m:
			s := boss()
			s.BossMark = 1
			return s
		default:
			s := held(1, 0)
			s.BossMark = 1
			return s
		}
	}
	assert.Equal(t, mins(10, 13, 18), times(run(defaults(), 20*m, m, state, nil)))
}

func postsWithHeld() attention.State {
	s := posts(1, 0)
	s.Held, s.OldestHeld = 1, at(0)
	return s
}

func TestNewPostAfterAckKeepsItsFirstAlertTime(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		if d < 13*m {
			return acked(boss(), 12*m)
		}
		return acked(posts(1, 13*m), 12*m)
	}
	assert.Equal(t, mins(23), times(run(defaults(), 25*m, m, state, nil)))
}

func TestNewPostDueInsideTheAckQuietIntervalWaits(t *testing.T) {
	t.Parallel()
	cfg := defaults()
	cfg.After = m
	state := func(d time.Duration) attention.State {
		if d < 13*m {
			return acked(boss(), 12*m)
		}
		return acked(posts(1, 13*m), 12*m)
	}
	assert.Equal(t, mins(17), times(run(cfg, 20*m, m, state, nil)))
}

func TestCeilingBoundCanAlertDuringTheQuietInterval(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	cfg := defaults()
	_, ok := tr.Observe("app", cfg, acked(boss(), 12*m), at(12*m))
	require.False(t, ok)
	state := acked(ceilingPost(boss(), 13*m), 12*m)
	_, ok = tr.Observe("app", cfg, state, at(13*m))
	require.False(t, ok)
	a, ok := tr.Observe("app", cfg, state, at(15*m))
	require.True(t, ok, "a ceiling bound at 15 can fire before the quiet interval ends at 17")
	assert.True(t, a.Ceiling)
}

func TestAcknowledgedCeilingCanAlertAgainInsideTheQuietInterval(t *testing.T) {
	t.Parallel()
	heldCeiling := func(newest time.Duration) attention.State {
		s := held(1, 0)
		s.OldestHeldCeiling, s.NewestCeiling = at(0), at(newest)
		return s
	}
	withPost := func(s attention.State) attention.State {
		s.Pending, s.Oldest = 1, at(0)
		return s
	}
	cases := []struct {
		name                  string
		before, during, after attention.State
		alerts                []time.Duration
	}{
		{"partial acknowledgement then append", withPost(heldCeiling(0)), posts(1, 0), withPost(heldCeiling(13 * m)), mins(2, 7, 15, 20)},
		{"full acknowledgement then append", heldCeiling(0), boss(), heldCeiling(13 * m), mins(2, 7, 13, 18)},
		{"full acknowledgement then drain", heldCeiling(0), boss(), ceilingPost(boss(), 0), mins(2, 7, 13, 18)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			state := func(d time.Duration) attention.State {
				switch {
				case d < 12*m:
					return c.before
				case d < 13*m:
					return acked(c.during, 12*m)
				default:
					return acked(c.after, 12*m)
				}
			}
			assert.Equal(t, c.alerts, times(run(defaults(), 20*m, m, state, nil)))
		})
	}
}

func TestDrainedCeilingCountsAgainDuringTheAckQuietInterval(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ordinary attention.State
		every    time.Duration
		alerts   []time.Duration
	}{
		{"partial acknowledgement", posts(1, 0), 5 * m, mins(10, 15)},
		{"full acknowledgement", boss(), 5 * m, mins(10, 13)},
		{"partial acknowledgement with hourly reminders", posts(1, 0), time.Hour, mins(10, 15)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaults()
			cfg.Every = c.every
			cfg.MaxEvery = max(cfg.MaxEvery, c.every)
			state := func(d time.Duration) attention.State {
				switch {
				case d < 12*m:
					s := c.ordinary
					s.Held, s.OldestHeld = 1, at(0)
					if d >= 11*m {
						s.OldestHeldCeiling, s.NewestCeiling = at(0), at(11*m)
					}
					return s
				case d < 13*m:
					return acked(c.ordinary, 12*m)
				default:
					// A drain posts the ceiling at the held row's creation time.
					return acked(ceilingPost(c.ordinary, 0), 12*m)
				}
			}
			assert.Equal(t, c.alerts, times(run(cfg, 17*m, m, state, nil)))
		})
	}
}

// TestEarlierAckIsNoReaction (A74): a LastAck earlier than the last one
// seen, a clock stepped back or the held record replaced at an earlier
// time, is no reaction: only a newer acknowledgement resets the count.
func TestEarlierAckIsNoReaction(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		switch {
		case d < 30*m:
			return posts(2, 0)
		case d < 45*m:
			return acked(posts(1, m), 30*m)
		}
		return acked(posts(1, m), 25*m)
	}
	got := run(defaults(), 50*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 35, 40, 50), times(got))
	assert.Equal(t, []int{1, 2, 3, 1, 2, 3}, ns(got), "the third after the reaction, not a first")
}

// TestAckIsTimedFromItsOwnTime (A74, R4 of 87ebad71): the next alert is due
// every after the acknowledgement (92094e96 §3), timed at the acknowledgement,
// not at the poll that first sees it; a boss's read in the same poll has
// no time of its own, so that reaction keeps the poll's, as does an
// acknowledgement stamped after the poll.
func TestAckIsTimedFromItsOwnTime(t *testing.T) {
	t.Parallel()
	cfg := defaults()
	opened := func() *attention.Tracker {
		tr := attention.NewTracker()
		_, ok := tr.Observe("ha", cfg, posts(2, 0), at(10*m))
		require.True(t, ok)
		tr.Shown("ha", true, at(10*m))
		return tr
	}
	t.Run("an acknowledgement seen late", func(t *testing.T) {
		t.Parallel()
		a, ok := opened().Observe("ha", cfg, acked(posts(1, 0), 11*m), at(20*m))
		require.True(t, ok, "acknowledged at 11, the rest due at 16: the first poll after, at 20, alerts")
		assert.Equal(t, 1, a.N)
	})
	t.Run("a boss's read in the same poll", func(t *testing.T) {
		t.Parallel()
		tr := opened()
		read := acked(posts(1, 0), 11*m)
		read.BossMark = 1
		_, ok := tr.Observe("ha", cfg, read, at(20*m))
		assert.False(t, ok, "the read is timed at 20")
		_, ok = tr.Observe("ha", cfg, read, at(25*m))
		assert.True(t, ok)
	})
	t.Run("an acknowledgement stamped after the poll", func(t *testing.T) {
		t.Parallel()
		tr := opened()
		ahead := acked(posts(1, 0), 25*m)
		_, ok := tr.Observe("ha", cfg, ahead, at(20*m))
		assert.False(t, ok, "timed at the poll, 20, never later")
		_, ok = tr.Observe("ha", cfg, ahead, at(25*m))
		assert.True(t, ok, "due 5 after 20")
	})
}

// TestAckedCeilingKeepsItsBound (A74, R5 of 87ebad71): a ceiling's bound,
// once set, holds when that ceiling is acknowledged and a later one
// remains: sooner, never later (ef66f8de).
func TestAckedCeilingKeepsItsBound(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	cfg := defaults()
	old := ceilingPost(posts(1, 0), 0)
	for _, d := range mins(2, 7) {
		_, ok := tr.Observe("ha", cfg, old, at(d))
		require.True(t, ok)
		tr.Shown("ha", true, at(d))
	}
	_, ok := tr.Observe("ha", cfg, ceilingPost(old, 11*m), at(11*m))
	require.False(t, ok, "the ceiling at 11 sets the bound at 13")
	remaining := acked(ceilingPost(posts(1, 0), 12*m), 12*m)
	_, ok = tr.Observe("ha", cfg, remaining, at(12*m))
	require.False(t, ok)
	_, ok = tr.Observe("ha", cfg, remaining, at(13*m))
	assert.True(t, ok, "the bound the acknowledged ceiling set, 13, not the remaining one's 14")
}

// TestDrainedCeilingKeepsItsBoundAcrossAnUnrelatedAck combines #184's drain
// with a separate board post acknowledged in the poll that sees the drain.
func TestDrainedCeilingKeepsItsBoundAcrossAnUnrelatedAck(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		if d >= 17*m {
			// Posting keeps the inbox row's creation time of 0, and the
			// acknowledgement removes only the unrelated board post.
			return acked(ceilingPost(boss(), 0), 17*m)
		}
		s := posts(1, 0)
		s.Held, s.OldestHeld = 1, at(0)
		if d >= 16*m {
			s.OldestHeldCeiling, s.NewestCeiling = at(0), at(16*m)
		}
		return s
	}
	got := run(defaults(), 22*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 18), times(got), "the armed deadline survives the drain and acknowledgement")
}

func TestAckedCeilingKeepsItsBoundWithOnlyAnOlderCeiling(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		old := ceilingPost(boss(), 0)
		switch {
		case d >= 17*m:
			return acked(old, 17*m)
		case d >= 16*m:
			return ceilingPost(old, 16*m)
		default:
			return old
		}
	}
	got := run(defaults(), 23*m, m, state, nil)
	assert.Equal(t, mins(2, 7, 18, 23), times(got), "the older pending ceiling can receive one earlier alert at 18")
}

func TestAckingTheOnlyPendingCeilingClearsItsBound(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		s := posts(1, 0)
		switch {
		case d >= 17*m:
			return acked(s, 17*m)
		case d >= 16*m:
			return ceilingPost(s, 16*m)
		default:
			return s
		}
	}
	got := run(defaults(), 22*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 22), times(got), "ordinary work follows the acknowledgement cadence")
}

func TestCeilingAfterQuietIntervalAlertGetsItsOwnDeadline(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		switch {
		case d < 12*m:
			return held(1, 0)
		case d < 15*m:
			return acked(boss(), 12*m)
		}
		s := acked(posts(1, 0), 12*m)
		if d >= 16*m {
			s.Held, s.OldestHeld = 1, at(16*m)
		}
		if d >= 17*m {
			// The reminder due at 17 covers the first line.
			s.OldestHeldCeiling, s.NewestCeiling = at(16*m), at(17*m)
		}
		if d >= 18*m {
			// The second line arrives after that alert consumed the bound.
			s.NewestCeiling = at(18 * m)
		}
		return s
	}
	assert.Equal(t, mins(10, 17, 20), times(run(defaults(), 20*m, m, state, nil)))
}

func TestReopenedOldestCeilingKeepsItsFirstAlertTime(t *testing.T) {
	t.Parallel()
	cfg := defaults()
	tr := attention.NewTracker()
	s := acked(posts(1, 0), 12*m)
	_, ok := tr.Observe("app", cfg, s, at(13*m))
	require.False(t, ok, "the ordinary reminder is quiet until 17")

	// A drain posts an old ceiling at the same time as a new ceiling
	// arrives. The old ceiling is already due and the new one's bound is
	// at 16.
	s = ceilingPost(ceilingPost(s, 0), 14*m)
	a, ok := tr.Observe("app", cfg, s, at(14*m))
	require.True(t, ok)
	assert.True(t, a.Ceiling)
}

func TestRestartedEpisodeKeepsTheOldestCeilingDeadline(t *testing.T) {
	t.Parallel()
	cfg := defaults()
	tr := attention.NewTracker()
	first := ceilingPost(boss(), 0)
	_, ok := tr.Observe("app", cfg, first, at(0))
	require.False(t, ok, "the first ceiling is due at 2")

	tr = attention.NewTracker()
	_, ok = tr.Observe("app", cfg, ceilingPost(first, m), at(m))
	require.False(t, ok, "a restart with a new ceiling does not alert before 2")

	// The second ceiling is acknowledged. The first is still pending
	// and reaches its original deadline in this poll.
	alert, ok := tr.Observe("app", cfg, acked(first, 2*m), at(2*m))
	require.True(t, ok, "the first ceiling is due even during the quiet interval")
	assert.True(t, alert.Ceiling)
}

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

// TestAckBeforeTheEpisodeIsNoReaction (A74): the first poll after a
// restart has a recent acknowledgement in state.db. Remember it without
// delaying the alert already due for the remaining post.
func TestAckBeforeTheEpisodeIsNoReaction(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	a, ok := tr.Observe("app", defaults(), acked(posts(1, 0), 8*m), at(10*m))
	require.True(t, ok, "the remaining post is due at 10 even though another was acknowledged at 8")
	assert.Equal(t, 1, a.N)
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
	_, ok := tr.Observe("ha", cfg, posts(1, 0), at(10*m))
	require.True(t, ok)
	tr.Shown("ha", true, at(10*m))
	_, ok = tr.Observe("ha", cfg, ceilingPost(posts(1, 0), 11*m), at(11*m))
	require.False(t, ok, "the ceiling at 11 sets the bound at 13")
	remaining := acked(ceilingPost(posts(1, 0), 12*m), 12*m)
	_, ok = tr.Observe("ha", cfg, remaining, at(12*m))
	require.False(t, ok)
	_, ok = tr.Observe("ha", cfg, remaining, at(13*m))
	assert.True(t, ok, "the bound the acknowledged ceiling set, 13, not the remaining one's 14")
}

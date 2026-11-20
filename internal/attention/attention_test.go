package attention_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/attention"
	"github.com/abanna/herdr-agentisan/internal/team"
)

const m = time.Minute

// t0 is minute 0 of every timeline.
var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// defaults is the user's decided config (106c51d8): reminders until someone
// reacts, the first after 10 minutes, after 30 while the boss is busy, and a
// ceiling after 2.
func defaults() attention.Config {
	return attention.Config{
		After: 10 * m, CeilingAfter: 2 * m, Every: 5 * m, MaxEvery: 20 * m, BusyGrace: 30 * m, Sound: "request",
	}
}

// boss is ha-boss in its place, idle, with claude showing, and nothing
// pending.
func boss() attention.State {
	return attention.State{Boss: "ha-boss", Status: team.StatusIdle, Agent: "claude", Place: true}
}

// posts is boss with n posts unread, the oldest from oldest.
func posts(n int, oldest time.Duration) attention.State {
	s := boss()
	s.Pending, s.Oldest = n, at(oldest)
	return s
}

// held is boss with n messages held in its inbox, the oldest from oldest.
func held(n int, oldest time.Duration) attention.State {
	s := boss()
	s.Held, s.OldestHeld = n, at(oldest)
	return s
}

// ceilingPost is s with one more unread post, a ceiling made at c, the
// newest ceiling; the oldest ceiling stays the older one.
func ceilingPost(s attention.State, c time.Duration) attention.State {
	s.Pending++
	if s.Oldest.IsZero() {
		s.Oldest = at(c)
	}
	if s.OldestCeiling.IsZero() {
		s.OldestCeiling = at(c)
	}
	s.NewestCeiling, s.NewestCeilingPost = at(c), at(c)
	return s
}

// try is one alert a timeline's Observe returned, at its offset from t0.
type try struct {
	at time.Duration
	a  attention.Alert
}

// run Observes project app from 0 to end every step, with the state state
// gives at each offset, and answers each alert Shown(shown(offset)): every
// one shown when shown is nil. It returns the alerts tried, in order.
func run(c attention.Config, end, step time.Duration, state func(time.Duration) attention.State, shown func(time.Duration) bool) []try {
	tr := attention.NewTracker()
	var out []try
	for d := time.Duration(0); d <= end; d += step {
		a, ok := tr.Observe("app", c, state(d), at(d))
		if !ok {
			continue
		}
		out = append(out, try{d, a})
		tr.Shown("app", shown == nil || shown(d), at(d))
	}
	return out
}

// times lists the offsets of tries.
func times(ts []try) []time.Duration {
	out := []time.Duration{}
	for _, t := range ts {
		out = append(out, t.at)
	}
	return out
}

// ns lists the tries' alert numbers.
func ns(ts []try) []int {
	out := []int{}
	for _, t := range ts {
		out = append(out, t.a.N)
	}
	return out
}

func mins(xs ...time.Duration) []time.Duration {
	out := []time.Duration{}
	for _, x := range xs {
		out = append(out, x*m)
	}
	return out
}

// steady is a state that never changes.
func steady(s attention.State) func(time.Duration) attention.State {
	return func(time.Duration) attention.State { return s }
}

// TestHeldMessagesAlertTheUser (M-B): messages held in the boss's inbox open
// an episode as posts do, with the held title.
func TestHeldMessagesAlertTheUser(t *testing.T) {
	t.Parallel()
	a, ok := attention.NewTracker().Observe("app", defaults(), held(1, 0), at(10*m))
	require.True(t, ok, "a message held 10 min alerts with no post pending")
	assert.Equal(t, "agentisan: 1 messages wait in ha-boss's inbox", a.Title)
}

func TestNoAlertBeforeAfter(t *testing.T) {
	t.Parallel()
	_, ok := attention.NewTracker().Observe("app", defaults(), posts(1, 0), at(10*m-time.Second))
	assert.False(t, ok, "a post 9:59 old")
}

func TestFirstAlertAtAfter(t *testing.T) {
	t.Parallel()
	a, ok := attention.NewTracker().Observe("app", defaults(), posts(1, 0), at(10*m))
	require.True(t, ok, "a post 10:00 old")
	assert.Equal(t, 1, a.N)
}

// TestAfterZeroIsOff: after 0 turns every alert off, a ceiling's included
// (rule 1 before rule 2, b5).
func TestAfterZeroIsOff(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.After = 0
	assert.Empty(t, run(c, 60*m, m, steady(posts(1, 0)), nil), "posts")

	state := func(d time.Duration) attention.State {
		if d < 5*m {
			return posts(1, 0)
		}
		return ceilingPost(posts(1, 0), 5*m)
	}
	assert.Empty(t, run(c, 60*m, m, state, nil), "a ceiling posted at 5 min never alerts with alerts off")
}

// TestRemindersDoubleToMaxEvery (rule 4): after the k-th shown alert the
// next waits min(every * 2^(k-1), max_every); no gap ever shrinks, however
// long the episode runs.
func TestRemindersDoubleToMaxEvery(t *testing.T) {
	t.Parallel()
	got := run(defaults(), 90*m, m, steady(posts(1, 0)), nil)
	assert.Equal(t, mins(10, 15, 25, 45, 65, 85), times(got))

	long := times(run(defaults(), 48*time.Hour, m, steady(posts(1, 0)), nil))
	require.Greater(t, len(long), 140, "an alert every 20 min for 48 h")
	for i := 5; i < len(long); i++ {
		require.Equal(t, 20*m, long[i]-long[i-1], "the gap after alert %d", i)
	}
}

// TestUntilReactionNeverStops (B4): with alerts 0 the reminders go on while
// anything is pending and no one reacts.
func TestUntilReactionNeverStops(t *testing.T) {
	t.Parallel()
	got := times(run(defaults(), 6*time.Hour, m, steady(posts(1, 0)), nil))
	require.NotEmpty(t, got)
	assert.Equal(t, 345*m, got[len(got)-1], "still alerting at 6 h: 65 min, then every 20")
	assert.Len(t, got, 19)
}

// TestAlertsLimitStops (rule 5): alerts > 0 stops the episode after that
// many shown alerts, and the last one says so.
func TestAlertsLimitStops(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.Alerts = 4
	got := run(c, 6*time.Hour, m, steady(posts(1, 0)), nil)
	require.Equal(t, mins(10, 15, 25, 45), times(got))
	for i, tr := range got {
		assert.Equal(t, 4, tr.a.Of)
		assert.Equal(t, i == 3, tr.a.Last, "alert %d", i+1)
	}
	assert.Contains(t, got[3].a.Body, "; the last alert")
	assert.NotContains(t, got[2].a.Body, "the last alert")
}

// TestCeilingAlertsAtCeilingAfter (M5.1): a ceiling post alerts after
// ceiling_after, not after.
func TestCeilingAlertsAtCeilingAfter(t *testing.T) {
	t.Parallel()
	got := run(defaults(), 9*m, m, steady(ceilingPost(boss(), 0)), nil)
	assert.Equal(t, mins(2, 7), times(got))
}

// TestCeilingIgnoresBusyGrace (M5.1): busy_grace never holds a ceiling back.
func TestCeilingIgnoresBusyGrace(t *testing.T) {
	t.Parallel()
	s := ceilingPost(boss(), 0)
	s.Status = team.StatusWorking
	got := run(defaults(), 3*m, m, steady(s), nil)
	assert.Equal(t, mins(2), times(got))
}

// TestWorkingBossWaitsForBusyGrace: a boss mid-turn gets busy_grace for its
// first alert.
func TestWorkingBossWaitsForBusyGrace(t *testing.T) {
	t.Parallel()
	s := posts(1, 0)
	s.Status = team.StatusWorking
	got := run(defaults(), 30*m, m, steady(s), nil)
	assert.Equal(t, mins(30), times(got))
}

// TestNotWorkingBossAlertsAtAfter (gate 0, 16aea21c): only a boss mid-turn
// waits for busy_grace. herdr shows a boss whose board wait runs as done.
func TestNotWorkingBossAlertsAtAfter(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]attention.State{
		"idle":     {Status: team.StatusIdle, Agent: "claude"},
		"done":     {Status: team.StatusDone, Agent: "claude"},
		"blocked":  {Status: team.StatusBlocked, Agent: "codex"},
		"unknown":  {Status: team.StatusUnknown, Agent: "claude"},
		"no agent": {Status: team.StatusUnknown},
		// a6a11d95 rule 2: "no agent" counts at after, whatever status
		// herdr gives the pane.
		"no agent, herdr saying working": {Status: team.StatusWorking},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s.Boss, s.Place, s.Pending, s.Oldest = "ha-boss", true, 1, t0
			got := run(defaults(), 10*m, m, steady(s), nil)
			assert.Equal(t, mins(10), times(got))
		})
	}
}

// TestReadThenFreshPostWaitsForAfter (M-A): a read that leaves only a post
// made since waits for that post to age, as a first alert does, not for
// the reaction + every.
func TestReadThenFreshPostWaitsForAfter(t *testing.T) {
	t.Parallel()
	const read = 40 * m
	fresh := read + time.Second
	for name, tc := range map[string]struct {
		status team.Status
		wait   time.Duration
	}{
		"idle":    {team.StatusIdle, 10 * m},
		"working": {team.StatusWorking, 30 * m},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := func(d time.Duration) attention.State {
				s := posts(1, 0)
				if d >= fresh {
					s = posts(1, fresh)
					s.BossMark = 1
				}
				s.Status = tc.status
				return s
			}
			var after []time.Duration
			for _, d := range times(run(defaults(), fresh+tc.wait, time.Second, state, nil)) {
				if d >= fresh {
					after = append(after, d)
				}
			}
			assert.Equal(t, []time.Duration{fresh + tc.wait}, after)
		})
	}
}

// TestPartialReadDelaysButKeepsTheEpisode (M1, M-A): a read that leaves an
// old post resets the count and alerts again every after it.
func TestPartialReadDelaysButKeepsTheEpisode(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		if d < 41*m {
			return posts(2, 0)
		}
		s := posts(1, m)
		s.BossMark = 1
		return s
	}
	got := run(defaults(), 61*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 46, 51, 61), times(got))
	assert.Equal(t, []int{1, 2, 3, 1, 2, 3}, ns(got))
}

// TestPendingZeroEndsTheEpisode (rule 1): an episode ends when nothing is
// pending, here a held message delivered, with no read, and the next opens
// afresh. Its row empties the posts; a read that empties them is a reaction,
// which alone also gives 30, so only an episode emptied with no read can
// tell a kept episode, which alerts at 20, from a new one.
func TestPendingZeroEndsTheEpisode(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		switch {
		case d < 12*m:
			return held(1, 0)
		case d < 20*m:
			return boss()
		}
		return posts(1, 20*m)
	}
	got := run(defaults(), 34*m, m, state, nil)
	assert.Equal(t, mins(10, 30), times(got))
	assert.Equal(t, []int{1, 1}, ns(got))
}

// TestNewCeilingPastTheLimitAlertsOnce (rule 5): past a limit, a ceiling
// newer than the last shown alert gives one more alert; a held ceiling's
// time is its row's created_at, so a line appended to an older row gives
// none (b4).
func TestNewCeilingPastTheLimitAlertsOnce(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.Alerts = 2
	posted := func(d time.Duration) attention.State {
		if d < 20*m {
			return posts(1, 0)
		}
		return ceilingPost(posts(1, 0), 20*m)
	}
	got := run(c, 120*m, m, posted, nil)
	require.Equal(t, mins(10, 15, 22), times(got), "a ceiling posted at 20")
	// The boss's ruling (Q1, 2026-11-19): past the limit the body says so,
	// never "Alert 3 of 2", and not "the last alert", since a newer ceiling
	// can bring one more.
	assert.Contains(t, got[2].a.Body, ". Alert 3, past the limit of 2. Open the board")
	assert.NotContains(t, got[2].a.Body, "the last alert")
	assert.False(t, got[2].a.Last)

	appended := func(d time.Duration) attention.State {
		s := held(1, 0)
		if d >= 16*m {
			s.OldestHeldCeiling, s.NewestCeiling = at(0), at(16*m)
		}
		return s
	}
	assert.Equal(t, mins(10, 15), times(run(c, 60*m, m, appended, nil)), "a ceiling appended at 16 to a row from 0")
}

// TestLimitHoldsWhileAnOlderCeilingIsUnread (rule 5): past a limit, a held
// ceiling appended to a row older than the last alert gives none, since the
// row's newest time moves with every notice appended. A ceiling posted
// directly gives one (TestPostedWarningPastTheLimitAlertsOnce).
func TestLimitHoldsWhileAnOlderCeilingIsUnread(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.Alerts = 1
	twoCeilings := func(d time.Duration) attention.State {
		s := held(1, 0)
		s.OldestHeldCeiling, s.NewestCeiling = at(0), at(0)
		if d >= 10*m {
			s.NewestCeiling = at(10 * m)
		}
		return s
	}
	assert.Equal(t, mins(2), times(run(c, 60*m, m, twoCeilings, nil)), "a held ceiling from 0, another appended at 10")
}

// TestPostedWarningPastTheLimitAlertsOnce (rule 5, the user's decision of
// 2026-11-20): past a limit, each new ceiling posted directly gives one
// alert of its own, even while an older one is unread.
func TestPostedWarningPastTheLimitAlertsOnce(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.Alerts = 1
	state := func(d time.Duration) attention.State {
		s := posts(1, 0)
		if d >= 20*m {
			s = ceilingPost(s, 20*m)
		}
		if d >= 40*m {
			s = ceilingPost(s, 40*m)
		}
		return s
	}
	got := run(c, 90*m, m, state, nil)
	require.Equal(t, mins(10, 22, 42), times(got))
	assert.Contains(t, got[2].a.Body, ". Alert 3, past the limit of 1. Open the board")
}

// TestHeldWarningPastTheLimitStaysQuiet (rule 5): past a limit, a held
// ceiling with a notice appended every minute gives no alert; a ceiling
// posted while its bound is armed alerts at once.
func TestHeldWarningPastTheLimitStaysQuiet(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.Alerts = 1
	appended := func(d time.Duration) attention.State {
		s := unseen()
		if d >= 16*m {
			s.OldestHeldCeiling, s.NewestCeiling = at(0), at(d)
		}
		return s
	}
	assert.Equal(t, mins(10), times(run(c, 60*m, m, appended, nil)), "held")
	posted := func(d time.Duration) attention.State {
		if d < 30*m {
			return appended(d)
		}
		return ceilingPost(appended(d), 30*m)
	}
	assert.Equal(t, mins(10, 30), times(run(c, 60*m, m, posted, nil)), "a ceiling posted at 30")
}

// TestUnshownDoesNotCount (rule 6): an alert not shown is tried again every
// later, flat, with its number; once tried, a ceiling's bound no longer
// brings it, for that ceiling or an older one, a reaction included.
func TestUnshownDoesNotCount(t *testing.T) {
	t.Parallel()
	unshownAt := func(x time.Duration) func(time.Duration) bool {
		return func(d time.Duration) bool { return d != x }
	}
	got := run(defaults(), 21*m, m, steady(posts(1, 0)), unshownAt(10*m))
	assert.Equal(t, mins(10, 15, 20), times(got), "the first alert not shown")
	assert.Equal(t, []int{1, 1, 2}, ns(got))

	ceilingAt26 := func(d time.Duration) attention.State {
		if d < 26*m {
			return posts(1, 0)
		}
		return ceilingPost(posts(1, 0), 26*m)
	}
	got = run(defaults(), 40*m, m, ceilingAt26, unshownAt(28*m))
	assert.Equal(t, mins(10, 15, 25, 28, 33), times(got), "the ceiling's alert at 28 not shown")
	assert.Equal(t, []int{1, 2, 3, 4, 4}, ns(got))

	ceilingAt24 := func(d time.Duration) attention.State {
		if d < 24*m {
			return posts(1, 0)
		}
		return ceilingPost(posts(1, 0), 24*m)
	}
	got = run(defaults(), 31*m, m, ceilingAt24, unshownAt(25*m))
	assert.Equal(t, mins(10, 15, 25, 30), times(got), "rule 4's alert at 25 not shown, a ceiling from 24")

	readAt29 := func(d time.Duration) attention.State {
		if d < 29*m {
			return ceilingAt26(d)
		}
		s := ceilingPost(boss(), 26*m)
		s.BossMark = 1
		return s
	}
	got = run(defaults(), 35*m, m, readAt29, unshownAt(28*m))
	assert.Equal(t, mins(10, 15, 25, 28, 34), times(got), "then a read at 29 leaving the ceiling")
	assert.Equal(t, []int{1, 2, 3, 4, 1}, ns(got))

	tr := attention.NewTracker()
	tr.Shown("app", true, at(0))
	_, ok := tr.Observe("app", defaults(), posts(1, 0), at(9*m))
	require.False(t, ok)
	tr.Shown("app", true, at(9*m))
	a, ok := tr.Observe("app", defaults(), posts(1, 0), at(10*m))
	require.True(t, ok, "a Shown with no alert tried changes nothing")
	assert.Equal(t, 1, a.N)
}

// TestNoBossPlacePausesTheAlarm (M-D, rule 7): with no boss place no alert
// is shown, and the count and the due time are kept; an alert that fell due
// is shown at the first poll the place is back.
func TestNoBossPlacePausesTheAlarm(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		s := posts(1, 0)
		s.Place = d < 12*m || d >= 42*m
		return s
	}
	got := run(defaults(), 55*m, m, state, nil)
	assert.Equal(t, mins(10, 42, 52), times(got))
	assert.Equal(t, []int{1, 2, 3}, ns(got))
}

// TestEpisodesArePerProject: one project's reaction resets its own count
// alone.
func TestEpisodesArePerProject(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	last := map[string]attention.Alert{}
	for d := time.Duration(0); d <= 25*m; d += m {
		for _, project := range []string{"a", "b"} {
			s := posts(2, 0)
			if project == "a" && d >= 20*m {
				s = posts(1, m)
				s.BossMark = 1
			}
			if a, ok := tr.Observe(project, defaults(), s, at(d)); ok {
				last[project] = a
				tr.Shown(project, true, at(d))
			}
		}
	}
	assert.Equal(t, 1, last["a"].N, "a reacted at 20: its alert at 25 is its first again")
	assert.Equal(t, 3, last["b"].N, "b did not: its alert at 25 is its third")
}

// TestAlertText: the titles and bodies of "The text".
func TestAlertText(t *testing.T) {
	t.Parallel()
	limited := defaults()
	limited.Alerts = 4
	done := defaults()
	done.Sound = "done"
	withHeld := posts(1, 0)
	withHeld.Status, withHeld.Held, withHeld.OldestHeld = team.StatusDone, 2, at(3*m)
	noAgent := held(2, 0)
	noAgent.Status, noAgent.Agent = team.StatusUnknown, ""
	ceiling := ceilingPost(boss(), 0)
	ceiling.Status = team.StatusWorking
	blocked := posts(1, 0)
	blocked.Status = team.StatusBlocked
	unknown := posts(3, 0)
	unknown.Status = team.StatusUnknown
	for name, tc := range map[string]struct {
		c    attention.Config
		s    attention.State
		at   time.Duration
		nth  int
		want attention.Alert
	}{
		"posts only": {
			c: done, s: unknown, at: 10 * m, nth: 1,
			want: attention.Alert{
				Title: "agentisan: ha-boss has 3 unread on its board",
				Body:  "oldest 10 min; the boss is unknown. Alert 1. Open the board: prefix+d, then b",
				Sound: "done", N: 1,
			},
		},
		"a ceiling": {
			c: defaults(), s: ceiling, at: 2 * m, nth: 1,
			want: attention.Alert{
				Title: "agentisan: a ceiling waits unread for ha-boss",
				Body:  "oldest 2 min; the boss is working. Alert 1. Open the board: prefix+d, then b",
				Sound: "request", N: 1, Ceiling: true,
			},
		},
		"held only": {
			c: defaults(), s: noAgent, at: 10 * m, nth: 1,
			want: attention.Alert{
				Title: "agentisan: 2 messages wait in ha-boss's inbox",
				Body:  "oldest 10 min; the boss is showing no agent. Alert 1. Open the board: prefix+d, then b",
				Sound: "request", N: 1,
			},
		},
		"posts with held": {
			c: defaults(), s: withHeld, at: 10 * m, nth: 1,
			want: attention.Alert{
				Title: "agentisan: ha-boss has 1 unread on its board",
				Body:  "oldest 10 min; the boss is done; 2 messages wait in its inbox. Alert 1. Open the board: prefix+d, then b",
				Sound: "request", N: 1,
			},
		},
		"the third of four": {
			c: limited, s: blocked, at: 25 * m, nth: 3,
			want: attention.Alert{
				Title: "agentisan: ha-boss has 1 unread on its board",
				Body:  "oldest 25 min; the boss is blocked. Alert 3 of 4. Open the board: prefix+d, then b",
				Sound: "request", N: 3, Of: 4,
			},
		},
		"the last of four": {
			c: limited, s: posts(1, 0), at: 45 * m, nth: 4,
			want: attention.Alert{
				Title: "agentisan: ha-boss has 1 unread on its board",
				Body:  "oldest 45 min; the boss is idle. Alert 4 of 4. Open the board: prefix+d, then b; the last alert",
				Sound: "request", N: 4, Of: 4, Last: true,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := run(tc.c, tc.at, m, steady(tc.s), nil)
			require.Len(t, got, tc.nth)
			assert.Equal(t, tc.at, got[tc.nth-1].at)
			assert.Equal(t, tc.want, got[tc.nth-1].a)
		})
	}
}

// TestHeldCeilingAlertsAtCeilingAfter (M4): a ceiling line in a held row
// alerts after ceiling_after, busy or not.
func TestHeldCeilingAlertsAtCeilingAfter(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]team.Status{"idle": team.StatusIdle, "working": team.StatusWorking} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := held(1, 0)
			s.OldestHeldCeiling, s.Status = at(0), status
			got := run(defaults(), 2*m, m, steady(s), nil)
			assert.Equal(t, mins(2), times(got))
		})
	}
}

// TestCeilingAfterAReactionAlertsAtCeilingAfter (C1): a ceiling that comes
// with a reaction alerts after ceiling_after, not after every.
func TestCeilingAfterAReactionAlertsAtCeilingAfter(t *testing.T) {
	t.Parallel()
	const seen = 40*m + 10*time.Second
	state := func(d time.Duration) attention.State {
		if d < seen {
			return posts(2, 0)
		}
		s := ceilingPost(posts(1, m), seen)
		s.BossMark = 1
		return s
	}
	got := run(defaults(), 45*m, 10*time.Second, state, nil)
	assert.Equal(t, []time.Duration{10 * m, 15 * m, 25 * m, seen + 2*m}, times(got))

	// The boss's ruling (Q3, 2026-11-19): the episode starts at the opening
	// state's oldest pending time, not at the poll that opens it. A post
	// from 0:30 and a held ceiling from 0:40 are first seen at 1:00, as
	// after a daemon restart; the boss reads the post at 1:30. The ceiling
	// still alerts at 2:40, not at the read + every (7:00).
	tr := attention.NewTracker()
	var alerts []time.Duration
	for d := m; d <= 8*m; d += 10 * time.Second {
		s := held(1, 40*time.Second)
		s.OldestHeldCeiling, s.NewestCeiling = at(40*time.Second), at(40*time.Second)
		if d < 2*m {
			s.Pending, s.Oldest = 1, at(30*time.Second)
		} else {
			s.BossMark = 1
		}
		if _, ok := tr.Observe("app", defaults(), s, at(d)); ok {
			alerts = append(alerts, d)
			tr.Shown("app", true, at(d))
		}
	}
	assert.Equal(t, []time.Duration{2*m + 40*time.Second, 7*m + 40*time.Second}, alerts, "a ceiling pending when the episode opens")

	// The boss's ruling (Q4, 2026-11-19): before any shown alert the bound
	// arms for a ceiling at the episode's start too. A held row holding a
	// ceiling from its creation at 0:40 is the oldest pending item; a post
	// from 0:50 is read at 1:30. The ceiling alerts at 2:40, not at 6:30.
	tr, alerts = attention.NewTracker(), nil
	for d := 40 * time.Second; d <= 8*m; d += 10 * time.Second {
		s := held(1, 40*time.Second)
		s.OldestHeldCeiling, s.NewestCeiling = at(40*time.Second), at(40*time.Second)
		switch {
		case d >= 90*time.Second:
			s.BossMark = 1
		case d >= 50*time.Second:
			s.Pending, s.Oldest = 1, at(50*time.Second)
		}
		if _, ok := tr.Observe("app", defaults(), s, at(d)); ok {
			alerts = append(alerts, d)
			tr.Shown("app", true, at(d))
		}
	}
	assert.Equal(t, []time.Duration{2*m + 40*time.Second, 7*m + 40*time.Second}, alerts, "a ceiling that is the oldest pending item")
}

// TestCeilingBetweenRemindersAlertsAtCeilingAfter (C1, b3): a ceiling
// between reminders alerts after ceiling_after, and rule 4's gaps go on
// from it; a later read keeps the last shown alert, so the ceiling it
// already told does not bound the next.
func TestCeilingBetweenRemindersAlertsAtCeilingAfter(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		switch {
		case d < 26*m:
			return posts(1, 0)
		case d < 50*m:
			return ceilingPost(posts(1, 0), 26*m)
		}
		s := ceilingPost(boss(), 26*m)
		s.BossMark = 1
		return s
	}
	got := run(defaults(), 56*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 28, 48, 55), times(got))

	// A ceiling stamped at the alert at 15, as a line the same poll stores
	// after the alert, is first seen with a read at 16. It is not newer
	// than the last shown alert, which the read keeps, so it bounds
	// nothing: the read's later of 21 and 11 stands.
	sameTime := func(d time.Duration) attention.State {
		if d < 16*m {
			return posts(2, 0)
		}
		s := ceilingPost(posts(1, m), 15*m)
		s.BossMark = 1
		return s
	}
	assert.Equal(t, mins(10, 15, 21), times(run(defaults(), 22*m, m, sameTime, nil)), "a ceiling as old as the last alert")
}

// TestAppendedHeldCeilingAlertsMidEpisode (C1): a ceiling line appended to
// a held row mid-episode alerts after ceiling_after from the append.
func TestAppendedHeldCeilingAlertsMidEpisode(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		s := held(1, 0)
		if d >= 16*m {
			s.OldestHeldCeiling, s.NewestCeiling = at(0), at(16*m)
		}
		return s
	}
	got := run(defaults(), 20*m, m, state, nil)
	require.Equal(t, mins(10, 15, 18), times(got))
	// 971fff71: an alert the ceiling bound brings mid-episode takes the
	// second or third title.
	assert.Equal(t, "agentisan: 1 messages wait in ha-boss's inbox", got[2].a.Title)
	assert.False(t, got[2].a.Ceiling)
}

// TestLaterCeilingNeverDelaysTheBound (C1, b1): a second ceiling never moves
// the first one's bound later.
func TestLaterCeilingNeverDelaysTheBound(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		switch {
		case d < 26*m:
			return posts(1, 0)
		case d < 27*m:
			return ceilingPost(posts(1, 0), 26*m)
		}
		return ceilingPost(ceilingPost(posts(1, 0), 26*m), 27*m)
	}
	got := run(defaults(), 50*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 28, 48), times(got))
}

// unseen is a boss showing no agent with one message held since 0, as
// before the daemon has seen its agent.
func unseen() attention.State {
	s := held(1, 0)
	s.Agent = ""
	return s
}

// TestCeilingAppendedAtEveryPollKeepsItsGaps (rule 6): a held ceiling row a
// notice is appended to at every poll alerts at its bound, then at
// reminders, never at every poll.
func TestCeilingAppendedAtEveryPollKeepsItsGaps(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		s := unseen()
		s.OldestHeldCeiling, s.NewestCeiling = at(0), at(d)
		return s
	}
	got := run(defaults(), 12*m, m, state, nil)
	assert.Equal(t, mins(2, 5, 8, 11), times(got))
}

// TestCeilingGoneBeforeItsBoundBoundsNothing (rule 2): a ceiling posted at
// 26 and read at 27 brings no alert at 28; nor does a held one from 26
// typed into the boss at 27, with no read.
func TestCeilingGoneBeforeItsBoundBoundsNothing(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		switch {
		case d < 26*m:
			return posts(1, 0)
		case d < 27*m:
			return ceilingPost(posts(1, 0), 26*m)
		}
		s := posts(1, 27*m)
		s.BossMark = 2
		return s
	}
	got := run(defaults(), 40*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 37), times(got), "read")

	typed := func(d time.Duration) attention.State {
		s := posts(1, 0)
		if d >= 26*m && d < 27*m {
			s.Held, s.OldestHeld, s.OldestHeldCeiling, s.NewestCeiling = 1, at(26*m), at(26*m), at(26*m)
		}
		return s
	}
	got = run(defaults(), 50*m, m, typed, nil)
	assert.Equal(t, mins(10, 15, 25, 45), times(got), "typed in")
}

// TestNoBossPlaceHoldsTheFirstAlert (rule 7): with no boss place from 5 to
// 20, the first alert waits for the place.
func TestNoBossPlaceHoldsTheFirstAlert(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		s := posts(1, 0)
		s.Place = d < 5*m || d >= 20*m
		return s
	}
	got := run(defaults(), 26*m, m, state, nil)
	assert.Equal(t, mins(20, 25), times(got))
}

// TestRestartAlertsAtOnceWhateverTheMark (rules 3 and 8): a new daemon's
// first poll, at 30 with posts from 0, alerts at once; the boss's mark
// then, 0 or not, is no reaction.
func TestRestartAlertsAtOnceWhateverTheMark(t *testing.T) {
	t.Parallel()
	for _, mark := range []int64{0, 7} {
		s := posts(2, 0)
		s.BossMark = mark
		tr := attention.NewTracker()
		var got []time.Duration
		for d := 30 * m; d <= 40*m; d += m {
			if _, ok := tr.Observe("app", defaults(), s, at(d)); ok {
				got = append(got, d)
				tr.Shown("app", true, at(d))
			}
		}
		assert.Equal(t, mins(30, 35), got, "mark %d", mark)
	}
}

// TestHeldOlderThanThePostsSetsTheTime (rule 2): the oldest is the earlier
// of the posts' and the held messages'.
func TestHeldOlderThanThePostsSetsTheTime(t *testing.T) {
	t.Parallel()
	s := posts(1, 5*m)
	s.Held, s.OldestHeld = 1, at(0)
	got := run(defaults(), 12*m, m, steady(s), nil)
	require.Equal(t, mins(10), times(got))
	assert.Contains(t, got[0].a.Body, "oldest 10 min")
}

// TestHeldCeilingStartsTheEpisodeFirstSeenLate (Q4, f523b80b): a held
// ceiling from 0:40, older than the post from 0:50, is the episode's start
// though the episode is first seen at 1:00; the post is read at 1:30.
func TestHeldCeilingStartsTheEpisodeFirstSeenLate(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	var got []time.Duration
	for d := m; d <= 8*m; d += 10 * time.Second {
		s := held(1, 40*time.Second)
		s.OldestHeldCeiling, s.NewestCeiling = at(40*time.Second), at(40*time.Second)
		if d >= 90*time.Second {
			s.BossMark = 1
		} else {
			s.Pending, s.Oldest = 1, at(50*time.Second)
		}
		if _, ok := tr.Observe("app", defaults(), s, at(d)); ok {
			got = append(got, d)
			tr.Shown("app", true, at(d))
		}
	}
	assert.Equal(t, []time.Duration{2*m + 40*time.Second, 7*m + 40*time.Second}, got)
}

// TestDrainedCeilingKeepsItsBound (#184's S1): a held row from 0 gets a
// ceiling appended at 16 and is posted at 17 with its created_at, so the
// state's NewestCeiling steps back to 0. The episode keeps the newest
// ceiling since the last reaction, so the bound still alerts at 18; read
// instead, the ceiling bounds nothing.
func TestDrainedCeilingKeepsItsBound(t *testing.T) {
	t.Parallel()
	drain := func(d time.Duration) attention.State {
		if d < 17*m {
			s := unseen()
			if d >= 16*m {
				s.OldestHeldCeiling, s.NewestCeiling = at(0), at(16*m)
			}
			return s
		}
		s := posts(1, 0)
		s.OldestCeiling, s.NewestCeiling = at(0), at(0)
		return s
	}
	got := run(defaults(), 30*m, m, drain, nil)
	assert.Equal(t, mins(10, 15, 18), times(got), "drained at 17")

	read := func(d time.Duration) attention.State {
		if d < 17*m {
			return drain(d)
		}
		s := posts(1, 17*m)
		s.BossMark = 3
		return s
	}
	got = run(defaults(), 30*m, m, read, nil)
	assert.Equal(t, mins(10, 15, 27), times(got), "read at 17")

	mixed := func(d time.Duration) attention.State {
		s := unseen()
		s.OldestHeldCeiling, s.NewestCeiling = at(0), at(m)
		if d >= 16*m && d < 17*m {
			s = ceilingPost(s, 16*m)
		}
		if d >= 17*m {
			s.BossMark = 1
		}
		return s
	}
	got = run(defaults(), 30*m, m, mixed, nil)
	assert.NotContains(t, times(got), 18*m, "a newer posted ceiling read at 17 while the held one stays")
}

// TestPastTheLimitAnUnshownAlertIsRetried (rules 5 and 6): with Alerts 1,
// the ceiling's extra alert not shown at 32 comes back at 37.
func TestPastTheLimitAnUnshownAlertIsRetried(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.Alerts = 1
	state := func(d time.Duration) attention.State {
		if d < 30*m {
			return posts(1, 0)
		}
		return ceilingPost(posts(1, 0), 30*m)
	}
	got := run(c, 90*m, m, state, func(d time.Duration) bool { return d != 32*m })
	assert.Equal(t, mins(10, 32, 37), times(got))
}

// TestShorterBusyGraceNeverBringsItSooner (rule 2): BusyGrace counts only
// when longer than After.
func TestShorterBusyGraceNeverBringsItSooner(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.BusyGrace = 5 * m
	s := posts(1, 0)
	s.Status = team.StatusWorking
	got := run(c, 12*m, m, steady(s), nil)
	assert.Equal(t, mins(10), times(got))
}

// TestShownTwiceCountsOnce (rule 6): a second answer to one alert changes
// nothing.
func TestShownTwiceCountsOnce(t *testing.T) {
	t.Parallel()
	tr := attention.NewTracker()
	_, ok := tr.Observe("app", defaults(), posts(1, 0), at(10*m))
	require.True(t, ok)
	tr.Shown("app", true, at(10*m))
	tr.Shown("app", true, at(10*m))
	_, ok = tr.Observe("app", defaults(), posts(1, 0), at(15*m))
	assert.True(t, ok)
}

// TestCeilingTitleAndBody (Q2, f523b80b, and the body): a ceiling due at
// the same time as the oldest takes the ceiling title; a held ceiling's
// body keeps its held clause; minutes are cut, not rounded.
func TestCeilingTitleAndBody(t *testing.T) {
	t.Parallel()
	tie := func(d time.Duration) attention.State {
		if d < 8*m {
			return posts(1, 0)
		}
		return ceilingPost(posts(1, 0), 8*m)
	}
	got := run(defaults(), 10*m, m, tie, nil)
	require.NotEmpty(t, got)
	assert.True(t, got[0].a.Ceiling, "a tie")

	s := held(1, 0)
	s.OldestHeldCeiling, s.NewestCeiling = at(0), at(0)
	got = run(defaults(), 2*m, m, steady(s), nil)
	require.Len(t, got, 1)
	assert.Equal(t, "agentisan: a ceiling waits unread for ha-boss", got[0].a.Title)
	assert.Contains(t, got[0].a.Body, "; 1 messages wait in its inbox. Alert 1.")

	cut := func(d time.Duration) attention.State {
		if d < 40*time.Second {
			return posts(1, 0)
		}
		return ceilingPost(posts(1, 0), 40*time.Second)
	}
	got = run(defaults(), 3*m, 10*time.Second, cut, nil)
	require.NotEmpty(t, got)
	assert.Equal(t, 2*m+40*time.Second, got[0].at)
	assert.Contains(t, got[0].a.Body, "oldest 2 min;")
}

// TestLimitAtTheAlertsInstantAndAtAReaction (rule 5): a held or posted
// ceiling from the last shown alert's own instant is not newer than it; and
// a reaction at the poll a held-back ceiling's bound falls due resets the
// count first.
func TestLimitAtTheAlertsInstantAndAtAReaction(t *testing.T) {
	t.Parallel()
	c := defaults()
	c.Alerts = 1
	heldFrom := func(d, row, appended time.Duration) attention.State {
		s := posts(1, 0)
		if d >= row+m {
			s.Held, s.OldestHeld, s.OldestHeldCeiling, s.NewestCeiling = 1, at(row), at(row), at(row)
		}
		if d >= appended {
			s.NewestCeiling = at(appended)
		}
		return s
	}
	instant := func(d time.Duration) attention.State { return heldFrom(d, 10*m, 12*m) }
	assert.Equal(t, mins(10), times(run(c, 30*m, m, instant, nil)), "a row from 10, appended at 12")
	posted := func(d time.Duration) attention.State {
		s := posts(1, 0)
		s.Held, s.OldestHeld = 1, at(0)
		if d >= 11*m {
			s = ceilingPost(s, 10*m)
		}
		if d >= 16*m {
			s.OldestHeldCeiling, s.NewestCeiling = at(0), at(16*m)
		}
		return s
	}
	assert.Equal(t, mins(10), times(run(c, 30*m, m, posted, nil)), "a ceiling posted at 10, a held one appended at 16")

	reaction := func(d time.Duration) attention.State {
		s := heldFrom(d, 12*m, 20*m)
		if d >= 22*m {
			s.Pending, s.Oldest, s.BossMark = 0, time.Time{}, 1
		}
		return s
	}
	assert.Equal(t, mins(10, 14, 22), times(run(c, 30*m, m, reaction, nil)), "a row from 12, appended at 20, a read at 22")
}

// TestCeilingKeepsItsBoundThroughAPause (rule 7): paused from 26 to 40, the
// ceiling from 27 alerts when the place is back; a later one in the same
// pause never moves it.
func TestCeilingKeepsItsBoundThroughAPause(t *testing.T) {
	t.Parallel()
	state := func(d time.Duration) attention.State {
		s := posts(1, 0)
		if d >= 27*m {
			s = ceilingPost(s, 27*m)
		}
		if d >= 39*m {
			s = ceilingPost(s, 39*m)
		}
		s.Place = d < 26*m || d >= 40*m
		return s
	}
	got := run(defaults(), 50*m, m, state, nil)
	assert.Equal(t, mins(10, 15, 25, 40), times(got))
}

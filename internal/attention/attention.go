// Package attention is the alert that tells the user a board-mode boss has
// left posts unread on its board, or messages waiting in its inbox, too long
// (ADR-001 A74): when an episode opens, its reminders, and what resets,
// pauses or ends it. It is pure. The daemon's team poll builds each State and
// shows each Alert (PR 3b); until then nothing calls it, and tests build its
// inputs by hand.
package attention

import (
	"fmt"
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/team"
)

// Config is one project's alert settings, [notices.attention].
type Config struct {
	// After is how long the oldest post or held message waits before an
	// episode's first alert; 0 turns every alert off. CeilingAfter is how
	// long a context ceiling waits, busy or not. Every is the first
	// reminder's gap, doubled after each reminder up to MaxEvery, and the
	// wait after a reaction or an alert not shown. BusyGrace stands in for
	// After, when longer, while the boss is working.
	After, CeilingAfter, Every, MaxEvery, BusyGrace time.Duration
	Alerts                                          int    // the most alerts an episode shows; 0: no limit, until a reaction
	Sound                                           string // none, done or request
}

// State is what one team poll sees of a board-mode boss. A zero time is
// none.
type State struct {
	Boss                  string
	Pending               int // posts after the boss's mark
	Oldest, OldestCeiling time.Time
	BossMark              int64
	Held                  int // messages waiting in the boss's inbox
	OldestHeld            time.Time
	// OldestHeldCeiling is the created_at of the oldest held daemon row
	// holding a ceiling line, and NewestCeiling the latest ceiling change
	// pending: the newest ceiling post's created_at, or the latest
	// updated_at of a held daemon row holding a ceiling line.
	OldestHeldCeiling time.Time
	NewestCeiling     time.Time
	Status            team.Status // of the pane in the boss place bearing the boss's name
	Agent             string      // "" when none shows
	Place             bool        // this poll's model has a pane in the team's boss place
}

// Alert is one pop-up for the user.
type Alert struct {
	Title, Body, Sound string
	N, Of              int // Of 0: no limit
	// Ceiling is set when the title names a ceiling, and Last when the body
	// says this is the episode's last alert.
	Ceiling, Last bool
}

// Tracker keeps each project's episode, in memory only: a new daemon opens a
// new episode for whatever is pending. Build one with NewTracker. Only one
// goroutine uses it, the team poll's.
type Tracker struct{ episodes map[string]*episode }

// NewTracker is a Tracker with no episode open.
func NewTracker() *Tracker { return &Tracker{episodes: map[string]*episode{}} }

// episode is one project's run of alerts, while anything is pending.
type episode struct {
	// start is the oldest post or held message pending when the episode
	// opened, so a ceiling already pending then is newer than it, after a
	// restart too; now when the state gives no time.
	start time.Time
	mark  int64 // BossMark at the last Observe
	// count is the alerts shown since the episode opened or the boss last
	// reacted, and lastShown the time of the last one; a reaction resets
	// the count, not the time.
	count     int
	lastShown time.Time
	reacted   time.Time // the last reaction
	retry     time.Time // when an alert not shown is tried again
	// bound is when a ceiling newer than the last shown alert makes the
	// next alert due at the latest, and tried the newest ceiling pending
	// at the last try, which no longer bounds anything.
	bound, tried time.Time
	every        time.Duration // Every at the last try, for Shown
	outstanding  bool          // an alert was tried and Shown has not answered it
}

// Observe reads one poll's state of project's boss, with project's c, at now,
// and returns the alert due, if one is. The caller shows it and answers with
// Shown before it observes project again: an alert left unanswered is
// returned again at every poll below the limit, and never again past it.
//
//   - An episode runs while Pending + Held > 0, and ends, forgotten, when
//     nothing is pending. After 0 turns every alert off.
//   - Its first alert is due at the earlier of the oldest post or held
//     message + After (or BusyGrace, when longer, while the boss is
//     working), and the oldest ceiling, posted or held, + CeilingAfter.
//   - After the k-th shown alert the next waits min(Every × 2^(k−1),
//     MaxEvery). One not shown is tried again Every later.
//   - A rise of BossMark is a reaction: the count goes to 0 and the next
//     alert waits for the later of Every and the first alert's time.
//   - A ceiling newer than the last shown alert is never held back
//     (bound).
//   - Alerts > 0 stops the episode after that many shown alerts, but for a
//     ceiling newer than the last.
//   - With no boss place no alert is shown, and the episode is kept.
func (t *Tracker) Observe(project string, c Config, s State, now time.Time) (Alert, bool) {
	if c.After <= 0 || s.Pending+s.Held <= 0 {
		delete(t.episodes, project)
		return Alert{}, false
	}
	e := t.episodes[project]
	if e == nil {
		e = &episode{start: earliest(earliest(s.Oldest, s.OldestHeld), now), mark: s.BossMark}
		t.episodes[project] = e
	}
	if s.BossMark > e.mark {
		e.count, e.reacted, e.retry = 0, now, time.Time{}
	}
	e.mark = s.BossMark
	stopped := c.Alerts > 0 && e.count >= c.Alerts
	e.keepBound(c, s)
	if !s.Place {
		return Alert{}, false
	}
	due := e.due(c, s, stopped)
	if due.IsZero() || now.Before(due) {
		return Alert{}, false
	}
	e.tried, e.bound, e.every, e.outstanding = s.NewestCeiling, time.Time{}, c.Every, true
	return e.alert(c, s, now), true
}

// Shown answers the alert Observe last returned for project: shown counts
// it, and one not shown is tried again Every later, with its number. With
// no alert outstanding it changes nothing.
func (t *Tracker) Shown(project string, shown bool, now time.Time) {
	e := t.episodes[project]
	if e == nil || !e.outstanding {
		return
	}
	e.outstanding = false
	if !shown {
		e.retry = now.Add(e.every)
		return
	}
	e.count++
	e.lastShown, e.retry = now, time.Time{}
}

// keepBound keeps the ceiling's bound: while the newest pending ceiling is
// newer than the last shown alert (before any, than the episode's start)
// and than the ceiling pending at the last try, the next alert is due no
// later than it + CeilingAfter, and a later ceiling never moves that later.
// A cap on alerts a ceiling repeats would go here.
func (e *episode) keepBound(c Config, s State) {
	since := e.lastShown
	if since.IsZero() {
		since = e.start
	}
	if !s.NewestCeiling.After(since) || !s.NewestCeiling.After(e.tried) {
		e.bound = time.Time{}
		return
	}
	e.bound = earliest(e.bound, s.NewestCeiling.Add(c.CeilingAfter))
}

// due is when the next alert is due, zero for none. Past the limit, only a
// ceiling newer than the last shown alert gives one, at the ceiling's bound.
func (e *episode) due(c Config, s State, stopped bool) time.Time {
	var at time.Time
	switch {
	case stopped:
		if !ceilingOf(s).After(e.lastShown) {
			return time.Time{}
		}
		at = e.retry
	case !e.retry.IsZero():
		at = e.retry
	case e.count > 0:
		at = e.lastShown.Add(gap(c, e.count))
	case !e.reacted.IsZero():
		at = later(e.reacted.Add(c.Every), first(c, s))
	default:
		at = first(c, s)
	}
	return earliest(at, e.bound)
}

// gap is the wait after the k-th shown alert, min(Every × 2^(k−1),
// MaxEvery), doubled only while under MaxEvery so it never overflows.
func gap(c Config, k int) time.Duration {
	g := c.Every
	for i := 1; i < k && g < c.MaxEvery; i++ {
		g *= 2
	}
	return min(g, c.MaxEvery)
}

// first is when an episode's first alert is due, zero for none.
func first(c Config, s State) time.Time { return earliest(firstTimes(c, s)) }

// firstTimes are the two times a first alert is due at the earlier of: the
// oldest post or held message + After, or BusyGrace while the boss's agent
// is working, and the oldest ceiling + CeilingAfter. Zero is none.
func firstTimes(c Config, s State) (oldest, ceiling time.Time) {
	if o := earliest(s.Oldest, s.OldestHeld); !o.IsZero() {
		after := c.After
		if s.Status == team.StatusWorking && s.Agent != "" && c.BusyGrace > 0 {
			after = max(c.After, c.BusyGrace)
		}
		oldest = o.Add(after)
	}
	if ct := ceilingOf(s); !ct.IsZero() {
		ceiling = ct.Add(c.CeilingAfter)
	}
	return oldest, ceiling
}

// ceilingOf is the oldest pending ceiling, posted or held; zero for none.
func ceilingOf(s State) time.Time { return earliest(s.OldestCeiling, s.OldestHeldCeiling) }

// alert is the alert e tries now: the ceiling's title when it is the first
// alert since the episode opened or the boss reacted and the ceiling's time
// is the earlier of the two, else the posts' title, else the held one's.
func (e *episode) alert(c Config, s State, now time.Time) Alert {
	a := Alert{Sound: c.Sound, N: e.count + 1, Of: c.Alerts}
	a.Last = a.Of > 0 && a.N == a.Of
	oldest, ceiling := firstTimes(c, s)
	a.Ceiling = e.count == 0 && !ceiling.IsZero() && (oldest.IsZero() || !oldest.Before(ceiling))
	switch {
	case a.Ceiling:
		a.Title = "agentisan: a ceiling waits unread for " + s.Boss
	case s.Pending > 0:
		a.Title = fmt.Sprintf("agentisan: %s has %d unread on its board", s.Boss, s.Pending)
	default:
		a.Title = fmt.Sprintf("agentisan: %d messages wait in %s's inbox", s.Held, s.Boss)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "oldest %d min; the boss is %s", now.Sub(earliest(s.Oldest, s.OldestHeld))/time.Minute, statusOf(s))
	if s.Held > 0 && (a.Ceiling || s.Pending > 0) {
		fmt.Fprintf(&b, "; %d messages wait in its inbox", s.Held)
	}
	switch {
	case a.Of == 0:
		fmt.Fprintf(&b, ". Alert %d", a.N)
	case a.N > a.Of: // rule 5's ceiling past the limit, which a newer one can follow
		fmt.Fprintf(&b, ". Alert %d, past the limit of %d", a.N, a.Of)
	default:
		fmt.Fprintf(&b, ". Alert %d of %d", a.N, a.Of)
	}
	b.WriteString(". Open the board: prefix+d, then b")
	if a.Last {
		b.WriteString("; the last alert")
	}
	a.Body = b.String()
	return a
}

// statusOf is the boss as an alert's body names it.
func statusOf(s State) string {
	if s.Agent == "" {
		return "showing no agent"
	}
	switch s.Status {
	case team.StatusIdle, team.StatusWorking, team.StatusDone, team.StatusBlocked, team.StatusUnknown:
		return string(s.Status)
	}
	return string(team.StatusUnknown)
}

// earliest is the earlier of a and b, a zero being none.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}

// later is the later of a and b, a zero being none.
func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

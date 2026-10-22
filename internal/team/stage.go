package team

import (
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// StageState is whether a pane's stage still says what its agent is doing
// (ADR-001 A42): herdr keeps a token until its TTL, 24 h for a stage, long
// after the turn it named has ended.
type StageState struct {
	// Stale is set when nothing reported the stage again in the pane's latest
	// working stretch, now ended, or when herdr detects no agent in the pane.
	Stale bool
	// At is when the stage was last reported (stage_at); zero when unknown.
	At time.Time
}

// StageOf is the state of p's stage at now, from its stage_at and the
// stretches clock saw. A pane with no stage has none. A pane herdr detects
// no agent in is stale, its agent gone (F3); otherwise a stage is stale only
// when the pane is at rest, clock saw its latest stretch begin and end (two
// polls at rest, so a Stop hook's report that lands after the first is in
// time), the pane holds the stage it held as that stretch began, and its
// stage_at is before that stretch's start. Any of those unknown is a current
// stage, never a false alarm:
//   - a stage_at absent, unreadable or after now, as from an older binary;
//   - a stretch the clock did not see whole, as after a daemon restart;
//   - a stage that changed during the stretch, whatever stage_at says, as
//     herdr's own `pane report` changes it without one.
//
// stage_at is in whole seconds, so the stretch's start is too, and a report
// in the second the stretch began counts as inside it.
func StageOf(p Pane, clock *IdleClock, now time.Time) StageState {
	if p.Stage == "" {
		return StageState{}
	}
	at := p.StageAt
	if at.After(now) {
		at = time.Time{}
	}
	st := StageState{At: at}
	if p.Agent == "" {
		st.Stale = true
		return st
	}
	if !resting(p) || at.IsZero() {
		return st
	}
	from, stage, ok := clock.Stretch(p.ID)
	st.Stale = ok && stage == p.Stage && at.Before(from.Truncate(time.Second))
	return st
}

// The stage display (ADR-001 A42): every poll, the daemon writes each agent
// pane's stage into one of two tokens, so the user's sidebar rows draw a
// current stage with the stage colour rules and an old one dimmed, with its
// age. A colour rule cannot tell them apart: herdr gives no token a time.
// They go with `$team`'s source and TTL, renewed every poll.
const (
	// StageNowKey holds a current stage, as the pane's stage token holds it.
	StageNowKey = "stage_now"
	// StageOldKey holds a stale stage and its age, "build_test · 3h".
	StageOldKey = "stage_old"
)

// StageDisplay returns the key and the value that show a pane's stage in
// state st at now: StageNowKey and the stage as it is, or StageOldKey and the
// stage, then " · " and its age since st.At (StageAge), or " · stale" when
// that is not known, the stage cut with "…" so the whole fits herdr's
// report.MaxValueChars characters and the age always shows: the words say
// the stage is old where the row's colour does not. An empty stage has no
// display, and key is "".
func StageDisplay(stage string, st StageState, now time.Time) (key, value string) {
	switch {
	case stage == "":
		return "", ""
	case !st.Stale:
		return StageNowKey, stage
	}
	age := " · stale"
	if !st.At.IsZero() {
		age = " · " + StageAge(st.At, now)
	}
	if room := report.MaxValueChars - utf8.RuneCountInString(age); utf8.RuneCountInString(stage) > room {
		stage = string([]rune(stage)[:room-1]) + "…"
	}
	return StageOldKey, stage + age
}

// StageAge is the time from at to now in one coarse unit, for a narrow
// sidebar: whole minutes under an hour ("12m"), whole hours under a day
// ("3h"), else whole days ("2d"). A time after now is "0m".
func StageAge(at, now time.Time) string {
	d := max(now.Sub(at), 0)
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// stageTokens is the stage display of every pane in l, by pane id, as
// StageOf finds it at now with clock: both keys, the one not in use empty,
// which clears it, so a stage going stale never shows twice.
func stageTokens(l Layout, clock *IdleClock, now time.Time) map[string]map[string]string {
	names := agentNames(l.Agents)
	out := make(map[string]map[string]string, len(l.Panes))
	for _, p := range l.Panes {
		pane := worker(p, Target{}, nil, names).Pane
		tokens := map[string]string{StageNowKey: "", StageOldKey: ""}
		if key, value := StageDisplay(pane.Stage, StageOf(pane, clock, now), now); key != "" {
			tokens[key] = value
		}
		out[p.PaneID] = tokens
	}
	return out
}

package team_test

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// stretchT0 is the first poll of these tests: 400 ms into its second, so a
// stretch start that is not truncated to the second shows.
var stretchT0 = time.Date(2026, 10, 21, 18, 0, 0, 400_000_000, time.UTC)

// poll is the time of the nth poll, 3 s apart.
func poll(n int) time.Time { return stretchT0.Add(time.Duration(3*n) * time.Second) }

// seen is one pane at one poll: its status and stage, or gone from the
// model when gone is set.
type seen struct {
	status team.Status
	stage  string
	gone   bool
}

// observe feeds c one poll per entry of polls, the nth at poll(n).
func observe(c *team.IdleClock, polls ...seen) {
	for n, s := range polls {
		observeAt(c, n, s)
	}
}

// observeAt feeds c poll n: a model holding the agent pane w1:p1 as s saw
// it.
func observeAt(c *team.IdleClock, n int, s seen) {
	g := team.Group{Name: "coder"}
	if !s.gone {
		g.Workers = []team.Worker{{ID: "w1:p1", Pane: team.Pane{ID: "w1:p1", Agent: "claude", Status: s.status, Stage: s.stage}}}
	}
	c.Observe(team.Model{Projects: []team.Project{{Name: "ha", Groups: []team.Group{g}}}}, poll(n))
}

var (
	idle    = seen{status: team.StatusIdle, stage: "build_test"}
	done    = seen{status: team.StatusDone, stage: "build_test"}
	working = seen{status: team.StatusWorking, stage: "build_test"}
	blocked = seen{status: team.StatusBlocked, stage: "build_test"}
	unknown = seen{status: team.StatusUnknown, stage: "build_test"}
	gone    = seen{gone: true}
)

// TestIdleClockStretch: the clock times a working stretch only when it saw
// both its ends, from the last poll that saw the pane at rest before it,
// since the stretch began after that poll, with the stage the pane held at
// that poll, to the second poll in a row that sees it at rest again. Blocked
// is part of a stretch; unknown, or leaving the model, forgets the pane; a
// pane first seen at work, as after a daemon restart, needs a rest, a
// stretch and two polls at rest again.
func TestIdleClockStretch(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		polls     []seen
		from      time.Time
		stage     string
		unknownTo bool
	}{
		"a whole stretch, from the last rest before it": {polls: []seen{idle, idle, working, working, idle, idle}, from: poll(1), stage: "build_test"},
		"one poll at rest after it is too soon":         {polls: []seen{idle, working, idle}, unknownTo: true},
		"and still ended at the third":                  {polls: []seen{idle, working, idle, idle, idle}, from: poll(0), stage: "build_test"},
		"done is a rest":                                {polls: []seen{done, working, done, idle}, from: poll(0), stage: "build_test"},
		"blocked is part of the stretch":                {polls: []seen{idle, blocked, working, blocked, done, done}, from: poll(0), stage: "build_test"},
		"the stage held as it began":                    {polls: []seen{{status: team.StatusIdle, stage: "a"}, {status: team.StatusIdle, stage: "b"}, {status: team.StatusWorking, stage: "c"}, {status: team.StatusIdle, stage: "d"}, {status: team.StatusIdle, stage: "e"}}, from: poll(1), stage: "b"},
		"the latest of two stretches":                   {polls: []seen{idle, working, idle, idle, working, idle, idle}, from: poll(3), stage: "build_test"},
		"each stretch's end counts its rests anew":      {polls: []seen{idle, working, idle, idle, working, idle}, unknownTo: true},
		"a stretch still going":                         {polls: []seen{idle, working}, unknownTo: true},
		"blocked, still going":                          {polls: []seen{idle, blocked}, unknownTo: true},
		"first seen at work":                            {polls: []seen{working, idle, idle}, unknownTo: true},
		"first seen at work, then a whole":              {polls: []seen{working, idle, working, idle, idle}, from: poll(1), stage: "build_test"},
		"only ever at rest":                             {polls: []seen{idle, idle}, unknownTo: true},
		"unknown forgets the pane":                      {polls: []seen{idle, working, unknown, idle, idle}, unknownTo: true},
		"unknown at rest forgets it too":                {polls: []seen{idle, unknown, working, idle, idle}, unknownTo: true},
		"gone from the model forgets it":                {polls: []seen{idle, working, gone, idle, idle}, unknownTo: true},
		"a stretch ended, then gone":                    {polls: []seen{idle, working, idle, idle, gone}, unknownTo: true},
		"a stretch ended, then working on":              {polls: []seen{idle, working, idle, idle, working}, unknownTo: true},
		"never seen":                                    {unknownTo: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var c team.IdleClock
			observe(&c, tc.polls...)
			from, stage, ok := c.Stretch("w1:p1")
			if tc.unknownTo {
				assert.False(t, ok)
				assert.Zero(t, from)
				assert.Empty(t, stage)
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.from, from)
			assert.Equal(t, tc.stage, stage)
		})
	}
}

// TestIdleClockStretchLeavesTheRestAlone: timing stretches changes no rest
// (A23's idle_s, A37's idle_since): a rest still starts at the first poll
// that sees it.
func TestIdleClockStretchLeavesTheRestAlone(t *testing.T) {
	t.Parallel()
	var c team.IdleClock
	observe(&c, idle, idle, working, idle, idle)
	since, ok := c.Since("w1:p1")
	require.True(t, ok)
	assert.Equal(t, poll(3), since)
}

// TestIdleClockStretchNilAndConcurrent: a nil clock knows no stretch; the
// poll observing while a status op asks is safe (-race).
func TestIdleClockStretchNilAndConcurrent(t *testing.T) {
	t.Parallel()
	var nilClock *team.IdleClock
	_, _, ok := nilClock.Stretch("w1:p1")
	assert.False(t, ok)

	var c team.IdleClock
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { observe(&c, idle, working, idle, idle) })
		wg.Go(func() { c.Stretch("w1:p1") })
	}
	wg.Wait()
	_, _, ok = c.Stretch("w1:p1")
	assert.True(t, ok)
}

// secs is t truncated to the second, as a stage_at holds it.
func secs(t time.Time) time.Time { return time.Unix(t.Unix(), 0) }

// TestStageOf walks the rule against one clock that saw a whole stretch:
// at rest at poll 0, working at poll 1, at rest at polls 2 and 3 with
// "build_test", so the stretch began at poll 0, 400 ms into its second.
func TestStageOf(t *testing.T) {
	t.Parallel()
	var c team.IdleClock
	observe(&c, idle, working, idle, idle)
	now := poll(3).Add(time.Minute)
	before := secs(poll(0)).Add(-time.Second)
	pane := func(status team.Status, at time.Time) team.Pane {
		return team.Pane{ID: "w1:p1", Agent: "claude", Status: status, Stage: "build_test", StageAt: at}
	}

	tests := map[string]struct {
		pane team.Pane
		want team.StageState
	}{
		"reported before the stretch":        {pane: pane(team.StatusIdle, before), want: team.StageState{Stale: true, At: before}},
		"reported an hour before it":         {pane: pane(team.StatusIdle, before.Add(-time.Hour)), want: team.StageState{Stale: true, At: before.Add(-time.Hour)}},
		"done, reported before":              {pane: pane(team.StatusDone, before), want: team.StageState{Stale: true, At: before}},
		"reported in the second it began":    {pane: pane(team.StatusIdle, secs(poll(0))), want: team.StageState{At: secs(poll(0))}},
		"reported during the stretch":        {pane: pane(team.StatusIdle, secs(poll(1))), want: team.StageState{At: secs(poll(1))}},
		"reported again at its end":          {pane: pane(team.StatusIdle, secs(poll(2))), want: team.StageState{At: secs(poll(2))}},
		"reported now":                       {pane: pane(team.StatusIdle, secs(now)), want: team.StageState{At: secs(now)}},
		"no stage_at, as from an old binary": {pane: pane(team.StatusIdle, time.Time{}), want: team.StageState{}},
		"a stage_at ahead of the clock":      {pane: pane(team.StatusIdle, now.Add(time.Second)), want: team.StageState{}},
		"at work again":                      {pane: pane(team.StatusWorking, before), want: team.StageState{At: before}},
		"blocked":                            {pane: pane(team.StatusBlocked, before), want: team.StageState{At: before}},
		"status unknown":                     {pane: pane(team.StatusUnknown, before), want: team.StageState{At: before}},
		"no stage":                           {pane: team.Pane{ID: "w1:p1", Agent: "claude", Status: team.StatusIdle, StageAt: before}, want: team.StageState{}},
		"a pane the clock never saw":         {pane: team.Pane{ID: "w9:p9", Agent: "claude", Status: team.StatusIdle, Stage: "build_test", StageAt: before}, want: team.StageState{At: before}},
		"the stage changed during the stretch": {
			pane: team.Pane{ID: "w1:p1", Agent: "claude", Status: team.StatusIdle, Stage: "review", StageAt: before},
			want: team.StageState{At: before},
		},
		// F3: the agent is gone, so its stage is old whatever the clock saw.
		"no agent detected": {
			pane: team.Pane{ID: "w9:p9", Name: "ha-coder-1", Status: team.StatusUnknown, Stage: "build_test", StageAt: secs(now)},
			want: team.StageState{Stale: true, At: secs(now)},
		},
		"no agent and no stage_at": {pane: team.Pane{ID: "w9:p9", Stage: "build_test"}, want: team.StageState{Stale: true}},
		"no agent, stage_at ahead": {pane: team.Pane{ID: "w9:p9", Stage: "build_test", StageAt: now.Add(time.Hour)}, want: team.StageState{Stale: true}},
		"no agent and no stage":    {pane: team.Pane{ID: "w9:p9", StageAt: before}, want: team.StageState{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, team.StageOf(tc.pane, &c, now))
		})
	}
	assert.Equal(t, team.StageState{At: before}, team.StageOf(pane(team.StatusIdle, before), nil, now), "a nil clock saw no stretch")

	// Stages compare byte for byte, as herdr stores them: another spelling of
	// the stage the stretch began with is a changed stage, so current.
	var spelt team.IdleClock
	nfc, nfd := "caf\u00e9", "cafe\u0301"
	observe(&spelt, seen{status: team.StatusIdle, stage: nfc}, seen{status: team.StatusWorking, stage: nfc}, seen{status: team.StatusIdle, stage: nfd}, seen{status: team.StatusIdle, stage: nfd})
	for stage, stale := range map[string]bool{nfc: true, nfd: false} {
		p := team.Pane{ID: "w1:p1", Agent: "claude", Status: team.StatusIdle, Stage: stage, StageAt: before}
		assert.Equal(t, stale, team.StageOf(p, &spelt, now).Stale, "%+q", stage)
	}
}

// TestStageOfTheStretchItSaw: the rule over polls, as the daemon runs it. A
// stage reported at the end of each turn stays current while the pane
// rests, the same value reported again included ("waiting: boss"); one left
// from before a turn that reported nothing goes stale once that turn has
// ended, at the second poll that sees the pane at rest, never while it runs.
func TestStageOfTheStretchItSaw(t *testing.T) {
	t.Parallel()
	var c team.IdleClock
	reported := secs(poll(0)).Add(-10 * time.Second)
	state := func(n int, status team.Status) team.StageState {
		observeAt(&c, n, seen{status: status, stage: "waiting: boss"})
		return team.StageOf(team.Pane{ID: "w1:p1", Agent: "claude", Status: status, Stage: "waiting: boss", StageAt: reported}, &c, poll(n))
	}

	assert.False(t, state(0, team.StatusIdle).Stale, "the daemon has seen no turn yet")
	assert.False(t, state(1, team.StatusWorking).Stale, "a turn is running")
	reported = secs(poll(1)).Add(2 * time.Second) // its Stop hook reports
	assert.False(t, state(2, team.StatusIdle).Stale, "reported at the end of the turn")
	assert.False(t, state(3, team.StatusIdle).Stale, "and still, while the pane rests")

	assert.False(t, state(4, team.StatusWorking).Stale)
	reported = secs(poll(4)).Add(time.Second) // the same value, reported again
	assert.False(t, state(5, team.StatusIdle).Stale, "the same stage reported again is current")

	assert.False(t, state(6, team.StatusWorking).Stale, "a turn that reports nothing, still running")
	assert.False(t, state(7, team.StatusIdle).Stale, "at rest for one poll: its Stop hook may still report")
	got := state(8, team.StatusIdle)
	assert.True(t, got.Stale, "has ended: the stage is old")
	assert.Equal(t, reported, got.At)
	assert.True(t, state(9, team.StatusIdle).Stale, "and stays old while the pane rests")
}

// TestStageOfGivesTheStopHookAPoll: a Stop hook's report races the poll that
// first sees its turn's end, so the stage is judged at the second poll at
// rest. Whichever lands first, a stage the hook reports is current at both;
// one nothing reports is current at the first and old from the second.
func TestStageOfGivesTheStopHookAPoll(t *testing.T) {
	t.Parallel()
	old := secs(poll(0)).Add(-10 * time.Second)
	for name, tc := range map[string]struct {
		// reported is when the hook's report lands, as a stage_at, or zero
		// when nothing reports.
		reported time.Time
		stale    [2]bool
	}{
		"the report before the first poll at rest":     {reported: secs(poll(2)).Add(-time.Second)},
		"the report between the first and second poll": {reported: secs(poll(2)).Add(time.Second)},
		"no report": {stale: [2]bool{false, true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var c team.IdleClock
			at := func(n int) time.Time {
				if !tc.reported.IsZero() && !tc.reported.After(poll(n)) {
					return tc.reported
				}
				return old
			}
			state := func(n int, status team.Status) team.StageState {
				observeAt(&c, n, seen{status: status, stage: "review"})
				return team.StageOf(team.Pane{ID: "w1:p1", Agent: "claude", Status: status, Stage: "review", StageAt: at(n)}, &c, poll(n))
			}
			require.False(t, state(0, team.StatusIdle).Stale)
			require.False(t, state(1, team.StatusWorking).Stale)
			assert.Equal(t, tc.stale[0], state(2, team.StatusIdle).Stale, "the first poll at rest")
			assert.Equal(t, tc.stale[1], state(3, team.StatusIdle).Stale, "the second")
		})
	}
}

// TestStageDisplay: a current stage goes to stage_now as it is; a stale one
// to stage_old with its age, " · stale" when that is unknown, cut so the
// whole value is at most herdr's 80 characters and the age always shows.
func TestStageDisplay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 21, 0, 0, 0, time.UTC)
	old := team.StageState{Stale: true, At: now.Add(-3 * time.Hour)}
	long := strings.Repeat("x", report.MaxValueChars)
	wide := strings.Repeat("é", report.MaxValueChars)

	tests := map[string]struct {
		stage      string
		st         team.StageState
		key, value string
	}{
		"current":                {stage: "build_test", st: team.StageState{At: now}, key: team.StageNowKey, value: "build_test"},
		"current, no stage_at":   {stage: "build_test", key: team.StageNowKey, value: "build_test"},
		"current, 80 long":       {stage: long, key: team.StageNowKey, value: long},
		"stale":                  {stage: "build_test", st: old, key: team.StageOldKey, value: "build_test · 3h"},
		"stale, age unknown":     {stage: "build_test", st: team.StageState{Stale: true}, key: team.StageOldKey, value: "build_test · stale"},
		"stale, 80 long":         {stage: long, st: old, key: team.StageOldKey, value: strings.Repeat("x", 74) + "… · 3h"},
		"stale, 80 wide chars":   {stage: wide, st: old, key: team.StageOldKey, value: strings.Repeat("é", 74) + "… · 3h"},
		"stale, just fits":       {stage: strings.Repeat("x", 75), st: old, key: team.StageOldKey, value: strings.Repeat("x", 75) + " · 3h"},
		"stale, wide, just fits": {stage: strings.Repeat("é", 75), st: old, key: team.StageOldKey, value: strings.Repeat("é", 75) + " · 3h"},
		"stale, one over":        {stage: strings.Repeat("x", 76), st: old, key: team.StageOldKey, value: strings.Repeat("x", 74) + "… · 3h"},
		"stale, long, unknown":   {stage: long, st: team.StageState{Stale: true}, key: team.StageOldKey, value: strings.Repeat("x", 71) + "… · stale"},
		"no stage":               {st: old},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, value := team.StageDisplay(tc.stage, tc.st, now)
			assert.Equal(t, tc.key, key)
			assert.Equal(t, tc.value, value)
			assert.LessOrEqual(t, utf8.RuneCountInString(value), report.MaxValueChars)
		})
	}
}

// TestStageAge: one coarse unit, truncated, for a narrow sidebar.
func TestStageAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 21, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		-time.Hour: "0m", 0: "0m", 59 * time.Second: "0m", time.Minute: "1m", 59*time.Minute + 59*time.Second: "59m",
		time.Hour: "1h", 23*time.Hour + 59*time.Minute: "23h", 24 * time.Hour: "1d", 49 * time.Hour: "2d",
	} {
		assert.Equal(t, want, team.StageAge(now.Add(-d), now), "%v", d)
	}
}

// stageLayout is a ◆ coders space holding an agent pane, wN:p1, and a pane
// agent.list still names whose agent herdr no longer detects, wN:p3, with a
// pane outside every team, w2:p1: the agents at status, each with stage and
// stage_at tokens.
func stageLayout(status string, stage string, at time.Time) team.Layout {
	tokens := map[string]string{report.StageKey: stage, report.StageAtKey: report.FormatStageAt(at)}
	return team.Layout{
		Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}, {WorkspaceID: "w2", Label: "notes"}},
		Panes: []herdr.PaneInfo{
			{PaneID: "wN:p1", WorkspaceID: "wN", Agent: "claude", AgentStatus: status, Tokens: tokens},
			{PaneID: "wN:p3", WorkspaceID: "wN", AgentStatus: "unknown", Tokens: tokens},
			{PaneID: "w2:p1", WorkspaceID: "w2", Agent: "claude", AgentStatus: status, Tokens: tokens},
		},
		Agents: []herdr.AgentInfo{{Name: "ha-coder-9", PaneID: "wN:p3"}},
	}
}

// TestSidebarTokensShowTheStage: every agent pane carries its stage display
// beside its ctx display, both keys, the one not in use empty. A team pane
// whose turn ended with no report shows stage_old with its age; a pane
// outside every team, which the clock does not time, keeps stage_now; a
// pane whose agent is gone shows stage_old whatever the clock saw. A report
// again moves the stage back.
func TestSidebarTokensShowTheStage(t *testing.T) {
	t.Parallel()
	r := team.Resolvers{team.Spaces{Project: "agentisan"}}
	var c team.IdleClock
	reported := secs(poll(0)).Add(-time.Hour)
	var l team.Layout
	var m team.Model
	for n, status := range []string{"idle", "working", "idle", "idle"} {
		l = stageLayout(status, "build_test", reported)
		m = team.Build(l, r, nil)
		c.Observe(m, poll(n))
	}
	stages := func(now time.Time) map[string][2]string {
		out := map[string][2]string{}
		for _, pt := range team.SidebarTokens(m, l, &c, now) {
			out[pt.PaneID] = [2]string{pt.Tokens[team.StageNowKey], pt.Tokens[team.StageOldKey]}
			assert.Contains(t, pt.Tokens, team.StageNowKey)
			assert.Contains(t, pt.Tokens, team.StageOldKey)
			assert.Contains(t, pt.Tokens, team.CtxOKKey, "beside the ctx display")
		}
		return out
	}

	assert.Equal(t, map[string][2]string{
		"wN:p1": {"", "build_test · 1h"},
		"wN:p3": {"", "build_test · 1h"},
		"w2:p1": {"build_test", ""},
	}, stages(poll(3)))

	reported = secs(poll(3))
	l = stageLayout("idle", "build_test", reported)
	m = team.Build(l, r, nil)
	assert.Equal(t, map[string][2]string{
		"wN:p1": {"build_test", ""},
		"wN:p3": {"", "build_test · 0m"},
		"w2:p1": {"build_test", ""},
	}, stages(poll(3)), "reported again")

	l = stageLayout("idle", "", reported)
	m = team.Build(l, r, nil)
	assert.Equal(t, map[string][2]string{"wN:p1": {}, "wN:p3": {}, "w2:p1": {}}, stages(poll(3)), "no stage clears both")
}

// TestBuildReadsStageAt: a pane's stage_at token is read only as
// report.ParseStageAt reads it; anything else is no time.
func TestBuildReadsStageAt(t *testing.T) {
	t.Parallel()
	r := team.Resolvers{team.Spaces{Project: "agentisan"}}
	for v, want := range map[string]time.Time{
		"1761069600": time.Unix(1_761_069_600, 0), "": {}, "soon": {}, "-1": {}, "01761069600": {}, strconv.Itoa(0): {},
	} {
		l := team.Layout{
			Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}},
			Panes:      []herdr.PaneInfo{{PaneID: "wN:p1", WorkspaceID: "wN", Agent: "claude", Tokens: map[string]string{report.StageAtKey: v}}},
		}
		m := team.Build(l, r, nil)
		require.Len(t, m.Projects, 1)
		require.Len(t, m.Projects[0].Groups, 1)
		assert.Equal(t, want, m.Projects[0].Groups[0].Workers[0].Pane.StageAt, "%q", v)
	}
}

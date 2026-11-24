package notice_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// muteTeam is "ha" with its boss, two bound coders, a named unbound agent and
// an unnamed one.
func muteTeam() team.Model {
	return teamOf([]team.Worker{agentPane("w1:pB", "ha-boss", team.StatusWorking)},
		team.Worker{ID: "ha-coder-1", Bound: true, Pane: team.Pane{ID: "w1:p1", Name: "ha-coder-1", Agent: "claude"}},
		team.Worker{ID: "c2", Bound: true, Pane: team.Pane{ID: "w1:p2", Name: "c2", Agent: "claude"}},
		team.Worker{ID: "w1:p3", Pane: team.Pane{ID: "w1:p3", Agent: "codex"}},
		agentPane("w1:p4", "scratch", team.StatusWorking))
}

// about is a notice of kind k about the bound worker, with the scope the poll
// gives it; "" is about no worker.
func about(worker string, k notice.Kind) notice.Notice {
	w := team.Worker{ID: worker, Bound: true, Pane: team.Pane{ID: "pane of " + worker, Name: worker}}
	n := (notice.Crossing{Project: "ha", Worker: w, Level: team.LevelSoft}).Notice(false)
	n.Worker, n.Kind, n.Text = worker, k, worker+" "+string(k)
	return n
}

// TestMuteLasts: a mute lasts an hour by default, or as long as asked, up
// to a day; it silences its worker until its end, exclusive, and no longer.
func TestMuteLasts(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		d    time.Duration
		want time.Duration
	}{
		"the default":  {0, time.Hour},
		"a minute":     {time.Minute, time.Minute},
		"a nanosecond": {time.Nanosecond, time.Nanosecond},
		"a day":        {24 * time.Hour, 24 * time.Hour},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := notice.NewMutes(nil)
			got, err := s.Mute(t.Context(), muteTeam(), "ha-coder-1", tc.d, t0)
			require.NoError(t, err)
			assert.Equal(t, notice.Mute{Worker: "ha-coder-1", Until: t0.Add(tc.want)}, got)
			w := muteTeam().Projects[0].Groups[0].Workers[0]
			n := (notice.Crossing{Project: "ha", Worker: w, Level: team.LevelSoft}).Notice(false)
			assert.True(t, s.Muted(n, t0), "from the start")
			assert.True(t, s.Muted(n, t0.Add(tc.want-time.Nanosecond)), "until a tick before its end")
			assert.False(t, s.Muted(n, t0.Add(tc.want)), "not at its end")
		})
	}
	assert.Equal(t, time.Hour, notice.DefaultMute)
	assert.Equal(t, 24*time.Hour, notice.MaxMute)
}

// TestMuteRefuses: a length past a day or below nothing, and a name no
// bound worker has, the boss's and an unbound agent's included, are refused
// and mute no one.
func TestMuteRefuses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		worker string
		d      time.Duration
		want   error
	}{
		"a tick past a day":       {"ha-coder-1", 24*time.Hour + time.Nanosecond, notice.ErrMuteLength},
		"a negative length":       {"ha-coder-1", -time.Second, notice.ErrMuteLength},
		"a typo":                  {"ha-coder-9", 0, notice.ErrUnknownWorker},
		"the boss":                {"ha-boss", 0, notice.ErrUnknownWorker},
		"a bound worker's pane":   {"w1:p1", 0, notice.ErrUnknownWorker},
		"an unbound named agent":  {"scratch", 0, notice.ErrUnknownWorker},
		"an unbound pane id":      {"w1:p3", 0, notice.ErrUnknownWorker},
		"an empty name":           {"", 0, notice.ErrUnknownWorker},
		"a name as a line has it": {"pane w1:p3", 0, notice.ErrUnknownWorker},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := notice.NewMutes(nil)
			_, err := s.Mute(t.Context(), muteTeam(), tc.worker, tc.d, t0)
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, s.List(t0), "no one is muted")
		})
	}
}

// TestMuteTakesABoundWorkersID: a bound worker by its logical id, in any
// team, matched byte for byte.
func TestMuteTakesABoundWorkersID(t *testing.T) {
	t.Parallel()
	m := muteTeam()
	m.Projects = append(m.Projects, team.Project{Name: "skills", Groups: []team.Group{{Name: "qa", Workers: []team.Worker{
		{ID: "sk-qa-1", Bound: true, Pane: team.Pane{ID: "w2:p1", Agent: "claude"}},
		{ID: "エージェント", Bound: true, Pane: team.Pane{ID: "w2:p2", Agent: "claude"}},
		{ID: "c\n3", Bound: true, Pane: team.Pane{ID: "w2:p3", Agent: "claude"}},
	}}}})
	s := notice.NewMutes(nil)
	for _, w := range []string{"ha-coder-1", "c2", "sk-qa-1", "エージェント", "c\n3"} {
		_, err := s.Mute(t.Context(), m, w, 0, t0)
		require.NoError(t, err, w)
	}
	assert.Len(t, s.List(t0), 5)
	for _, w := range []string{"c 3", "c3", "エージェント ", "HA-CODER-1"} {
		_, err := s.Mute(t.Context(), m, w, 0, t0)
		require.ErrorIs(t, err, notice.ErrUnknownWorker, "%q: a name is matched byte for byte, as herdr reports it", w)
	}
	assert.True(t, s.Muted(about("c\n3", notice.Blocked), t0), "a notice names it as herdr does")
}

// TestMutedKinds: a mute silences every kind a team can skip, the soft and
// hard context levels included, never the ceiling, and nothing about
// another worker or about no worker. A nil Mutes mutes no one.
func TestMutedKinds(t *testing.T) {
	t.Parallel()
	s := notice.NewMutes(nil)
	_, err := s.Mute(t.Context(), muteTeam(), "c2", 0, t0)
	require.NoError(t, err)
	for _, k := range notice.Kinds {
		assert.Equal(t, k != notice.CtxCeiling, s.Muted(about("c2", k), t0), "%s", k)
		assert.False(t, s.Muted(about("ha-coder-1", k), t0), "%s about another worker", k)
		assert.False(t, s.Muted(about("", k), t0), "%s about no worker", k)
	}
	var none *notice.Mutes
	assert.False(t, none.Muted(about("c2", notice.Blocked), t0))
}

// TestUnmute: an unmute ends the mute and says whether one was in force; a
// worker with none, or whose mute ended, is no refusal.
func TestUnmute(t *testing.T) {
	t.Parallel()
	s := notice.NewMutes(nil)
	_, err := s.Mute(t.Context(), muteTeam(), "c2", time.Minute, t0)
	require.NoError(t, err)
	assert.True(t, s.Unmute("c2", t0.Add(time.Second)))
	assert.False(t, s.Muted(about("c2", notice.Blocked), t0.Add(time.Second)))
	assert.False(t, s.Unmute("c2", t0.Add(time.Second)), "none left")
	assert.False(t, s.Unmute("nobody", t0))

	_, err = s.Mute(t.Context(), muteTeam(), "c2", time.Minute, t0)
	require.NoError(t, err)
	assert.False(t, s.Unmute("c2", t0.Add(time.Minute)), "one that ended was not in force")
}

// TestMuteAgainSetsItsEnd: muting a muted worker sets its end anew, shorter
// or longer.
func TestMuteAgainSetsItsEnd(t *testing.T) {
	t.Parallel()
	s := notice.NewMutes(nil)
	_, err := s.Mute(t.Context(), muteTeam(), "c2", time.Hour, t0)
	require.NoError(t, err)
	_, err = s.Mute(t.Context(), muteTeam(), "c2", time.Minute, t0)
	require.NoError(t, err)
	assert.False(t, s.Muted(about("c2", notice.Blocked), t0.Add(time.Minute)))
	assert.Equal(t, []notice.Mute{}, s.List(t0.Add(time.Minute)))
}

// TestMuteList: the mutes in force, by name, and those that ended are
// forgotten; none is an empty list, never null.
func TestMuteList(t *testing.T) {
	t.Parallel()
	s := notice.NewMutes(nil)
	assert.Equal(t, []notice.Mute{}, s.List(t0))
	for w, d := range map[string]time.Duration{"c2": time.Minute, "ha-coder-1": 2 * time.Hour} {
		_, err := s.Mute(t.Context(), muteTeam(), w, d, t0)
		require.NoError(t, err)
	}
	assert.Equal(t, []notice.Mute{
		{Worker: "c2", Until: t0.Add(time.Minute)},
		{Worker: "ha-coder-1", Until: t0.Add(2 * time.Hour)},
	}, s.List(t0))
	assert.Equal(t, []notice.Mute{{Worker: "ha-coder-1", Until: t0.Add(2 * time.Hour)}}, s.List(t0.Add(time.Minute)))
	assert.False(t, s.Unmute("c2", t0), "the list forgot the mute that ended, so it is no longer there even before its end")
}

// TestMutesAreSafeAcrossGoroutines: the ops mute while the poll reads, as
// the daemon does (run with -race).
func TestMutesAreSafeAcrossGoroutines(t *testing.T) {
	t.Parallel()
	s := notice.NewMutes(nil)
	m := muteTeam()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				_, _ = s.Mute(t.Context(), m, "c2", time.Minute, t0)
				s.Unmute("c2", t0)
			}
		})
		wg.Go(func() {
			for range 100 {
				s.Muted(about("c2", notice.Blocked), t0)
				s.List(t0)
			}
		})
	}
	wg.Wait()
	_, err := s.Mute(t.Context(), m, "c2", time.Minute, t0)
	require.NoError(t, err)
	assert.True(t, s.Muted(about("c2", notice.Blocked), t0))
}

// TestMuteMatchesOnlyTheWorkerGeneration isolates row identity from cleanup (NERD-5730).
func TestMuteMatchesOnlyTheWorkerGeneration(t *testing.T) {
	t.Parallel()
	const id = "ha-coder-1"
	model := func(terminal, status string, generation int64) team.Model {
		return team.Build(team.Layout{
			Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "w1", Label: "ha"}},
			Tabs:       []herdr.TabInfo{{TabID: "w1:t1", WorkspaceID: "w1", Label: id}},
			Panes:      []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", TerminalID: terminal, Agent: "claude", AgentStatus: status}},
		}, team.Tabs{Project: "ha", Short: "ha"}, []store.Worker{{LogicalID: id, TerminalID: terminal, Generation: generation}})
	}
	s := notice.NewMutes(nil)
	_, err := s.Mute(t.Context(), model("old", "idle", 0), id, 0, t0)
	require.NoError(t, err)
	for _, tc := range []struct {
		terminal   string
		generation int64
		muted      bool
	}{
		{terminal: "rotated", generation: 0, muted: true},
		{terminal: "reused", generation: 1, muted: false},
	} {
		w := notice.NewWatch()
		w.Observe(model(tc.terminal, "idle", tc.generation), nil, true)
		got := w.Observe(model(tc.terminal, "blocked", tc.generation), nil, true)
		require.Len(t, got, 1)
		assert.Equal(t, tc.muted, s.Muted(got[0], t0), tc.terminal)
	}
	fresh, err := s.Mute(t.Context(), model("reused", "idle", 1), id, 0, t0)
	require.NoError(t, err)
	assert.True(t, s.ClearWorker(id, 0, t0), "a late clear ends the old generation's own mute")
	assert.Equal(t, []notice.Mute{fresh}, s.List(t0), "and cannot remove the next generation's")
}

func TestClearWorkerReportsAnExpiredMuteAsInactive(t *testing.T) {
	t.Parallel()
	s := notice.NewMutes(nil)
	const worker = "ha-coder-1"
	_, err := s.Mute(t.Context(), muteTeam(), worker, time.Minute, t0)
	require.NoError(t, err)
	assert.False(t, s.ClearWorker(worker, 0, t0.Add(time.Minute)))
	assert.Empty(t, s.List(t0), "the expired entry was deleted")
}

// namesakes are a bound row and an unbound pane carrying the same name
// (NERD-5730, third review): the first model has only the unbound pane, the
// second both, as when worker spawn reuses a name its holder freed.
func namesakes() (alone, both team.Model, bound, unbound team.Worker) {
	const name = "ha-qa-1"
	bound = team.Worker{ID: name, Bound: true, Generation: 4, Pane: team.Pane{ID: "w1:p1", TerminalID: "tb", Name: "session-x", Agent: "claude"}}
	unbound = team.Worker{ID: "w1:p2", Pane: team.Pane{ID: "w1:p2", TerminalID: "tu", Name: name, Agent: "claude"}}
	boss := []team.Worker{agentPane("w1:pB", "ha-boss", team.StatusWorking)}
	return teamOf(boss, unbound), teamOf(boss, unbound, bound), bound, unbound
}

func softNotice(w team.Worker) notice.Notice {
	return (notice.Crossing{Project: "ha", Worker: w, Level: team.LevelSoft}).Notice(false)
}

// TestMuteIsTheBoundRowsAlone: a bound row and an unbound pane carry one name;
// the mute is the row's, the pane's notices pass, and unmute ends the row's
// mute and nothing else (NERD-5730, the third review's namesakes).
func TestMuteIsTheBoundRowsAlone(t *testing.T) {
	t.Parallel()
	_, both, bound, unbound := namesakes()
	s := notice.NewMutes(nil)
	_, err := s.Mute(t.Context(), both, "ha-qa-1", time.Hour, t0)
	require.NoError(t, err, "the bound row wins the selection")
	assert.True(t, s.Muted(softNotice(bound), t0))
	assert.False(t, s.Muted(softNotice(unbound), t0), "the namesake is not muted")
	assert.Equal(t, []notice.Mute{{Worker: "ha-qa-1", Until: t0.Add(time.Hour)}}, s.List(t0))
	assert.True(t, s.Unmute("ha-qa-1", t0))
	assert.Empty(t, s.List(t0))
	assert.False(t, s.Unmute("ha-qa-1", t0), "none left")
}

// TestHoldLetsOnlyTheDepartureThrough: a held mute lets its row's Gone notice
// through, silences every other and takes no new mute (NERD-5730).
func TestHoldLetsOnlyTheDepartureThrough(t *testing.T) {
	t.Parallel()
	_, both, bound, _ := namesakes()
	gone := softNotice(bound)
	gone.Kind = notice.Gone
	s := notice.NewMutes(nil)
	_, err := s.Mute(t.Context(), both, "ha-qa-1", time.Hour, t0)
	require.NoError(t, err)
	release := s.Hold("ha-qa-1", 4)
	assert.False(t, s.Muted(gone, t0), "the departure is told")
	assert.True(t, s.Muted(softNotice(bound), t0), "anything else stays muted, the retire may fail")
	_, err = s.Mute(t.Context(), both, "ha-qa-1", time.Hour, t0)
	require.ErrorIs(t, err, notice.ErrUnknownWorker)
	assert.Len(t, s.List(t0), 1, "the mute is held, not cleared")
	release()
	assert.True(t, s.Muted(gone, t0))
	_, err = s.Mute(t.Context(), both, "ha-qa-1", time.Hour, t0)
	require.NoError(t, err)
}

// TestClearWorkerIgnoresNothingAndNil: clearing a row with no mute, or on no
// Mutes at all, is no mute ended.
func TestClearWorkerIgnoresNothingAndNil(t *testing.T) {
	t.Parallel()
	var none *notice.Mutes
	assert.False(t, none.ClearWorker("ha-qa-1", 4, t0))
	assert.False(t, notice.NewMutes(nil).ClearWorker("ha-qa-1", 4, t0))
}

// TestAClearedMuteStillSilencesWhatItSampled: a poll that sampled a notice
// under the mute may batch it after the row ended and the mute was cleared;
// only the departure passes then, and nothing lists or ends the entry
// (NERD-5730).
func TestAClearedMuteStillSilencesWhatItSampled(t *testing.T) {
	t.Parallel()
	_, both, bound, _ := namesakes()
	gone := softNotice(bound)
	gone.Kind = notice.Gone
	s := notice.NewMutes(nil)
	_, err := s.Mute(t.Context(), both, "ha-qa-1", time.Hour, t0)
	require.NoError(t, err)
	require.True(t, s.ClearWorker("ha-qa-1", 4, t0))
	assert.True(t, s.Muted(softNotice(bound), t0), "sampled under the mute, so still muted")
	assert.False(t, s.Muted(gone, t0), "the departure passes")
	assert.Empty(t, s.List(t0))
	assert.False(t, s.Active("ha-qa-1", 4, t0))
	assert.False(t, s.ClearWorker("ha-qa-1", 4, t0), "cleared once")
	assert.False(t, s.Unmute("ha-qa-1", t0), "nothing in force to end")
	assert.False(t, s.Muted(softNotice(bound), t0.Add(2*time.Hour)), "and it ends with the mute")
}

// TestMuteRefusesAnUnboundWorker: only a worker bound to a row can be muted;
// an unbound agent, named or not, is refused and its notices still pass.
func TestMuteRefusesAnUnboundWorker(t *testing.T) {
	t.Parallel()
	_, _, _, unbound := namesakes()
	m := teamOf([]team.Worker{agentPane("w1:pB", "ha-boss", team.StatusWorking)}, unbound,
		team.Worker{ID: "w1:p3", Pane: team.Pane{ID: "w1:p3", Agent: "codex"}})
	for _, name := range []string{"ha-qa-1", "w1:p3"} {
		s := notice.NewMutes(nil)
		_, err := s.Mute(t.Context(), m, name, 0, t0)
		require.ErrorIs(t, err, notice.ErrUnknownWorker, name)
		assert.ErrorContains(t, err, "only a worker bound to a row can be muted", name)
		assert.Empty(t, s.List(t0))
		assert.False(t, s.Muted(softNotice(unbound), t0))
	}
}

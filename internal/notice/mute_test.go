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

// muteTeam is "ha" with its boss, a bound coder, a named one and an unnamed
// one.
func muteTeam() team.Model {
	return teamOf([]team.Worker{agentPane("w1:pB", "ha-boss", team.StatusWorking)},
		team.Worker{ID: "ha-coder-1", Bound: true, Pane: team.Pane{ID: "w1:p1", Name: "ha-coder-1", Agent: "claude"}},
		agentPane("w1:p2", "c2", team.StatusWorking),
		team.Worker{ID: "w1:p3", Pane: team.Pane{ID: "w1:p3", Agent: "codex"}})
}

// about is a notice of kind k about worker.
func about(worker string, k notice.Kind) notice.Notice {
	return notice.Notice{Project: "ha", Subject: "pane of " + worker, Worker: worker, Kind: k, Text: worker + " " + string(k)}
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
// group worker has, the boss's included, are refused and mute no one.
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
		"a named worker's pane":   {"w1:p2", 0, notice.ErrUnknownWorker},
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

// TestMuteTakesEveryName: a bound worker by its logical id, an unbound one
// by its agent's name, an unnamed one by its pane id, in any team.
func TestMuteTakesEveryName(t *testing.T) {
	t.Parallel()
	m := muteTeam()
	m.Projects = append(m.Projects, team.Project{Name: "skills", Groups: []team.Group{{Name: "qa", Workers: []team.Worker{
		agentPane("w2:p1", "sk-qa-1", team.StatusIdle), agentPane("w2:p2", "エージェント", team.StatusIdle), agentPane("w2:p3", "c\n3", team.StatusIdle),
	}}}})
	s := notice.NewMutes(nil)
	for _, w := range []string{"ha-coder-1", "c2", "w1:p3", "sk-qa-1", "エージェント", "c\n3"} {
		_, err := s.Mute(t.Context(), m, w, 0, t0)
		require.NoError(t, err, w)
	}
	assert.Len(t, s.List(t0), 6)
	for _, w := range []string{"c 3", "c3", "エージェント ", "HA-CODER-1"} {
		_, err := s.Mute(t.Context(), m, w, 0, t0)
		require.ErrorIs(t, err, notice.ErrUnknownWorker, "%q: a name is matched byte for byte, as herdr reports it", w)
	}
	assert.True(t, s.Muted(notice.Notice{Worker: "c\n3", Kind: notice.Blocked}, t0), "a notice names it as herdr does")
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
	for w, d := range map[string]time.Duration{"w1:p3": time.Hour, "c2": time.Minute, "ha-coder-1": 2 * time.Hour} {
		_, err := s.Mute(t.Context(), muteTeam(), w, d, t0)
		require.NoError(t, err)
	}
	assert.Equal(t, []notice.Mute{
		{Worker: "c2", Until: t0.Add(time.Minute)},
		{Worker: "ha-coder-1", Until: t0.Add(2 * time.Hour)},
		{Worker: "w1:p3", Until: t0.Add(time.Hour)},
	}, s.List(t0))
	assert.Equal(t, []notice.Mute{
		{Worker: "ha-coder-1", Until: t0.Add(2 * time.Hour)},
		{Worker: "w1:p3", Until: t0.Add(time.Hour)},
	}, s.List(t0.Add(time.Minute)))
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
	assert.False(t, s.ClearWorker(id, 0, t0), "a late clear cannot remove the next generation's mute")
	assert.Equal(t, []notice.Mute{fresh}, s.List(t0))
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

package attentioninput_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/attention"
	"github.com/abanna/herdr-agentisan/internal/daemon/attentioninput"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

func TestBoardAlertInputsProjectSettings(t *testing.T) {
	t.Parallel()

	shared := settings.AttentionConfig{
		After: time.Minute, CeilingAfter: 2 * time.Minute,
		Every: 3 * time.Minute, MaxEvery: 4 * time.Minute, BusyGrace: 5 * time.Minute,
		Alerts: 6, Sound: settings.SoundRequest,
	}
	own := settings.AttentionConfig{
		After: 0, CeilingAfter: 7 * time.Minute,
		Every: 8 * time.Minute, MaxEvery: 9 * time.Minute, BusyGrace: 10 * time.Minute,
		Alerts: 11, Sound: settings.SoundDone,
	}
	resolved := settings.NoticeSettings{
		Default:  settings.NoticeConfig{Attention: shared},
		Projects: map[string]settings.NoticeConfig{"studio": {Attention: own}},
	}

	require.Equal(t, attention.Config(shared), attentioninput.NewConfigs(resolved).For("other"))
	require.Equal(t, attention.Config(own), attentioninput.NewConfigs(resolved).For("studio"))
}

func TestBoardAlertInputsPendingAndBossPlace(t *testing.T) {
	t.Parallel()

	at := time.Unix(1000, 0).UTC()
	pending := store.BoardPendingSnapshot{
		Pending: 2, Oldest: at, OldestCeiling: at.Add(time.Minute), BossMark: 33,
		Held: 4, OldestHeld: at.Add(2 * time.Minute), OldestHeldCeiling: at.Add(3 * time.Minute),
		NewestCeiling: at.Add(5 * time.Minute), NewestCeilingPost: at.Add(4 * time.Minute),
	}
	built := &team.Model{Projects: []team.Project{
		{Name: "empty"},
		{Name: "other", Boss: []team.Worker{{Pane: team.Pane{Name: "boss", Agent: "codex", Status: team.StatusIdle}}}},
		{Name: "studio", Boss: []team.Worker{
			{Pane: team.Pane{Name: "unbound", Status: team.StatusDone}},
			{Pane: team.Pane{Name: "boss", Agent: "claude", Status: team.StatusWorking}},
		}},
	}}

	got := attentioninput.State(pending, built, "studio", "boss")
	for field := range reflect.TypeFor[store.BoardPendingSnapshot]().Fields() {
		value := reflect.ValueOf(pending).FieldByName(field.Name)
		require.False(t, value.IsZero(), "test must set %s", field.Name)
		mapped := reflect.ValueOf(got).FieldByName(field.Name)
		require.True(t, mapped.IsValid(), "attention state must carry %s", field.Name)
		require.Equal(t, value.Interface(), mapped.Interface(), field.Name)
	}
	require.Equal(t, "boss", got.Boss)
	require.True(t, got.Place)
	require.Equal(t, team.StatusWorking, got.Status)
	require.Equal(t, "claude", got.Agent)

	unnamed := attentioninput.State(pending, built, "studio", "missing")
	require.True(t, unnamed.Place)
	require.Equal(t, team.StatusUnknown, unnamed.Status)
	require.Empty(t, unnamed.Agent)

	empty := attentioninput.State(pending, built, "empty", "boss")
	require.False(t, empty.Place, "a project with no boss pane has no place")

	for _, model := range []*team.Model{nil, built} {
		absent := attentioninput.State(pending, model, "absent", "boss")
		require.False(t, absent.Place)
		require.Equal(t, team.StatusUnknown, absent.Status)
		require.Empty(t, absent.Agent)
	}
}

func TestBoardAlertInputsDefaultCadenceAndSound(t *testing.T) {
	t.Parallel()

	resolved, err := settings.Notices("")
	require.NoError(t, err)
	cfg := attentioninput.NewConfigs(resolved).For("studio")
	at := time.Unix(1000, 0).UTC()
	state := attention.State{Boss: "boss", Place: true, Pending: 1, Oldest: at}

	tracker := attention.NewTracker()
	var minutes []int
	for minute := 0; minute <= 125; minute++ {
		now := at.Add(time.Duration(minute) * time.Minute)
		if alert, due := tracker.Observe("studio", cfg, state, now); due {
			require.Equal(t, settings.SoundRequest, alert.Sound)
			minutes = append(minutes, minute)
			tracker.Shown("studio", true, now)
		}
	}
	require.Equal(t, []int{10, 15, 25, 45, 65, 85, 105, 125}, minutes)

	state.Status, state.Agent = team.StatusWorking, "claude"
	busy := attention.NewTracker()
	_, due := busy.Observe("studio", cfg, state, at.Add(29*time.Minute))
	require.False(t, due)
	_, due = busy.Observe("studio", cfg, state, at.Add(30*time.Minute))
	require.True(t, due)

	state.OldestCeiling, state.NewestCeiling = at, at
	ceiling := attention.NewTracker()
	_, due = ceiling.Observe("studio", cfg, state, at.Add(time.Minute))
	require.False(t, due)
	alert, due := ceiling.Observe("studio", cfg, state, at.Add(2*time.Minute))
	require.True(t, due)
	require.True(t, alert.Ceiling)
}

func TestBoardAlertInputsEachPostedCeilingPastLimit(t *testing.T) {
	t.Parallel()

	at := time.Unix(1000, 0).UTC()
	clock := func(minute int) time.Time { return at.Add(time.Duration(minute) * time.Minute) }
	built := &team.Model{Projects: []team.Project{{Name: "studio", Boss: []team.Worker{
		{Pane: team.Pane{Name: "boss", Status: team.StatusIdle}},
	}}}}
	cfg := attention.Config{
		After: 10 * time.Minute, CeilingAfter: 2 * time.Minute,
		Every: 5 * time.Minute, MaxEvery: 20 * time.Minute, Alerts: 1,
	}
	tracker := attention.NewTracker()
	pending := store.BoardPendingSnapshot{Pending: 1, Oldest: at}
	observe := func(minute int) (attention.Alert, bool) {
		state := attentioninput.State(pending, built, "studio", "boss")
		return tracker.Observe("studio", cfg, state, clock(minute))
	}

	_, due := observe(10)
	require.True(t, due, "the ordinary first alert")
	tracker.Shown("studio", true, clock(10))
	pending.Pending, pending.OldestCeiling = 2, clock(20)
	pending.NewestCeiling, pending.NewestCeilingPost = clock(20), clock(20)
	_, due = observe(22)
	require.True(t, due, "the first warning is still unread")
	tracker.Shown("studio", true, clock(22))
	pending.Pending, pending.NewestCeiling, pending.NewestCeilingPost = 3, clock(40), clock(40)
	_, due = observe(41)
	require.False(t, due)
	alert, due := observe(42)
	require.True(t, due, "a second directly posted warning gets its own alert")
	require.Equal(t, 3, alert.N)
	tracker.Shown("studio", true, clock(42))
	_, due = observe(43)
	require.False(t, due, "the second warning has already alerted")
}

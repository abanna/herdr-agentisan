package workforce_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// mixedConfig is teamConfig with a codex agent of its own beside the
// top-level claude, so either provider can run every worker type but the
// debugger and review (A62).
var mixedConfig = strings.Replace(teamConfig, "[projects.herdr-agentisan]\n",
	"[agent.codex]\ncommand = [\"codex\", \"--search\"]\n\n[projects.herdr-agentisan]\n", 1)

// modelsConfig is mixedConfig with a model for each provider, the claude one
// at the top level and the codex one in [agent.codex]: a spawn of codex
// takes codex's (A62), never the type's default provider's.
var modelsConfig = strings.Replace(mixedConfig, "[agent.codex]\ncommand = [\"codex\", \"--search\"]\n",
	"[agent]\nmodel = \"claude-opus-5-5\"\n\n[agent.codex]\ncommand = [\"codex\", \"--search\"]\nmodel = \"gpt-6.1-sol\"\n", 1)

// TestSpawnRunsTheProviderAsked: --provider runs that agent in a group whose
// default is the other, with routing off or deciding, and each row records
// the provider its worker runs (ADR-001 A66); with no --provider the packet's
// provider_preference chooses, else the type's default.
func TestSpawnRunsTheProviderAsked(t *testing.T) {
	t.Parallel()
	found := &routing.Task{LookupStatus: routing.LookupFound}
	preferring := func(provider string) *routing.Task {
		t := *found
		t.ProviderPreference = provider
		return &t
	}
	for name, tc := range map[string]struct {
		config string
		req    workforce.SpawnRequest
		kind   string
		args   []any
		// routed is the decision's binding, "" while routing is off.
		routed string
	}{
		"codex asked, routing off": {
			config: mixedConfig, req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex"},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access"},
		},
		"codex asked, routing on": {
			config: mixedConfig + "\n[routing]\nenabled = true\n", req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex"},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="high"`},
			routed: "codex_strong",
		},
		"codex asked with its own binding": {
			config: mixedConfig + "\n[routing]\nenabled = true\n", req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex", Model: "codex_strong"},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="high"`},
			routed: "codex_strong",
		},
		"the packet prefers codex, routing off": {
			config: mixedConfig, req: workforce.SpawnRequest{Group: "coder", Count: 1, Task: preferring("codex")},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access"},
		},
		"the packet prefers codex, routing on": {
			config: mixedConfig + "\n[routing]\nenabled = true\n", req: workforce.SpawnRequest{Group: "coder", Count: 1, Task: preferring("codex")},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="high"`},
			routed: "codex_strong",
		},
		"--provider and the packet agree": {
			config: mixedConfig, req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex", Task: preferring("codex")},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access"},
		},
		"claude asked in a codex group": {
			config: strings.Replace(mixedConfig, "[projects.herdr-agentisan.groups.coder]\n", "[projects.herdr-agentisan.groups.coder]\nagent = { provider = \"codex\" }\n", 1),
			req:    workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "claude"},
			kind:   "claude", args: withHooksAny("--name", "ha-coder-1"),
		},
		"codex asked, each provider's model, routing off": {
			config: modelsConfig, req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex"},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-6.1-sol"},
		},
		"codex asked with its model, routing off": {
			config: modelsConfig, req: workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex", Model: "gpt-6.1-sol"},
			kind: "codex", args: []any{"--search", "--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-6.1-sol"},
		},
		"nothing asked: the type's default": {
			config: mixedConfig, req: workforce.SpawnRequest{Group: "coder", Count: 1, Task: found},
			kind: "claude", args: withHooksAny("--name", "ha-coder-1"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := routedFixture(t, tc.config)
			res, err := f.svc.Spawn(t.Context(), tc.req)
			require.NoError(t, err)
			require.Len(t, res.Workers, 1)
			assert.Equal(t, tc.kind, res.Workers[0].Agent)
			starts := f.h.params("agent.start")
			require.Len(t, starts, 1)
			assert.Equal(t, tc.kind, starts[0]["kind"])
			assert.Equal(t, tc.args, starts[0]["args"])
			assert.Equal(t, tc.kind, f.row(t, "ha-coder-1").Provider, "the row records the provider it runs")

			routed := rec.all()
			require.Len(t, routed, 1)
			d := routed[0].Decision
			require.NotNil(t, d, "a decision is made, applied or not")
			assert.Equal(t, tc.kind, d.Provider, "routing decided for the provider asked")
			assert.Equal(t, tc.routed != "", d.Applied)
			if tc.routed != "" {
				assert.Equal(t, tc.routed, d.SelectedBinding)
				assert.Equal(t, tc.kind, routingOf(t, f, "ha-coder-1").Provider)
			}
		})
	}
}

// TestSpawnRefusesAProviderTheTypeCannotRun: --provider for a type that
// cannot run it yet is provider_refused's sentinel, naming what lifts the
// gap, before anything opens (A62, A66).
func TestSpawnRefusesAProviderTheTypeCannotRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t, mixedConfig)
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "review", Count: 1, Provider: "claude"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NERD-5529")
	assert.NotErrorIs(t, err, workforce.ErrConfig, "the boss's choice, not the configuration's")
	assert.Empty(t, f.h.params("tab.create"), "nothing opens")
}

// TestAMixedGroupSharesItsSlots: a group's slots count every worker of it,
// whichever provider each runs, so a claude and a codex worker fill a group
// of two, and a third of either is refused (A66).
func TestAMixedGroupSharesItsSlots(t *testing.T) {
	t.Parallel()
	f := newFixture(t, strings.Replace(mixedConfig, "workers = 2\nslots = 3\n", "workers = 2\nslots = 2\n", 1))
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
	_, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex"})
	require.NoError(t, err)
	assert.Equal(t, "claude", f.row(t, "ha-coder-1").Provider)
	assert.Equal(t, "codex", f.row(t, "ha-coder-2").Provider)

	for _, provider := range []string{"claude", "codex"} {
		_, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Provider: provider})
		require.ErrorIs(t, err, workforce.ErrSlotCap, provider)
		assert.Contains(t, err.Error(), "2 occupied")
	}
	st, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	for _, g := range st.Groups {
		if g.Name == "coder" {
			assert.Equal(t, 2, g.Occupied)
			assert.Equal(t, 0, g.Open)
			require.Len(t, g.Members, 2)
			assert.Equal(t, []string{"claude", "codex"}, []string{g.Members[0].Provider, g.Members[1].Provider})
			assert.Equal(t, []string{"claude", "codex"}, []string{g.Members[0].Agent, g.Members[1].Agent}, "herdr's detected agent beside it")
		}
	}
}

// TestTeamUpCountsAMixedGroup: team up fills a group to its count with the
// type's default provider, counting the workers of the other provider
// already there, and records each row's own provider (A66; counts per
// provider are NERD-5521's).
func TestTeamUpCountsAMixedGroup(t *testing.T) {
	t.Parallel()
	f := newFixture(t, mixedConfig)
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex"})
	require.NoError(t, err)

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	var coders []workforce.Spawned
	for _, w := range res.Workers {
		if w.Group == "coder" {
			coders = append(coders, w)
		}
	}
	require.Len(t, coders, 1, "one more coder makes the group's two")
	assert.Equal(t, "claude", coders[0].Agent)
	assert.Equal(t, map[string]string{"ha-coder-1": "codex", "ha-coder-2": "claude", "ha-qa-1": "claude"}, map[string]string{
		"ha-coder-1": f.row(t, "ha-coder-1").Provider, "ha-coder-2": f.row(t, "ha-coder-2").Provider, "ha-qa-1": f.row(t, "ha-qa-1").Provider,
	})
}

// TestRetireAWorkerOfTheOtherProvider: a codex worker in a group whose
// default is claude retires as any other, its pane closed, and its row keeps
// the provider it ran; team status shows an unbound row's provider too
// (A66).
func TestRetireAWorkerOfTheOtherProvider(t *testing.T) {
	t.Parallel()
	f := newFixture(t, mixedConfig)
	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 2, Provider: "codex"})
	require.NoError(t, err)
	require.Len(t, res.Workers, 2)

	out, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "ha-coder-1"})
	require.NoError(t, err)
	assert.Equal(t, workforce.OutcomeRetired, out.Outcome)
	assert.Equal(t, []map[string]any{{"pane_id": res.Workers[0].PaneID}}, f.h.params("pane.close"))
	row := f.row(t, "ha-coder-1")
	assert.Equal(t, store.StatusRetired, row.Status)
	assert.Equal(t, "codex", row.Provider)

	f.markLost(t, "ha-coder-2")
	st, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	require.Len(t, st.Unbound, 1)
	assert.Equal(t, workforce.UnboundWorker{ID: "ha-coder-2", Group: "coder", PaneID: res.Workers[1].PaneID, Status: store.StatusLost, Provider: "codex"}, st.Unbound[0])
}

// TestSpawnRefusesAPacketItsProviderDisagreesWith: --provider and a packet
// that prefers the other provider are refused before anything opens, with
// routing off too: the provider is the spawn's choice, so the refusal is
// never only recorded as routing's own are (A66).
func TestSpawnRefusesAPacketItsProviderDisagreesWith(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, mixedConfig)
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{
		Group: "coder", Count: 1, Provider: "codex",
		Task: &routing.Task{LookupStatus: routing.LookupFound, ProviderPreference: "claude"},
	})
	require.ErrorIs(t, err, routing.ErrProviderMismatch)
	assert.Contains(t, err.Error(), "--provider is codex, and the task packet's provider_preference is claude")
	assert.Empty(t, f.h.params("tab.create"), "nothing opens")
	assert.Empty(t, rec.all(), "nothing is routed")
}

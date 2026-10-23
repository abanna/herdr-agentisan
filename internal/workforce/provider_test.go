package workforce_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestSpawnRunsTheGroupsProvider: a group whose provider is codex spawns
// codex from [agent.codex], or its own codex table, with that provider's
// model, while the top-level command stays claude for every other type; team
// up fills it the same way (ADR-001 A62).
func TestSpawnRunsTheGroupsProvider(t *testing.T) {
	t.Parallel()
	config := strings.Replace(teamConfig, "[projects.herdr-agentisan]\n", "[agent.codex]\ncommand = [\"codex\", \"--search\"]\nmodel = \"gpt-6.1-sol\"\n\n[projects.herdr-agentisan]\n", 1)
	config = strings.Replace(config, "[projects.herdr-agentisan.groups.coder]\n", "[projects.herdr-agentisan.groups.coder]\nagent = { provider = \"codex\" }\n", 1)
	f := newFixture(t, config)

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1})
	require.NoError(t, err)
	require.Len(t, res.Workers, 1)
	assert.Equal(t, "codex", res.Workers[0].Agent)
	starts := f.h.params("agent.start")
	require.Len(t, starts, 1)
	assert.Equal(t, "codex", starts[0]["kind"])
	assert.Equal(t, []any{"--search", "--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-6.1-sol"}, starts[0]["args"], "with the coder's sandbox (A59)")

	_, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "qa", Count: 1})
	require.NoError(t, err)
	starts = f.h.params("agent.start")
	assert.Equal(t, "claude", starts[len(starts)-1]["kind"], "qa keeps the top-level claude")
	assert.Equal(t, withHooksAny("--name", "ha-qa-1"), starts[len(starts)-1]["args"], "and none of codex's model")

	own := config + "\n[projects.herdr-agentisan.groups.coder.agent.codex]\ncommand = [\"codex\", \"-c\", \"x=1\"]\nmodel = \"gpt-5.5\"\n"
	own = strings.Replace(own, "agent = { provider = \"codex\" }\n", "", 1)
	own = strings.Replace(own, "[projects.herdr-agentisan.groups.coder.agent.codex]", "[projects.herdr-agentisan.groups.coder.agent]\nprovider = \"codex\"\n[projects.herdr-agentisan.groups.coder.agent.codex]", 1)
	g := newFixture(t, own)
	_, err = g.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	var codex []map[string]any
	for _, s := range g.h.params("agent.start") {
		if s["kind"] == "codex" {
			codex = append(codex, s)
		}
	}
	require.Len(t, codex, 2, "team up fills the coder group, both codex")
	for _, s := range codex {
		assert.Equal(t, []any{"-c", "x=1", "--no-daemon", "--sandbox", "danger-full-access", "--model", "gpt-5.5"}, s["args"], "the group's own codex table")
	}
}

// TestSpawnRefusesAModelOfTheOtherProvider: with routing off, a spawn's
// --model, or the group's, whose name says the other provider runs it is
// refused before anything opens, naming both providers and never the model;
// a name the plugin does not know passes (A62).
func TestSpawnRefusesAModelOfTheOtherProvider(t *testing.T) {
	t.Parallel()
	codexQA := strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = { command = [\"codex\"] }\n", 1)
	for name, tc := range map[string]struct {
		config string
		req    workforce.SpawnRequest
		says   string
	}{
		"a claude model for codex": {config: codexQA, req: workforce.SpawnRequest{Group: "qa", Count: 1, Model: "claude-opus-5-5"}, says: "qa's agent model is a claude model, and its agent runs codex"},
		"a codex model for claude": {config: teamConfig, req: workforce.SpawnRequest{Group: "coder", Count: 1, Model: "gpt-6.1-sol"}, says: "coder's agent model is a codex model, and its agent runs claude"},
		"the group's own model":    {config: strings.Replace(codexQA, "command = [\"codex\"] }", "command = [\"codex\"], model = \"sonnet\" }", 1), req: workforce.SpawnRequest{Group: "qa", Count: 1}, says: "a claude model"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.config)
			_, err := f.svc.Spawn(t.Context(), tc.req)
			require.ErrorIs(t, err, workforce.ErrConfig)
			assert.Contains(t, err.Error(), tc.says)
			assert.NotContains(t, err.Error(), "opus-5-5")
			assert.NotContains(t, err.Error(), "6.1-sol")
			assert.Empty(t, f.h.params("tab.create"), "nothing opens")
		})
	}

	f := newFixture(t, codexQA)
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "qa", Count: 1, Model: "llama3"})
	require.NoError(t, err, "a model the plugin does not know passes")
	assert.Equal(t, []any{"--no-daemon", "--sandbox", "danger-full-access", "--model", "llama3"}, f.h.params("agent.start")[0]["args"])
}

// TestPlanRefusesAProviderTheTypeCannotRun: a profile Load did not build,
// which would have refused it first, still cannot launch the debugger under
// codex or review under claude (A62).
func TestPlanRefusesAProviderTheTypeCannotRun(t *testing.T) {
	t.Parallel()
	p := &settings.Profile{
		Agent:  settings.Agent{Command: []string{"claude"}},
		Groups: map[string]settings.Group{settings.ReviewType: {Provider: "claude"}},
	}
	_, _, err := workforce.Plan(p, settings.ReviewType, "", "")
	require.ErrorIs(t, err, workforce.ErrConfig)
	require.ErrorIs(t, err, settings.ErrProviderRefused)
	assert.Contains(t, err.Error(), "NERD-5529")
}

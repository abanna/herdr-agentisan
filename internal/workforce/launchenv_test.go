package workforce_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// Claude Code's two switches, spelled out here so that renaming either in
// the code fails these tests (NERD-5650, A72).
const (
	agentViewOff   = "CLAUDE_CODE_DISABLE_AGENT_VIEW"
	suggestionsOff = "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION"
)

// claudeEnv is the environment a Claude worker's tab is opened with.
func claudeEnv(id string) map[string]any {
	return map[string]any{workforce.WorkerEnv: id, agentViewOff: "1", suggestionsOff: "0"}
}

// tabEnv is the environment the tab (or workspace) labelled label was
// opened with.
func tabEnv(t *testing.T, f *fixture, method, label string) map[string]any {
	t.Helper()
	for _, p := range f.h.params(method) {
		if p["label"] == label {
			env, ok := p["env"].(map[string]any)
			require.True(t, ok, "%s %s has an env", method, label)
			return env
		}
	}
	require.Failf(t, "no such tab", "%s opened no %s", method, label)
	return nil
}

// TestSpawnTurnsOffTheAgentsViewAndSuggestionsForClaude: every Claude worker
// spawn opens gets both switches in its tab's environment, whatever its
// command passes, so a left arrow cannot fork its conversation into a
// background session and no suggestion greys its composer; a Codex worker
// gets neither (NERD-5650, A72).
func TestSpawnTurnsOffTheAgentsViewAndSuggestionsForClaude(t *testing.T) {
	t.Parallel()
	own := strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = { command = [\"claude\", \"--settings\", \"/etc/mine.json\"] }\n", 1)
	codex := strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.qa]\n", "[projects.herdr-agentisan.groups.qa]\nagent = { command = [\"codex\"] }\n", 1)
	for name, tc := range map[string]struct {
		config string
		group  string
		id     string
		want   map[string]any
	}{
		"claude":                   {config: teamConfig, group: "coder", id: "ha-coder-1", want: claudeEnv("ha-coder-1")},
		"claude with its own file": {config: own, group: "qa", id: "ha-qa-1", want: claudeEnv("ha-qa-1")},
		"codex":                    {config: codex, group: "qa", id: "ha-qa-1", want: map[string]any{workforce.WorkerEnv: "ha-qa-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, tc.config)
			_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: tc.group, Count: 1})
			require.NoError(t, err)
			assert.Equal(t, tc.want, tabEnv(t, f, "tab.create", tc.id))
		})
	}
}

// TestUpGivesTheSwitchesToClaudeWorkersOnly: team up opens a mixed team with
// the switches on each Claude worker's tab and on no Codex worker's.
func TestUpGivesTheSwitchesToClaudeWorkersOnly(t *testing.T) {
	t.Parallel()
	config := strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.coder]\n", "[projects.herdr-agentisan.groups.coder]\nagent = { command = [\"codex\"] }\n", 1)
	f := newFixture(t, config)
	_, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{workforce.WorkerEnv: "ha-coder-1"}, tabEnv(t, f, "tab.create", "ha-coder-1"))
	assert.Equal(t, map[string]any{workforce.WorkerEnv: "ha-coder-2"}, tabEnv(t, f, "tab.create", "ha-coder-2"))
	assert.Equal(t, claudeEnv("ha-qa-1"), tabEnv(t, f, "tab.create", "ha-qa-1"))
}

package workforce_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// Both packet directions must choose independently of the group's default.
func TestQA146PacketChoosesClaudeInCodexGroup(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "routing_off", true: "routing_on"}[on], func(t *testing.T) {
			config := strings.Replace(mixedConfig, "[projects.herdr-agentisan.groups.coder]\n", "[projects.herdr-agentisan.groups.coder]\nagent = { provider = \"codex\" }\n", 1)
			if on {
				config += "\n[routing]\nenabled = true\n"
			}
			f, rec := routedFixture(t, config)
			out, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Task: &routing.Task{LookupStatus: routing.LookupFound, ProviderPreference: "claude"}})
			require.NoError(t, err)
			require.Len(t, out.Workers, 1)
			assert.Equal(t, "claude", out.Workers[0].Agent)
			assert.Equal(t, "claude", f.row(t, out.Workers[0].ID).Provider)
			starts := f.h.params("agent.start")
			require.Len(t, starts, 1)
			assert.Equal(t, "claude", starts[0]["kind"])
			records := rec.all()
			require.Len(t, records, 1)
			require.NotNil(t, records[0].Decision)
			assert.Equal(t, "claude", records[0].Decision.Provider)
		})
	}
}

// The durable launch provider and the current detected agent are separate facts.
func TestQA146StatusKeepsLaunchProviderWhenAgentChanges(t *testing.T) {
	f := newFixture(t, mixedConfig)
	out, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 1, Provider: "codex"})
	require.NoError(t, err)
	require.Len(t, out.Workers, 1)
	f.h.mu.Lock()
	f.h.pane(out.Workers[0].PaneID).agent = "claude"
	f.h.mu.Unlock()
	status, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	found := false
	for _, g := range status.Groups {
		for _, m := range g.Members {
			if m.ID == out.Workers[0].ID {
				found = true
				assert.Equal(t, "codex", m.Provider)
				assert.Equal(t, "claude", m.Agent)
			}
		}
	}
	require.True(t, found, "the bound worker is reported")
}

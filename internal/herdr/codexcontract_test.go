package herdr_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// TestCodexAgentGetContract: herdr v0.9.5's agent_info for a Codex agent, at
// each status it publishes for one, decodes to what the readiness wait
// reads. Each reply is shaped as herdr serialises AgentInfo
// (src/api/schema/agents.rs:187-229: a false flag, and an agent or a
// completion herdr has none of, left out), its status is pane_agent_status
// of the terminal's state and whether its pane was seen
// (src/app/api_helpers.rs:96-107), and each row names the source lines that
// yield it, at the tag, with Codex's rules from its embedded manifest
// src/detect/manifests/codex.toml (2026.10.11.1). A quoted reply cannot fail
// when herdr changes: the protocol pin and the live checks find that. It
// pins the plugin's side to herdr's reply as quoted (Codex parity R32).
//
// Codex's transcript_viewer rule (codex.toml:22-33) has no row: it sets
// skip_state_update (:27), and herdr drops such a detection
// (src/pane/agent_detection.rs:360), so it never publishes a status.
func TestCodexAgentGetContract(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reply string
		want  herdr.Agent
	}{
		// Right after agent.start: the managed launch is pending
		// (src/terminal/state.rs:2162-2169), no agent is detected yet, so
		// the label is unset (src/app/creation.rs:354) and the state
		// Unknown (api_helpers.rs:105).
		"launch pending, not yet detected": {
			reply: `{"terminal_id":"term-w14:p7","name":"ha-coder-3","agent_status":"unknown","workspace_id":"w14","tab_id":"w14:t2","pane_id":"w14:p7","focused":false,"launch_pending":true,"state_change_seq":0,"revision":1}`,
			want:  herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-3", AgentStatus: "unknown", LaunchPending: true},
		},
		// Codex detected and blocked while its launch is pending: the
		// folder-trust or update screen (codex.toml:34-56, 57-76) or an
		// Action Required title (:6-13). The phase goes Blocked
		// (state.rs:2248-2254), which is still pending (:2162-2169).
		"blocked during startup": {
			reply: `{"terminal_id":"term-w14:p7","name":"ha-coder-3","agent":"codex","agent_status":"blocked","workspace_id":"w14","tab_id":"w14:t2","pane_id":"w14:p7","focused":false,"launch_pending":true,"state_change_seq":2,"revision":3}`,
			want:  herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-3", Agent: "codex", AgentStatus: "blocked", LaunchPending: true},
		},
		// Codex's composer is up and its screen reads Unknown: Codex alone
		// goes Active on Unknown with the composer seen (state.rs:2259-2272;
		// from Blocked, :2226-2241), and Active is interactive_ready
		// (:2171-2174).
		"ready, composer up and unknown": {
			reply: `{"terminal_id":"term-w14:p7","name":"ha-coder-3","agent":"codex","agent_status":"unknown","workspace_id":"w14","tab_id":"w14:t2","pane_id":"w14:p7","focused":false,"interactive_ready":true,"state_change_seq":3,"revision":4}`,
			want:  herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-3", Agent: "codex", AgentStatus: "unknown", InteractiveReady: true},
		},
		// Idle and seen: the OSC title with no spinner (codex.toml:119-128),
		// or no rule at all for a known agent (src/detect/manifest.rs:
		// 550-554), as Codex's hook-review screen. Any state but Idle marks
		// the pane seen (src/app/actions.rs:1844-1845), and an edge into
		// Idle from Unknown is no completion (:22-25), so it reads idle
		// (api_helpers.rs:102), and Idle makes the launch Active
		// (state.rs:2259-2272).
		"idle": {
			reply: `{"terminal_id":"term-w14:p7","name":"ha-coder-3","agent":"codex","agent_status":"idle","workspace_id":"w14","tab_id":"w14:t2","pane_id":"w14:p7","focused":false,"interactive_ready":true,"state_change_seq":4,"revision":5}`,
			want:  herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-3", Agent: "codex", AgentStatus: "idle", InteractiveReady: true},
		},
		// A turn: the OSC title's spinner (codex.toml:14-21) or the
		// interrupt timer on screen (:105-118). An edge that is no
		// completion clears completion_seq (actions.rs:1755-1763).
		"working": {
			reply: `{"terminal_id":"term-w14:p7","name":"ha-coder-3","agent":"codex","agent_status":"working","workspace_id":"w14","tab_id":"w14:t2","pane_id":"w14:p7","focused":false,"interactive_ready":true,"state_change_seq":5,"revision":6}`,
			want:  herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-3", Agent: "codex", AgentStatus: "working", InteractiveReady: true},
		},
		// Blocked in a turn: an Action Required title (codex.toml:6-13), a
		// question to answer (:77-90) or approval wording (:91-104). The
		// launch stays Active, so it is not pending.
		"blocked in a turn": {
			reply: `{"terminal_id":"term-w14:p7","name":"ha-coder-3","agent":"codex","agent_status":"blocked","workspace_id":"w14","tab_id":"w14:t2","pane_id":"w14:p7","focused":false,"interactive_ready":true,"state_change_seq":6,"revision":7}`,
			want:  herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-3", Agent: "codex", AgentStatus: "blocked", InteractiveReady: true},
		},
		// A turn's end: Working or Blocked into Idle is a completion
		// (actions.rs:22-25), which sets completion_seq (:1755-1763) and
		// leaves the pane unseen outside the focused tab (:1846-1847), so
		// it reads done (api_helpers.rs:101) until viewed.
		"done": {
			reply: `{"terminal_id":"term-w14:p7","name":"ha-coder-3","agent":"codex","agent_status":"done","workspace_id":"w14","tab_id":"w14:t2","pane_id":"w14:p7","focused":false,"interactive_ready":true,"state_change_seq":7,"completion_seq":7,"revision":8}`,
			want:  herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-3", Agent: "codex", AgentStatus: "done", InteractiveReady: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result := json.RawMessage(`{"type":"agent_info","agent":` + tc.reply + `}`)
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: result} })

			got, err := herdr.Client{SocketPath: srv.Path}.GetAgent(t.Context(), "ha-coder-3")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

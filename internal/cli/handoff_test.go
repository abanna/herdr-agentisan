package cli_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// TestHandoffReadyAsksTheDaemon: handoff ready sends its op with the target
// and --clear, and herdr is never asked; with no target, with the caller's
// pane as workforce.FindCallerPane confirms it and HERDR_AGENTISAN_WORKER.
// It prints the daemon's answer as the envelope.
func TestHandoffReadyAsksTheDaemon(t *testing.T) {
	t.Parallel()
	failing := func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "internal", Message: "boom"}}
	}
	const ok = `{"ok":true,"data":{"id":"ha-coder-1","bound":true,"project":"ha","group":"coder","pane_id":"w14:p1","ready":true,"changed":true}}`
	const notAWorker = `{"ok":false,"error":{"code":"not_a_worker","message":"w14:p1 is in the boss's place"}}`
	tests := map[string]struct {
		args  []string
		env   map[string]string
		herdr herdrtest.Handler
		reply string
		sent  string
	}{
		"a target, herdr unasked": {
			args: []string{"handoff", "ready", "ha-coder-1"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1", "HERDR_AGENTISAN_WORKER": "ha-coder-2"}, herdr: failing, reply: ok,
			sent: `{"v":1,"op":"handoff.ready","args":{"target":"ha-coder-1"}}`,
		},
		"a pane id, cleared": {
			args: []string{"handoff", "ready", "w14:p2", "--clear"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, herdr: failing, reply: ok,
			sent: `{"v":1,"op":"handoff.ready","args":{"target":"w14:p2","clear":true}}`,
		},
		"a target like a flag, after --": {
			args: []string{"handoff", "ready", "--", "-x"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, herdr: failing, reply: ok,
			sent: `{"v":1,"op":"handoff.ready","args":{"target":"-x"}}`,
		},
		"the caller's pane": {
			args: []string{"handoff", "ready"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1", "HERDR_AGENTISAN_WORKER": "ha-coder-1"}, reply: ok,
			sent: `{"v":1,"op":"handoff.ready","args":{"caller_pane":"w14:p1","worker":"ha-coder-1"}}`,
		},
		"a frozen alias sends the pane's current id": {
			args: []string{"handoff", "ready", "--clear"}, env: map[string]string{"HERDR_PANE_ID": "w9:p9"}, reply: ok,
			sent: `{"v":1,"op":"handoff.ready","args":{"clear":true,"caller_pane":"w14:p1"}}`,
		},
		"no pane, the worker's variable": {
			args: []string{"handoff", "ready"}, env: map[string]string{"HERDR_PANE_ID": "", "HERDR_AGENTISAN_WORKER": "ha-coder-1"}, reply: ok,
			sent: `{"v":1,"op":"handoff.ready","args":{"worker":"ha-coder-1"}}`,
		},
		"a refusal is printed with its code": {
			args: []string{"handoff", "ready"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, reply: notAWorker,
			sent: `{"v":1,"op":"handoff.ready","args":{"caller_pane":"w14:p1"}}`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := tc.herdr
			if h == nil {
				h = paneHerdr("w14:p1", "w9:p9")
			}
			e := newDaemonEnvWith(t, h)
			sent := fakeDaemon(t, e, tc.reply)
			out, _, err := runOut(t, e.lookup(tc.env), tc.args...)
			if strings.Contains(tc.reply, `"ok":true`) {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a refusal exits 1")
			}
			assert.JSONEq(t, tc.reply, out)
			oneJSONObject(t, out)
			require.Len(t, sent, 1)
			assert.JSONEq(t, tc.sent, <-sent)
		})
	}
}

// TestHandoffReadyRefusesBeforeAsking: a command line handoff ready cannot
// take, no daemon, and a herdr that fails finding the caller's pane are
// refused in one JSON object, exit 1, and the daemon is never asked.
func TestHandoffReadyRefusesBeforeAsking(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args   []string
		env    map[string]string
		daemon bool
		code   string
	}{
		"a target like a flag":   {args: []string{"handoff", "ready", "-x"}, daemon: true, code: "bad_request"},
		"two targets":            {args: []string{"handoff", "ready", "ha-coder-1", "ha-coder-2"}, daemon: true, code: "bad_request"},
		"an empty target":        {args: []string{"handoff", "ready", ""}, daemon: true, code: "bad_request"},
		"a project":              {args: []string{"handoff", "ready", "ha-coder-1", "--project", "ha"}, daemon: true, code: "bad_request"},
		"a value for --clear":    {args: []string{"handoff", "ready", "--clear=maybe"}, daemon: true, code: "bad_request"},
		"an unknown verb":        {args: []string{"handoff", "ack"}, daemon: true, code: "bad_request"},
		"the group alone":        {args: []string{"handoff"}, daemon: true, code: "bad_request"},
		"herdr failing":          {args: []string{"handoff", "ready"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, daemon: true, code: "herdr_failed"},
		"no daemon":              {args: []string{"handoff", "ready", "ha-coder-1"}, code: "daemon_unavailable"},
		"no plugin state dir":    {args: []string{"handoff", "ready", "ha-coder-1"}, env: map[string]string{"HERDR_PLUGIN_STATE_DIR": ""}, code: "daemon_unavailable"},
		"no herdr socket":        {args: []string{"handoff", "ready"}, env: map[string]string{"HERDR_SOCKET_PATH": ""}, code: "daemon_unavailable"},
		"a flag with no command": {args: []string{"handoff", "--clear"}, daemon: true, code: "bad_request"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnv(t)
			var asked <-chan string
			if tc.daemon {
				asked = fakeDaemon(t, e, `{"ok":true,"data":{}}`)
			}
			out, _, err := runOut(t, e.lookup(tc.env), tc.args...)
			require.Error(t, err, "a refusal exits 1")
			v := oneJSONObject(t, out)
			assert.Equal(t, false, v["ok"])
			errBody, _ := v["error"].(map[string]any)
			assert.Equal(t, tc.code, errBody["code"])
			assert.NotEmpty(t, errBody["message"])
			assert.Empty(t, asked, "the daemon is never asked")
		})
	}
}

package cli_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// failingHerdr fails every call, so a command that asks herdr is refused.
func failingHerdr(herdrtest.Request) herdrtest.Reply {
	return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "internal", Message: "boom"}}
}

// TestNoticesAskTheDaemon (A44): each notices command sends its op with
// the worker and, for mute, how long in nanoseconds, an hour by default,
// asking herdr nothing, and prints the daemon's answer as the envelope:
// data when ok, its code, exit 1, when refused.
func TestNoticesAskTheDaemon(t *testing.T) {
	t.Parallel()
	const muted = `{"ok":true,"data":{"worker":"ha-coder-1","until":"2026-10-22T22:00:00Z"}}`
	const unknown = `{"ok":false,"error":{"code":"unknown_worker","message":"no team's worker has that name: \"ha-coder-9\""}}`
	tests := map[string]struct {
		args  []string
		reply string
		sent  string
	}{
		"mute, an hour by default": {
			args: []string{"notices", "mute", "ha-coder-1"}, reply: muted,
			sent: `{"v":1,"op":"notices.mute","args":{"worker":"ha-coder-1","for":3600000000000}}`,
		},
		"mute for ten minutes": {
			args: []string{"notices", "mute", "ha-coder-1", "--for", "10m"}, reply: muted,
			sent: `{"v":1,"op":"notices.mute","args":{"worker":"ha-coder-1","for":600000000000}}`,
		},
		"mute for 0, the daemon's default": {
			args: []string{"notices", "mute", "--for", "0s", "ha-coder-1"}, reply: muted,
			sent: `{"v":1,"op":"notices.mute","args":{"worker":"ha-coder-1"}}`,
		},
		"a name like a flag, after --": {
			args: []string{"notices", "mute", "--", "-x"}, reply: muted,
			sent: `{"v":1,"op":"notices.mute","args":{"worker":"-x","for":3600000000000}}`,
		},
		"a refusal is printed with its code": {
			args: []string{"notices", "mute", "ha-coder-9"}, reply: unknown,
			sent: `{"v":1,"op":"notices.mute","args":{"worker":"ha-coder-9","for":3600000000000}}`,
		},
		"unmute": {
			args: []string{"notices", "unmute", "ha-coder-1"}, reply: `{"ok":true,"data":{"worker":"ha-coder-1","ended":true}}`,
			sent: `{"v":1,"op":"notices.unmute","args":{"worker":"ha-coder-1"}}`,
		},
		"list": {
			args: []string{"notices", "list"}, reply: `{"ok":true,"data":{"mutes":[]}}`,
			sent: `{"v":1,"op":"notices.list","args":{}}`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnvWith(t, failingHerdr)
			sent := fakeDaemon(t, e, tc.reply)
			out, _, err := runOut(t, e.lookup(map[string]string{"HERDR_PANE_ID": "w14:p1"}), tc.args...)
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

// TestNoticesRefuseBeforeAsking: a command line the notices commands cannot
// take, and no daemon, are refused in one JSON object, exit 1, and the
// daemon is never asked.
func TestNoticesRefuseBeforeAsking(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args   []string
		env    map[string]string
		daemon bool
		code   string
	}{
		"the group alone":          {args: []string{"notices"}, daemon: true, code: "bad_request"},
		"an unknown verb":          {args: []string{"notices", "snooze", "ha-coder-1"}, daemon: true, code: "bad_request"},
		"mute with no worker":      {args: []string{"notices", "mute"}, daemon: true, code: "bad_request"},
		"mute with two workers":    {args: []string{"notices", "mute", "a", "b"}, daemon: true, code: "bad_request"},
		"mute for no duration":     {args: []string{"notices", "mute", "a", "--for", "an hour"}, daemon: true, code: "bad_request"},
		"mute for with no value":   {args: []string{"notices", "mute", "a", "--for"}, daemon: true, code: "bad_request"},
		"mute past a Duration":     {args: []string{"notices", "mute", "a", "--for", "9999999999h"}, daemon: true, code: "bad_request"},
		"mute takes no project":    {args: []string{"notices", "mute", "a", "--project", "ha"}, daemon: true, code: "bad_request"},
		"unmute with no worker":    {args: []string{"notices", "unmute"}, daemon: true, code: "bad_request"},
		"unmute with two workers":  {args: []string{"notices", "unmute", "a", "b"}, daemon: true, code: "bad_request"},
		"unmute takes no duration": {args: []string{"notices", "unmute", "a", "--for", "1h"}, daemon: true, code: "bad_request"},
		"list with a worker":       {args: []string{"notices", "list", "a"}, daemon: true, code: "bad_request"},
		"mute with no daemon":      {args: []string{"notices", "mute", "a"}, code: "daemon_unavailable"},
		"unmute with no daemon":    {args: []string{"notices", "unmute", "a"}, code: "daemon_unavailable"},
		"list with no daemon":      {args: []string{"notices", "list"}, code: "daemon_unavailable"},
		"no herdr socket":          {args: []string{"notices", "list"}, env: map[string]string{"HERDR_SOCKET_PATH": ""}, code: "daemon_unavailable"},
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

// TestNoticesHelp: each command's usage and flags.
func TestNoticesHelp(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	for cmd, want := range map[string][]string{
		"notices mute":   {"herdr-agentisan notices mute <worker>", "--for duration", "(default 1h0m0s)", "at most 24h0m0s", "--state-dir", "ceiling always goes", "a restart ends them"},
		"notices unmute": {"herdr-agentisan notices unmute <worker>", "--state-dir"},
		"notices list":   {"herdr-agentisan notices list", "--state-dir"},
	} {
		out, _, err := runOut(t, e.lookup(nil), append(strings.Fields(cmd), "--help")...)
		require.NoError(t, err)
		for _, w := range want {
			assert.Contains(t, out, w, cmd)
		}
	}
}

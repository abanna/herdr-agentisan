package cli_test

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sandboxed is the environment Codex gives a tool command under its network
// sandbox, or an escalation it approved there, which carries the same.
var sandboxed = map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "1", "CODEX_THREAD_ID": "t1"}

// sandboxedPrefix opens the answer to a command Codex ran in its network
// sandbox whose dial reached nothing (workforce.CheckReach).
const sandboxedPrefix = "codex ran the command in its network sandbox: "

// sandboxCommands are every command that asks the daemon, and whether it
// first confirms the caller's pane through herdr when HERDR_PANE_ID is set.
var sandboxCommands = map[string]struct {
	args   []string
	caller bool
}{
	"team status":                  {args: []string{"team", "status"}, caller: true},
	"team up":                      {args: []string{"team", "up"}, caller: true},
	"worker spawn":                 {args: []string{"worker", "spawn", "--group", "coder"}, caller: true},
	"worker retire":                {args: []string{"worker", "retire", "ha-coder-1"}},
	"send":                         {args: []string{"send", "boss", "done: #125"}, caller: true},
	"handoff ready with no target": {args: []string{"handoff", "ready"}, caller: true},
	"handoff ready <id>":           {args: []string{"handoff", "ready", "ha-coder-1"}},
	"notices mute":                 {args: []string{"notices", "mute", "ha-coder-1"}},
	"notices unmute":               {args: []string{"notices", "unmute", "ha-coder-1"}},
	"notices list":                 {args: []string{"notices", "list"}},
}

// refusal runs args and returns the code and message it was refused with.
func refusal(t *testing.T, lookup func(string) (string, bool), args ...string) (code, message string) {
	t.Helper()
	out, err := runStdin(t, lookup, "", args...)
	require.Error(t, err, "a refusal exits 1")
	v := oneJSONObject(t, out)
	require.Equal(t, false, v["ok"])
	errBody, _ := v["error"].(map[string]any)
	code, _ = errBody["code"].(string)
	message, _ = errBody["message"].(string)
	return code, message
}

// TestCommandsFromACodexSandbox: a command Codex ran in its network sandbox,
// whose seccomp filter denies every connect, is told so once its dial
// reaches nothing, by every command that asks the daemon: daemon_unavailable
// with the cause and who to ask, never the daemon-restart advice (A25).
// Herdr refusing the caller's pane gets the same, and the daemon is never
// asked. One whose escalation Codex approved carries the same variable and
// reaches the daemon as any other; a daemon that took the request and gave
// no answer is still daemon_no_answer, since it may have run it (A52).
func TestCommandsFromACodexSandbox(t *testing.T) {
	t.Parallel()
	for name, tc := range sandboxCommands {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			code, msg := refusal(t, newDaemonEnv(t).lookup(sandboxed), tc.args...)
			assert.Equal(t, "daemon_unavailable", code, "no daemon")
			assert.True(t, strings.HasPrefix(msg, sandboxedPrefix), msg)
			assert.Contains(t, msg, "ask your boss")
			assert.NotContains(t, msg, "daemon-restart")

			e := newDaemonEnv(t)
			asked := fakeDaemon(t, e, `{"ok":true,"data":{}}`)
			out, err := runStdin(t, e.lookup(sandboxed), "", tc.args...)
			require.NoError(t, err, "an approved escalation reaches the daemon")
			assert.Equal(t, true, oneJSONObject(t, out)["ok"])
			require.Len(t, asked, 1)

			e = newDaemonEnv(t)
			asked = silentDaemon(t, e)
			code, msg = refusal(t, e.lookup(sandboxed), tc.args...)
			assert.Equal(t, "daemon_no_answer", code, "the daemon took it")
			assert.NotContains(t, msg, sandboxedPrefix)
			require.Len(t, asked, 1)

			if !tc.caller {
				return
			}
			// HERDR_PANE_ID has the command confirm the caller's pane
			// through herdr first, whose socket is gone.
			e = gone(t)
			asked = fakeDaemon(t, e, `{"ok":true,"data":{}}`)
			code, msg = refusal(t, e.lookup(withPane(sandboxed)), tc.args...)
			assert.Equal(t, "daemon_unavailable", code, "herdr unreached")
			assert.True(t, strings.HasPrefix(msg, sandboxedPrefix), msg)
			assert.Empty(t, asked, "the daemon is never asked")

			// A herdr reached that answers amiss, as after an approved
			// escalation, failed for its own reason.
			code, msg = refusal(t, newDaemonEnv(t).lookup(withPane(sandboxed)), tc.args...)
			assert.Equal(t, "herdr_failed", code, "herdr reached")
			assert.NotContains(t, msg, sandboxedPrefix)
		})
	}
}

// TestCommandsOutsideACodexSandbox: without the variable, or with it set to
// "", nothing changes: no daemon is the daemon-restart advice, and herdr
// unreached is herdr_failed.
func TestCommandsOutsideACodexSandbox(t *testing.T) {
	t.Parallel()
	for name, tc := range sandboxCommands {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, env := range []map[string]string{{}, {"CODEX_SANDBOX_NETWORK_DISABLED": "", "CODEX_THREAD_ID": "t1"}} {
				code, msg := refusal(t, newDaemonEnv(t).lookup(env), tc.args...)
				assert.Equal(t, "daemon_unavailable", code, "%v", env)
				assert.Contains(t, msg, "run the daemon-restart action", "%v", env)
				assert.NotContains(t, msg, sandboxedPrefix, "%v", env)

				if tc.caller {
					code, msg = refusal(t, gone(t).lookup(withPane(env)), tc.args...)
					assert.Equal(t, "herdr_failed", code, "%v", env)
					assert.NotContains(t, msg, sandboxedPrefix, "%v", env)
				}
			}
		})
	}
}

// gone is a daemonEnv whose herdr socket path names nothing, as a dial the
// sandbox denies reaches nothing; its plugin socket is its own to serve.
func gone(t *testing.T) daemonEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "dg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return daemonEnv{herdrSock: filepath.Join(dir, "h.sock"), stateDir: filepath.Join(dir, "state")}
}

// withPane is env with HERDR_PANE_ID set.
func withPane(env map[string]string) map[string]string {
	out := map[string]string{"HERDR_PANE_ID": "w14:p1"}
	maps.Copy(out, env)
	return out
}

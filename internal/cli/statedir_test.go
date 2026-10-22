package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// paneShell is a pane shell's environment: HERDR_SOCKET_PATH and
// HERDR_PANE_ID, but no HERDR_PLUGIN_STATE_DIR, which herdr gives only the
// plugin's own processes. HOME is a scratch directory, and with xdg
// XDG_STATE_HOME is one under it. The daemonEnv's state directory is the one
// herdr gives the plugin there (ADR-001 A51).
func paneShell(t *testing.T, h herdrtest.Handler, xdg bool) (daemonEnv, func(string) (string, bool)) {
	t.Helper()
	home := shortTemp(t)
	env := map[string]string{"HOME": home, "HERDR_PANE_ID": "w14:p1"}
	state := filepath.Join(home, ".local", "state")
	if xdg {
		state = filepath.Join(home, "xdg")
		env["XDG_STATE_HOME"] = state
	}
	e := daemonEnv{herdrSock: filepath.Join(home, "h.sock"), stateDir: filepath.Join(state, "herdr", "plugins", plugin.ID)}
	herdrtest.StartAt(t, e.herdrSock, h)
	env["HERDR_SOCKET_PATH"] = e.herdrSock
	return e, func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

// TestClientOpsFindHerdrsStateDir: run from a pane shell, with no
// --state-dir and no HERDR_PLUGIN_STATE_DIR, every command that asks a
// running daemon finds it under herdr's directory for the plugin, from
// HOME or XDG_STATE_HOME (ADR-001 A51).
func TestClientOpsFindHerdrsStateDir(t *testing.T) {
	t.Parallel()
	const queued = `{"ok":true,"data":{"id":"01J","state":"queued","to":{"kind":"boss","id":"ha-boss"},"from":{"kind":"worker","id":"ha-coder-1"}}}`
	const status = `{"ok":true,"data":{"groups":[]}}`
	tests := map[string]struct {
		args  []string
		xdg   bool
		reply string // the daemon's answer; "" for no daemon
		op    string // the op it must be asked
		out   string // stdout; the reply itself when ""
	}{
		"send":                         {args: []string{"send", "boss", "hi"}, reply: queued, op: daemon.OpSend},
		"team status":                  {args: []string{"team", "status"}, reply: status, op: daemon.OpTeamStatus},
		"team status, XDG_STATE_HOME":  {args: []string{"team", "status"}, xdg: true, reply: status, op: daemon.OpTeamStatus},
		"worker retire":                {args: []string{"worker", "retire", "ha-coder-2"}, reply: status, op: daemon.OpWorkerRetire},
		"back":                         {args: []string{"back"}, reply: `{"ok":true,"data":{"to":"w1:p1","from":"w1:p2"}}`, op: "back", out: "back to w1:p1 from w1:p2\n"},
		"status line":                  {args: []string{"status", "line"}, reply: statusSnapshot, op: "snapshot", out: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"status line, XDG_STATE_HOME":  {args: []string{"status", "line"}, xdg: true, reply: statusSnapshot, op: "snapshot", out: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"daemon health":                {args: []string{"daemon", "health"}, reply: `{"ok":true,"data":{"pid":42,"version":"v","commit":"c","uptime_s":5,"herdr_protocol":22,"herdr_socket":"/h","store_version":3,"focus_rows":7}}`, op: "health", out: "daemon pid 42, up 5s, v (c), herdr protocol 22 at /h, state schema 3, 7 focus rows\n"},
		"daemon stop, with none there": {args: []string{"daemon", "stop"}, out: "no daemon running\n"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, lookup := paneShell(t, paneHerdr("w14:p1"), tc.xdg)
			var asked <-chan string
			if tc.reply != "" {
				asked = fakeDaemon(t, e, tc.reply)
			}
			ctx := cli.WithDaemonHooks(t.Context(), refuseSpawn(t))
			stdout, stderr, err := runOutCtx(t, ctx, lookup, tc.args...)
			require.NoError(t, err, stderr)
			if tc.out == "" {
				assert.JSONEq(t, tc.reply, stdout)
			} else {
				assert.Equal(t, tc.out, stdout)
			}
			if tc.op != "" {
				var req struct {
					Op string `json:"op"`
				}
				require.NoError(t, json.Unmarshal([]byte(received(t, asked)), &req))
				assert.Equal(t, tc.op, req.Op)
			}
		})
	}
}

// TestDaemonStartNeverDerivesTheStateDir: start, run and restart refuse,
// as before, with no --state-dir and no HERDR_PLUGIN_STATE_DIR, and leave
// herdr's directory untouched. A daemon started under a wrong guess would be
// a second one for the same herdr server, which no lock would see (ADR-001
// A51).
func TestDaemonStartNeverDerivesTheStateDir(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"start", "run", "restart"} {
		t.Run(sub, func(t *testing.T) {
			t.Parallel()
			e, lookup := paneShell(t, paneHerdr("w14:p1"), false)
			ctx := cli.WithDaemonHooks(t.Context(), refuseSpawn(t))
			_, _, err := runOutCtx(t, ctx, lookup, "daemon", sub)
			require.ErrorIs(t, err, daemon.ErrNoStateDir)
			assert.NoDirExists(t, e.stateDir, "nothing made under herdr's directory for the plugin")
		})
	}
}

// TestNoDaemonUnderHerdrsStateDir: a workforce op that found no daemon in a
// directory it worked out says so, so a wrong guess (another
// XDG_STATE_HOME in herdr's environment, a debug herdr) is told from a
// daemon that is down. One given the directory does not.
func TestNoDaemonUnderHerdrsStateDir(t *testing.T) {
	t.Parallel()
	const note = "herdr's state directory for this plugin, from XDG_STATE_HOME or HOME; pass --state-dir if herdr runs with another"
	for name, given := range map[string]bool{"derived": false, "given": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, lookup := paneShell(t, paneHerdr("w14:p1"), false)
			args := []string{"team", "status"}
			if given {
				args = append(args, "--state-dir", e.stateDir)
			}
			stdout, _, err := runOut(t, lookup, args...)
			require.Error(t, err)
			var env struct {
				OK    bool `json:"ok"`
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &env))
			assert.Equal(t, "daemon_unavailable", env.Error.Code)
			assert.Contains(t, env.Error.Message, e.stateDir)
			if given {
				assert.NotContains(t, env.Error.Message, note)
			} else {
				assert.Contains(t, env.Error.Message, note)
			}
		})
	}
}

// TestARelativeHerdrStateDirIsRefused: an empty XDG_STATE_HOME or HOME
// gives a relative directory, as it does in herdr, and a command refuses it
// as it refuses a relative --state-dir, never resolving it against its own
// working directory.
func TestARelativeHerdrStateDirIsRefused(t *testing.T) {
	t.Parallel()
	for empty, dir := range map[string]string{
		"XDG_STATE_HOME": filepath.Join("herdr", "plugins", plugin.ID),
		"HOME":           filepath.Join(".local", "state", "herdr", "plugins", plugin.ID),
	} {
		t.Run(empty, func(t *testing.T) {
			t.Parallel()
			_, shell := paneShell(t, paneHerdr("w14:p1"), false)
			lookup := func(k string) (string, bool) {
				if k == empty {
					return "", true
				}
				return shell(k)
			}

			_, _, err := runOutCtx(t, t.Context(), lookup, "back")
			require.ErrorIs(t, err, daemon.ErrNoStateDir)

			stdout, _, err := runOut(t, lookup, "team", "status")
			require.Error(t, err)
			assert.Contains(t, stdout, `"code":"daemon_unavailable"`)
			assert.Contains(t, stdout, `\"`+dir+`\"`, "the refusal names the relative directory")
		})
	}
}

// refuseSpawn is a daemon start that fails the test: no command here may
// launch a daemon.
func refuseSpawn(t *testing.T) daemon.Hooks {
	t.Helper()
	return daemon.Hooks{Spawn: func(context.Context) (daemon.Spawned, error) {
		t.Error("a daemon was spawned")
		return daemon.Spawned{}, errors.New("no spawn in this test")
	}}
}

// runOutCtx is runOut on ctx, with the daemon's configuration fixed so the
// command never loads it from this process's environment.
func runOutCtx(t *testing.T, ctx context.Context, lookup func(string) (string, bool), args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := cli.Root()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	ctx = config.Into(ctx, config.Config{DaemonLockWait: time.Second})
	err = root.ExecuteContext(cli.WithLookupEnv(cli.WithLineage(ctx, testLineage), lookup))
	return out.String(), errOut.String(), err
}

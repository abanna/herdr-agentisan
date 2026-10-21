package cli_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// statusSnapshot is a team whose line is "PRs #886✓ · slots 1/2 · issues 4".
const statusSnapshot = `{"ok":true,"data":{"v":1,"at":"2026-10-21T06:00:00Z","team":"herdr-agentisan","project":{"prs":[{"number":886,"ci":"success","codex":"approved","merge":"clean"}],"issues":4,"slots":{"used":1,"total":2}},"groups":[]}}`

// TestStatusLineAsksForTheTeam: `status line` asks the daemon of the herdr
// server in HERDR_SOCKET_PATH for one team's snapshot and prints its line,
// one line, exit 0 (A41). The team is --project's or --workspace's, else
// the focused workspace's, which herdr gives a tab-bar command as
// HERDR_ACTIVE_WORKSPACE_ID, else the ◆ spaces'. It never calls herdr.
func TestStatusLineAsksForTheTeam(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args   []string
		env    map[string]string
		noFlag bool // the state dir from HERDR_PLUGIN_STATE_DIR, not --state-dir
		reply  string
		asked  string
		stdout string
	}{
		"the focused workspace's team": {env: map[string]string{"HERDR_ACTIVE_WORKSPACE_ID": "w14"}, asked: `{"workspace_id":"w14"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"a project's team":             {args: []string{"--project", "agentisan"}, env: map[string]string{"HERDR_ACTIVE_WORKSPACE_ID": "w14"}, asked: `{"project":"agentisan"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"a workspace's team":           {args: []string{"--workspace", "wN"}, env: map[string]string{"HERDR_ACTIVE_WORKSPACE_ID": "w14"}, asked: `{"workspace_id":"wN"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"no focused workspace":         {asked: `{}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"a workspace in no team": {
			env:   map[string]string{"HERDR_ACTIVE_WORKSPACE_ID": "w7"},
			reply: `{"ok":false,"error":{"code":"no_team","message":"workspace w7 is no team's"}}`, asked: `{"workspace_id":"w7"}`, stdout: "\n",
		},
		"a daemon starting": {
			reply: `{"ok":false,"error":{"code":"not_ready","message":"the team poll has not built one"}}`, asked: `{}`, stdout: dashboard.TabBarStarting + "\n",
		},
		"an empty focused workspace": {env: map[string]string{"HERDR_ACTIVE_WORKSPACE_ID": ""}, asked: `{}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"the state dir from HERDR_PLUGIN_STATE_DIR": {
			noFlag: true, env: map[string]string{"HERDR_ACTIVE_WORKSPACE_ID": "w14"}, asked: `{"workspace_id":"w14"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n",
		},
		// Names go to the daemon as given, in one JSON line; the daemon
		// matches them exactly (A24).
		"a non-ASCII project":         {args: []string{"--project", "ågentisan"}, asked: `{"project":"ågentisan"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"a project not UTF-8":         {args: []string{"--project", "a\xffb"}, asked: `{"project":"a\ufffdb"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"a project that looks a flag": {args: []string{"--project=--help"}, asked: `{"project":"--help"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
		"a project with a newline":    {args: []string{"--project", "a\nb c"}, asked: `{"project":"a\nb c"}`, stdout: "PRs #886✓ · slots 1/2 · issues 4\n"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var herdrCalls atomic.Int32
			e := newDaemonEnvWith(t, func(herdrtest.Request) herdrtest.Reply {
				herdrCalls.Add(1)
				return herdrtest.Reply{Result: map[string]any{"type": "pong", "version": "0.9.3", "protocol": 22}}
			})
			reply := tc.reply
			if reply == "" {
				reply = statusSnapshot
			}
			asked := fakeDaemon(t, e, reply)
			// --state-dir wins over HERDR_PLUGIN_STATE_DIR, which herdr does
			// not set for a tab-bar command anyway.
			env := map[string]string{"HERDR_PLUGIN_STATE_DIR": filepath.Join(e.stateDir, "elsewhere")}
			args := append([]string{"status", "line", "--state-dir", e.stateDir}, tc.args...)
			if tc.noFlag {
				env["HERDR_PLUGIN_STATE_DIR"] = e.stateDir
				args = append([]string{"status", "line"}, tc.args...)
			}
			maps.Copy(env, tc.env)
			stdout, stderr, err := runOut(t, e.lookup(env), args...)
			require.NoError(t, err, stderr)
			assert.Equal(t, tc.stdout, stdout)
			var req struct {
				Op   string          `json:"op"`
				Args json.RawMessage `json:"args"`
			}
			require.NoError(t, json.Unmarshal([]byte(received(t, asked)), &req))
			assert.Equal(t, "snapshot", req.Op)
			assert.JSONEq(t, tc.asked, string(req.Args))
			assert.Zero(t, herdrCalls.Load(), "the line comes from the daemon's cache, never from herdr")
		})
	}
}

// received is the request line the fake daemon read, failing the test at
// once if none comes, rather than at the package's timeout.
func received(t *testing.T, asked <-chan string) string {
	t.Helper()
	select {
	case line := <-asked:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the daemon")
		return ""
	}
}

// TestStatusLineWithoutADaemon: a daemon that is down, or slower than
// --timeout, is one short line and exit 0, within the budget: herdr blanks
// an entry whose command fails or outlives timeout_seconds.
func TestStatusLineWithoutADaemon(t *testing.T) {
	t.Parallel()

	t.Run("down", func(t *testing.T) {
		t.Parallel()
		e := newDaemonEnv(t)
		stdout, stderr, err := runOut(t, e.lookup(nil), "status", "line")
		require.NoError(t, err, stderr)
		assert.Equal(t, dashboard.TabBarDown+"\n", stdout)
	})

	t.Run("not answering", func(t *testing.T) {
		t.Parallel()
		e := newDaemonEnv(t)
		p, err := daemon.PathsFor(e.stateDir, e.herdrSock)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(p.Dir, 0o700))
		ln, err := net.Listen("unix", p.Socket)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		held := make(chan net.Conn, 1)
		go func() {
			if conn, err := ln.Accept(); err == nil {
				held <- conn
			}
		}()
		t.Cleanup(func() {
			select {
			case conn := <-held:
				_ = conn.Close()
			default:
			}
		})
		start := time.Now()
		stdout, stderr, err := runOut(t, e.lookup(nil), "status", "line", "--timeout", "200ms")
		require.NoError(t, err, stderr)
		assert.Equal(t, dashboard.TabBarNotAnswering+"\n", stdout)
		assert.Less(t, time.Since(start), 2*time.Second, "within herdr's default timeout_seconds")
	})
}

// TestStatusLineRejectsBadInvocations: a command line herdr's config got
// wrong fails, so herdr blanks the entry and logs why: no state dir, no
// herdr socket, both selectors, a budget that is not positive, an argument.
func TestStatusLineRejectsBadInvocations(t *testing.T) {
	t.Parallel()
	dir := shortTemp(t)
	sock := filepath.Join(dir, "h.sock")
	tests := map[string]struct {
		env  map[string]string
		args []string
		want error
	}{
		"no state dir":      {env: map[string]string{"HERDR_SOCKET_PATH": sock}, want: daemon.ErrNoStateDir},
		"a relative one":    {env: map[string]string{"HERDR_SOCKET_PATH": sock}, args: []string{"--state-dir", "state"}, want: daemon.ErrNoStateDir},
		"no herdr socket":   {env: map[string]string{"HERDR_PLUGIN_STATE_DIR": dir}, want: daemon.ErrHerdrGone},
		"a relative socket": {env: map[string]string{"HERDR_SOCKET_PATH": "h.sock", "HERDR_PLUGIN_STATE_DIR": dir}, want: daemon.ErrHerdrGone},
		"a budget too long": {env: map[string]string{"HERDR_SOCKET_PATH": sock, "HERDR_PLUGIN_STATE_DIR": dir}, args: []string{"--timeout", "9999999999h"}},
		"both selectors":    {env: map[string]string{"HERDR_SOCKET_PATH": sock, "HERDR_PLUGIN_STATE_DIR": dir}, args: []string{"--project", "p", "--workspace", "w1"}},
		"a zero budget":     {env: map[string]string{"HERDR_SOCKET_PATH": sock, "HERDR_PLUGIN_STATE_DIR": dir}, args: []string{"--timeout", "0s"}},
		"a negative budget": {env: map[string]string{"HERDR_SOCKET_PATH": sock, "HERDR_PLUGIN_STATE_DIR": dir}, args: []string{"--timeout", "-1s"}},
		"an argument":       {env: map[string]string{"HERDR_SOCKET_PATH": sock, "HERDR_PLUGIN_STATE_DIR": dir}, args: []string{"agentisan"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lookup := func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }
			stdout, _, err := runOut(t, lookup, append([]string{"status", "line"}, tc.args...)...)
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			assert.Empty(t, stdout, "no line, so herdr shows none")
		})
	}
}

// TestStatusNeedsASubcommand: a bare `status` fails rather than printing
// help and exiting 0, which a tab-bar entry would show as a line.
func TestStatusNeedsASubcommand(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"status"}, {"status", "lines"}} {
		_, _, err := runOut(t, func(string) (string, bool) { return "", false }, args...)
		require.Error(t, err, "%v", args)
	}
}

// shortTemp is a fresh directory with a path short enough for a unix
// socket beneath it.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "st")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// tabBarEntry is one tab_bar_right entry, with herdr 0.9.3's
// TabBarRightEntryConfig fields (src/config/tab_bar.rs).
type tabBarEntry struct {
	Type            string `toml:"type"`
	Command         string `toml:"command"`
	IntervalSeconds int64  `toml:"interval_seconds"`
	TimeoutSeconds  int64  `toml:"timeout_seconds"`
	Format          string `toml:"format"`
	Text            string `toml:"text"`
}

// tabBarKeys are the keys each entry type takes. herdr denies the others
// (TabBarRightEntryConfig is tagged by type, with deny_unknown_fields), so a
// stray key fails herdr's config check.
var tabBarKeys = map[string][]string{
	"zoom":     {"type"},
	"hostname": {"type"},
	"datetime": {"type", "format"},
	"text":     {"type", "text"},
	"command":  {"type", "command", "interval_seconds", "timeout_seconds"},
}

// shellWords is command as /bin/sh splits and expands it, with env its
// whole environment: herdr runs a tab-bar command through /bin/sh -lc.
func shellWords(t *testing.T, command string, env ...string) []string {
	t.Helper()
	sh := exec.CommandContext(t.Context(), "/bin/sh", "-c", "set -- "+command+`; printf '%s\0' "$@"`) //nolint:gosec // the snippet's own command line is what this test checks
	sh.Env = env
	out, err := sh.Output()
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// TestTabBarSnippet keeps docs/config/herdr-tab-bar.example.toml working
// (A41): entries herdr accepts, one command entry within herdr's bounds,
// whose timeout leaves the command's budget room, and whose command line,
// as /bin/sh expands it, prints the focused team's line from the daemon
// under the plugin's state directory, with no HERDR_PLUGIN_STATE_DIR set.
// herdr's state directory is $XDG_STATE_HOME/herdr when XDG_STATE_HOME is
// set, else $HOME/.local/state/herdr (src/config/io.rs:37-42,88-94), and
// the plugin's is plugins/<id> under it (src/plugin_paths.rs:21-25).
func TestTabBarSnippet(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "herdr-tab-bar.example.toml"))
	require.NoError(t, err)
	var cfg struct {
		UI struct {
			TabBarRight []tabBarEntry `toml:"tab_bar_right"`
			Separator   string        `toml:"tab_bar_right_separator"`
		} `toml:"ui"`
	}
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&cfg))
	var keys struct {
		UI struct {
			TabBarRight []map[string]any `toml:"tab_bar_right"`
		} `toml:"ui"`
	}
	require.NoError(t, toml.Unmarshal(raw, &keys))
	for i, entry := range keys.UI.TabBarRight {
		allowed, ok := tabBarKeys[cfg.UI.TabBarRight[i].Type]
		require.True(t, ok, "entry %d: herdr has no type %q", i, cfg.UI.TabBarRight[i].Type)
		for k := range entry {
			assert.Contains(t, allowed, k, "entry %d", i)
		}
	}
	var commands []tabBarEntry
	for _, e := range cfg.UI.TabBarRight {
		if e.Type == "command" {
			commands = append(commands, e)
		}
	}
	require.Len(t, commands, 1)
	c := commands[0]
	// herdr hides an entry outside these (src/config/tab_bar.rs:3-7).
	assert.Equal(t, int64(5), c.IntervalSeconds, "A41: the line follows a workspace switch within 5 s")
	assert.GreaterOrEqual(t, c.IntervalSeconds, int64(1))
	assert.LessOrEqual(t, c.IntervalSeconds, int64(31_536_000))
	assert.GreaterOrEqual(t, c.TimeoutSeconds, int64(1))
	assert.LessOrEqual(t, c.TimeoutSeconds, int64(3_600))
	assert.Greater(t, time.Duration(c.TimeoutSeconds)*time.Second, dashboard.TabBarBudget, "the line is printed before herdr kills the command")

	tests := map[string]struct {
		xdg       bool // XDG_STATE_HOME is set
		stateHome func(home string) string
	}{
		"XDG_STATE_HOME unset": {stateHome: func(home string) string { return filepath.Join(home, ".local", "state") }},
		"XDG_STATE_HOME set":   {xdg: true, stateHome: func(home string) string { return filepath.Join(home, "xdg") }},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := shortTemp(t)
			env := []string{"HOME=" + home}
			if tc.xdg {
				env = append(env, "XDG_STATE_HOME="+filepath.Join(home, "xdg"))
			}
			words := shellWords(t, c.Command, env...)
			require.NotEmpty(t, words)
			require.Equal(t, "herdr-agentisan", words[0])
			stateDir := filepath.Join(tc.stateHome(home), "herdr", "plugins", plugin.ID)
			require.Contains(t, words, stateDir, "herdr's state directory for this plugin")

			e := daemonEnv{herdrSock: filepath.Join(home, "h.sock"), stateDir: stateDir}
			asked := fakeDaemon(t, e, statusSnapshot)
			lookup := e.lookup(map[string]string{"HERDR_PLUGIN_STATE_DIR": "", "HERDR_ACTIVE_WORKSPACE_ID": "w14"})
			stdout, stderr, err := runOut(t, lookup, words[1:]...)
			require.NoError(t, err, stderr)
			assert.Equal(t, "PRs #886✓ · slots 1/2 · issues 4\n", stdout)
			assert.Contains(t, received(t, asked), `"args":{"workspace_id":"w14"}`, "the focused workspace's team")
		})
	}
}

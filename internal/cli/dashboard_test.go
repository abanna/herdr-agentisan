package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// sample is the snapshot docs/dashboard tells a reader to try.
var sample = filepath.Join("..", "..", "docs", "dashboard", "sample-snapshot.json")

// TestDashboardRejectsBadInvocations: each fails before taking over the
// terminal and before reaching herdr. With no fixture the dashboard reads
// the daemon, so it needs what names the daemon's socket: the plugin state
// directory (HERDR_PLUGIN_STATE_DIR or --state-dir) and HERDR_SOCKET_PATH.
func TestDashboardRejectsBadInvocations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"v":7}`), 0o600))

	tests := map[string]struct {
		args     []string
		env      map[string]string
		want     error
		contains string
	}{
		"no fixture, no state dir": {args: []string{"dashboard"}, want: daemon.ErrNoStateDir, contains: "--state-dir"},
		"an empty state dir":       {args: []string{"dashboard"}, env: map[string]string{"HERDR_PLUGIN_STATE_DIR": ""}, want: daemon.ErrNoStateDir},
		"a relative --state-dir":   {args: []string{"dashboard", "--state-dir", "state"}, want: daemon.ErrNoStateDir},
		"no fixture, no herdr":     {args: []string{"dashboard", "--state-dir", dir}, env: map[string]string{"HERDR_SOCKET_PATH": ""}, want: daemon.ErrHerdrGone},
		"a state dir too deep":     {args: []string{"dashboard", "--state-dir", "/" + strings.Repeat("d", 100)}, want: daemon.ErrSocketPathTooLong},
		"an empty fixture flag":    {args: []string{"dashboard", "--fixture", ""}, want: daemon.ErrNoStateDir},
		// pflag takes the next argument as the flag's value even when it
		// starts with '-': the name is a path, never a flag.
		"a dash-leading fixture is a path": {args: []string{"dashboard", "--fixture", "-nope.json"}, want: snapshot.ErrUnavailable, contains: "-nope.json"},
		"a directory as the fixture":       {args: []string{"dashboard", "--fixture", dir}, want: snapshot.ErrUnavailable, contains: "not a regular file"},
		"a malformed plugin context": {
			args: []string{"dashboard", "--fixture", sample}, env: map[string]string{"HERDR_PLUGIN_CONTEXT_JSON": "{"},
			want: plugin.ErrInvalidEnv,
		},
		"a project with a fixture": {args: []string{"dashboard", "--fixture", sample, "--project", "alpha"}, contains: "[fixture project]"},
		"positional":               {args: []string{"dashboard", "extra"}},
		"unknown flag":             {args: []string{"dashboard", "--bogus"}},
		"missing fixture":          {args: []string{"dashboard", "--fixture", filepath.Join(dir, "nope.json")}, want: snapshot.ErrUnavailable},
		"newer fixture":            {args: []string{"dashboard", "--fixture", bad}, want: snapshot.ErrUnsupportedVersion},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, focusOK)
			env := map[string]string{"HERDR_SOCKET_PATH": srv.Path}
			maps.Copy(env, tc.env)
			_, err := runCtx(envCtx(t, env), tc.args...)
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			assert.ErrorContains(t, err, tc.contains)
			assert.Empty(t, srv.Requests(), "a rejected invocation must not reach herdr")
		})
	}
}

func focusOK(r herdrtest.Request) herdrtest.Reply {
	if r.Method == "pane.zoom" {
		return herdrtest.Reply{Result: map[string]any{"type": "pane_zoom", "zoom": map[string]any{"pane_id": "w2:p1", "zoomed": true}}}
	}
	return herdrtest.Reply{Result: map[string]any{"type": "agent_info", "agent": map[string]any{"pane_id": "w2:p1", "name": "pee01"}}}
}

// lockedBuffer is a bytes.Buffer safe to read while the program writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// session runs `dashboard --fixture sample` with keys typed on a pipe that
// stays open, as a terminal's stdin would, and returns what it drew.
type session struct {
	t    *testing.T
	keys *io.PipeWriter
	out  *lockedBuffer
	done chan error
}

func startDashboard(t *testing.T, ctx context.Context) *session {
	t.Helper()
	return startDashboardWith(t, ctx, "dashboard", "--fixture", sample)
}

// startDashboardWith runs the dashboard command line args, as startDashboard
// runs the fixture's.
func startDashboardWith(t *testing.T, ctx context.Context, args ...string) *session {
	t.Helper()
	in, keys := io.Pipe()
	t.Cleanup(func() { _ = in.Close() })
	s := &session{t: t, keys: keys, out: &lockedBuffer{}, done: make(chan error, 1)}

	root := cli.Root()
	root.SetIn(in)
	root.SetOut(s.out)
	root.SetErr(s.out)
	root.SetArgs(args)
	go func() { s.done <- root.ExecuteContext(ctx) }()
	return s
}

func (s *session) waitFor(text string) {
	s.t.Helper()
	require.Eventually(s.t, func() bool { return strings.Contains(s.out.String(), text) }, 5*time.Second, 10*time.Millisecond,
		"the dashboard never drew %q", text)
}

func (s *session) press(keys string) {
	s.t.Helper()
	_, err := s.keys.Write([]byte(keys))
	require.NoError(s.t, err)
}

func (s *session) quit() {
	s.t.Helper()
	s.press("q")
	select {
	case err := <-s.done:
		require.NoError(s.t, err)
	case <-time.After(5 * time.Second):
		s.t.Fatal("q did not quit the dashboard")
	}
}

// envCtx injects exactly env, so the command never sees the developer's
// real HERDR_* variables. A key mapped to "" is set but empty.
func envCtx(t *testing.T, env map[string]string) context.Context {
	t.Helper()
	return cli.WithLookupEnv(t.Context(), func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
}

// TestDashboardDrawsTheFixture: with no usable herdr socket the dashboard
// still draws, and says focus is off.
func TestDashboardDrawsTheFixture(t *testing.T) {
	t.Parallel()

	tests := map[string]map[string]string{
		"HERDR_SOCKET_PATH unset":         {},
		"HERDR_SOCKET_PATH set but empty": {"HERDR_SOCKET_PATH": ""},
		"inside herdr with no socket":     {"HERDR_ENV": "1"},
	}
	for name, env := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := startDashboard(t, envCtx(t, env))
			s.waitFor("pee01")
			s.waitFor("focus off: HERDR_SOCKET_PATH is not set")
			s.quit()
		})
	}
}

// TestDashboardFocusFailureStaysInTheFooter: a HERDR_SOCKET_PATH that names
// no server makes Enter fail into the footer; the dashboard keeps running
// and still quits on q.
func TestDashboardFocusFailureStaysInTheFooter(t *testing.T) {
	t.Parallel()

	s := startDashboard(t, envCtx(t, map[string]string{"HERDR_SOCKET_PATH": filepath.Join(t.TempDir(), "gone.sock")}))
	s.waitFor("pee01")
	s.press("\r")
	s.waitFor("pee01: focus failed")
	s.waitFor("herdr is unavailable")
	s.quit()
}

// TestDashboardEnterFocusesThroughHerdr: Enter on the first agent sends
// agent.focus then pane.zoom to the socket in HERDR_SOCKET_PATH — the fake
// one, never the developer's.
func TestDashboardEnterFocusesThroughHerdr(t *testing.T) {
	t.Parallel()

	srv := herdrtest.Start(t, focusOK)
	s := startDashboard(t, pingCtx(t, srv.Path))
	s.waitFor("pee01")
	s.press("\r")
	require.Eventually(t, func() bool { return len(srv.Requests()) == 2 }, 5*time.Second, 10*time.Millisecond)
	s.quit()

	reqs := srv.Requests()
	assert.Equal(t, "agent.focus", reqs[0].Method)
	assert.JSONEq(t, `{"target":"pee01"}`, string(reqs[0].Params))
	assert.Equal(t, "pane.zoom", reqs[1].Method)
	assert.JSONEq(t, `{"pane_id":"w2:p1","mode":"on"}`, string(reqs[1].Params))
}

// TestDashboardReadsTheDaemon: with no fixture the dashboard reads the
// daemon of the herdr server in HERDR_SOCKET_PATH, under the plugin state
// directory from HERDR_PLUGIN_STATE_DIR or --state-dir, or else herdr's own
// for the plugin, from HOME, as in a pane shell (ADR-001 A51), exactly where
// `daemon health` looks. With no daemon there it says so, keeps polling and
// still quits on q. It asks herdr only where its own pane is (A24), and
// with --project not even that.
func TestDashboardReadsTheDaemon(t *testing.T) {
	t.Parallel()

	sampleLine := func(t *testing.T) string {
		t.Helper()
		raw, err := os.ReadFile(sample)
		require.NoError(t, err)
		var line bytes.Buffer
		require.NoError(t, json.Compact(&line, raw))
		return line.String()
	}
	tests := map[string]struct {
		flag    bool   // the state dir comes from --state-dir, not the environment
		home    bool   // no HERDR_PLUGIN_STATE_DIR: the state dir is herdr's under HOME
		daemon  bool   // a daemon answers at the socket
		reply   string // what it answers; "" for the sample
		project string // --project
		asked   string // the selector the daemon must have been sent
		waitFor []string
	}{
		"the state dir from HERDR_PLUGIN_STATE_DIR": {daemon: true, asked: `{"workspace_id":"w2"}`, waitFor: []string{"pee01"}},
		"the state dir from --state-dir":            {flag: true, daemon: true, asked: `{"workspace_id":"w2"}`, waitFor: []string{"pee01"}},
		"herdr's state dir, from HOME":              {home: true, daemon: true, asked: `{"workspace_id":"w2"}`, waitFor: []string{"pee01"}},
		"no daemon running":                         {waitFor: []string{"waiting for the daemon", "daemon not running"}},
		"a project's team":                          {daemon: true, project: "alpha", asked: `{"project":"alpha"}`, waitFor: []string{"pee01"}},
		"outside every team": {
			daemon: true, reply: `{"ok":false,"error":{"code":"no_team","message":"workspace w2 is no team's"}}` + "\n",
			waitFor: []string{"no team in this workspace", "this workspace (w2) is not a team's; pass --project"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, ownPane)
			stateDir, err := os.MkdirTemp("", "cs")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
			home := stateDir
			if tc.home {
				stateDir = filepath.Join(home, ".local", "state", "herdr", "plugins", plugin.ID)
			}
			paths, err := daemon.PathsFor(stateDir, srv.Path)
			require.NoError(t, err)
			var asked *lockedBuffer
			if tc.daemon {
				require.NoError(t, os.MkdirAll(paths.Dir, 0o700))
				reply := tc.reply
				if reply == "" {
					reply = `{"ok":true,"data":` + sampleLine(t) + "}\n"
				}
				asked = fakeDaemonAt(t, paths.Socket, reply)
			}
			env := map[string]string{
				"HERDR_SOCKET_PATH": srv.Path, "HERDR_PLUGIN_STATE_DIR": stateDir,
				"HERDR_PANE_ID": "w2:p1", "HERDR_WORKSPACE_ID": "w7",
			}
			args := []string{"dashboard"}
			if tc.project != "" {
				args = append(args, "--project", tc.project)
			}
			if tc.flag {
				env["HERDR_PLUGIN_STATE_DIR"] = filepath.Join(stateDir, "elsewhere")
				args = append(args, "--state-dir", stateDir)
			}
			if tc.home {
				delete(env, "HERDR_PLUGIN_STATE_DIR")
				env["HOME"] = home
			}

			s := startDashboardWith(t, envCtx(t, env), args...)
			for _, text := range tc.waitFor {
				s.waitFor(text)
			}
			s.quit()
			for _, r := range srv.Requests() {
				assert.Contains(t, []string{"pane.list", "pane.process_info"}, r.Method, "drawing never calls herdr but to find its own pane; the daemon is the collector")
			}
			if tc.project != "" {
				assert.Empty(t, srv.Requests(), "a project's team is not located")
			}
			if tc.asked != "" {
				assert.Contains(t, asked.String(), `"args":`+tc.asked)
			}
		})
	}
}

// ownPane is a herdr with one pane, w2:p1, which runs this process: the
// dashboard runs in the test's process.
func ownPane(r herdrtest.Request) herdrtest.Reply {
	switch r.Method {
	case "pane.list":
		return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []map[string]any{{"pane_id": "w2:p1", "workspace_id": "w2", "focused": true}}}}
	case "pane.process_info":
		return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": "w2:p1", "shell_pid": os.Getpid(), "foreground_process_group_id": os.Getpid(), "foreground_processes": []any{},
		}}}
	}
	return focusOK(r)
}

// fakeDaemonAt answers every connection at sock with reply, and returns
// every request line it read.
func fakeDaemonAt(t *testing.T, sock, reply string) *lockedBuffer {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	asked := &lockedBuffer{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			_, _ = asked.Write(line)
			_, _ = conn.Write([]byte(reply))
			_ = conn.Close()
		}
	}()
	return asked
}

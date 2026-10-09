package cli_test

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// sample is the snapshot docs/dashboard tells a reader to try.
var sample = filepath.Join("..", "..", "docs", "dashboard", "sample-snapshot.json")

// TestDashboardRejectsBadInvocations: each fails before taking over the
// terminal and before reaching herdr. The daemon's snapshot op is not served
// yet, so a fixture is the only source, and the error says so.
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
		"no fixture": {args: []string{"dashboard"}, want: dashboard.ErrNoSource, contains: "--fixture"},
		// pflag takes the next argument as the flag's value even when it
		// starts with '-': the name is a path, never a flag.
		"a dash-leading fixture is a path": {args: []string{"dashboard", "--fixture", "-nope.json"}, want: snapshot.ErrUnavailable, contains: "-nope.json"},
		"a directory as the fixture":       {args: []string{"dashboard", "--fixture", dir}, want: snapshot.ErrUnavailable, contains: "not a regular file"},
		"a malformed plugin context": {
			args: []string{"dashboard", "--fixture", sample}, env: map[string]string{"HERDR_PLUGIN_CONTEXT_JSON": "{"},
			want: plugin.ErrInvalidEnv,
		},
		"positional":      {args: []string{"dashboard", "extra"}},
		"unknown flag":    {args: []string{"dashboard", "--bogus"}},
		"missing fixture": {args: []string{"dashboard", "--fixture", filepath.Join(dir, "nope.json")}, want: snapshot.ErrUnavailable},
		"newer fixture":   {args: []string{"dashboard", "--fixture", bad}, want: snapshot.ErrUnsupportedVersion},
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
	in, keys := io.Pipe()
	t.Cleanup(func() { _ = in.Close() })
	s := &session{t: t, keys: keys, out: &lockedBuffer{}, done: make(chan error, 1)}

	root := cli.Root()
	root.SetIn(in)
	root.SetOut(s.out)
	root.SetErr(s.out)
	root.SetArgs([]string{"dashboard", "--fixture", sample})
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

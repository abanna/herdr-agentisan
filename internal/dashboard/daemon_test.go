package dashboard_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// shortDir is a fresh directory with a path short enough for a unix socket
// beneath it.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestDaemonConfig: the dashboard herdr runs reads the daemon of its herdr
// server, at the socket `daemon health` asks: the plugin state directory
// plus HERDR_SOCKET_PATH (daemon.PathsFor). Either missing is the caller's
// mistake, reported before the terminal is taken over. The team is the one
// where its pane is, found through that herdr, or the project it is given.
func TestDaemonConfig(t *testing.T) {
	t.Parallel()

	dir := shortDir(t)
	const herdrSock = "/run/user/1000/herdr.sock"
	where := dashboard.Where{PID: 77, PaneID: "w2:p1", WorkspaceID: "w2"}
	tests := map[string]struct {
		stateDir, herdrSocket string
		project               string
		want                  error
	}{
		"the daemon of this server": {stateDir: dir, herdrSocket: herdrSock},
		"a project's team":          {stateDir: dir, herdrSocket: herdrSock, project: "alpha"},
		// Nothing is read at config time: a state dir herdr has not made
		// yet is a daemon that is not running, which the footer reports.
		"a state dir that does not exist yet": {stateDir: filepath.Join(dir, "later"), herdrSocket: herdrSock},
		"no state dir":                        {herdrSocket: herdrSock, want: daemon.ErrNoStateDir},
		"a relative state dir":                {stateDir: "state", herdrSocket: herdrSock, want: daemon.ErrNoStateDir},
		"no herdr socket":                     {stateDir: dir, want: daemon.ErrHerdrGone},
		"a relative herdr socket":             {stateDir: dir, herdrSocket: "herdr.sock", want: daemon.ErrHerdrGone},
		"a state dir too deep to serve":       {stateDir: "/" + strings.Repeat("d", 100), herdrSocket: herdrSock, want: daemon.ErrSocketPathTooLong},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := where
			w.Project = tc.project
			cfg, err := dashboard.DaemonConfig(tc.stateDir, tc.herdrSocket, w)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			paths, err := daemon.PathsFor(tc.stateDir, tc.herdrSocket)
			require.NoError(t, err)
			want := &dashboard.TeamSource{Socket: paths.Socket, Project: tc.project}
			if tc.project == "" {
				want.Locator = &dashboard.Locator{Herdr: herdr.Client{SocketPath: tc.herdrSocket}, PID: 77, PaneID: "w2:p1", WorkspaceID: "w2"}
			}
			assert.Equal(t, want, cfg.Source)
			assert.Equal(t, dashboard.HerdrFocuser{Client: herdr.Client{SocketPath: tc.herdrSocket}}, cfg.Focuser)
			assert.Equal(t, dashboard.HerdrBtop{Client: herdr.Client{SocketPath: tc.herdrSocket}}, cfg.Btop)
		})
	}
}

// TestNoDaemonIsOneLineAndRetried: with no daemon the dashboard draws, says
// so on one footer line, and keeps polling; once the daemon answers, the
// team is drawn and the line goes.
func TestNoDaemonIsOneLineAndRetried(t *testing.T) {
	t.Parallel()

	stateDir := shortDir(t)
	const herdrSock = "/run/user/1000/herdr.sock"
	cfg, err := dashboard.DaemonConfig(stateDir, herdrSock, dashboard.Where{Project: "agentisan"})
	require.NoError(t, err)
	cfg.Interval, cfg.Now, cfg.Options = time.Millisecond, func() time.Time { return at }, plain
	m, tick := startedPolling(t, cfg, 120, 40)

	out := m.View().Content
	assert.Contains(t, out, "waiting for the daemon…")
	assert.Contains(t, out, "✖ daemon not running: ")
	m, tick = pollOnce(t, m, tick)
	assert.Contains(t, m.View().Content, "✖ daemon not running: ", "still polling, still saying so")

	paths, err := daemon.PathsFor(stateDir, herdrSock)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(paths.Socket), 0o700))
	sample, err := os.ReadFile(filepath.Join("testdata", "full.json"))
	require.NoError(t, err)
	var line bytes.Buffer
	require.NoError(t, json.Compact(&line, sample))
	fakeDaemon(t, paths.Socket, `{"ok":true,"data":`+line.String()+"}\n")

	m, _ = pollOnce(t, m, tick)
	out = m.View().Content
	assert.Contains(t, out, "pee01")
	assert.NotContains(t, out, "daemon not running")
}

// fakeDaemon answers every connection at sock with reply.
func fakeDaemon(t *testing.T, sock, reply string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			_, _ = conn.Write([]byte(reply))
			_ = conn.Close()
		}
	}()
}

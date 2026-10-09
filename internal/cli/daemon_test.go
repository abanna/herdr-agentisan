package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/logging"
)

// daemonEnv is a herdr server plus a plugin state directory, both scratch:
// a daemon test never touches the developer's herdr or state.
type daemonEnv struct {
	herdrSock string
	stateDir  string
}

func newDaemonEnv(t *testing.T) daemonEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "dc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := daemonEnv{herdrSock: filepath.Join(dir, "h.sock"), stateDir: filepath.Join(dir, "state")}
	herdrtest.StartAt(t, e.herdrSock, func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: map[string]any{"type": "pong", "version": "0.9.3", "protocol": 22}}
	})
	return e
}

func (e daemonEnv) lookup(extra map[string]string) func(string) (string, bool) {
	env := map[string]string{"HERDR_SOCKET_PATH": e.herdrSock, "HERDR_PLUGIN_STATE_DIR": e.stateDir}
	maps.Copy(env, extra)
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

// inProcess runs every daemon the commands spawn in this process, and stops
// it when the commands signal it.
type inProcess struct {
	t      *testing.T
	e      daemonEnv
	mu     sync.Mutex
	cancel map[int]context.CancelFunc
	runs   int
}

func (p *inProcess) hooks() daemon.Hooks {
	return daemon.Hooks{
		Spawn: func(context.Context) (int, error) {
			ctx, cancel := context.WithCancel(p.t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				err := daemon.Run(ctx, daemon.Options{
					StateDir: p.e.stateDir, HerdrSocket: p.e.herdrSock, Herdr: herdr.Client{SocketPath: p.e.herdrSock},
					LockWait: time.Second, Poll: 20 * time.Millisecond, Logger: zerolog.Nop(),
					Hooks: daemon.Hooks{StartTime: func(int) (uint64, error) { return 9, nil }},
				})
				if err != nil && !errors.Is(err, daemon.ErrAlreadyRunning) {
					p.t.Errorf("in-process daemon: %v", err)
				}
			}()
			p.t.Cleanup(func() { cancel(); <-done })
			p.mu.Lock()
			defer p.mu.Unlock()
			p.runs++
			p.cancel[os.Getpid()] = cancel
			return os.Getpid(), nil
		},
		Signal: func(pid int) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			if c, ok := p.cancel[pid]; ok {
				c()
				delete(p.cancel, pid)
			}
			return nil
		},
		StartTime: func(int) (uint64, error) { return 9, nil },
	}
}

func daemonCtx(t *testing.T, e daemonEnv, p *inProcess, logs *bytes.Buffer) context.Context {
	t.Helper()
	logger, err := logging.New(logs, "debug", "json")
	require.NoError(t, err)
	ctx := logging.Into(t.Context(), logger)
	ctx = config.Into(ctx, config.Config{DaemonLockWait: time.Second})
	ctx = cli.WithLookupEnv(ctx, e.lookup(nil))
	if p != nil {
		ctx = cli.WithDaemonHooks(ctx, p.hooks())
	}
	return ctx
}

// TestDaemonLifecycleCommands drives the commands the startup hook and the
// daemon-restart action run, in order, against one scratch herdr.
func TestDaemonLifecycleCommands(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	p := &inProcess{t: t, e: e, cancel: map[int]context.CancelFunc{}}
	var logs bytes.Buffer
	ctx := daemonCtx(t, e, p, &logs)

	out, err := runCtx(ctx, "daemon", "start")
	require.NoError(t, err)
	assert.Contains(t, out, "daemon running")

	out, err = runCtx(ctx, "daemon", "start")
	require.NoError(t, err)
	assert.Contains(t, out, "already running")

	out, err = runCtx(ctx, "daemon", "health", "--json")
	require.NoError(t, err)
	var info daemon.HealthInfo
	require.NoError(t, json.Unmarshal([]byte(out), &info))
	assert.Equal(t, os.Getpid(), info.PID)
	assert.EqualValues(t, 22, info.HerdrProtocol)

	out, err = runCtx(ctx, "daemon", "restart")
	require.NoError(t, err)
	assert.Contains(t, out, "daemon stopped")
	assert.Contains(t, out, "daemon running")

	out, err = runCtx(ctx, "daemon", "stop")
	require.NoError(t, err)
	assert.Contains(t, out, "daemon stopped")

	out, err = runCtx(ctx, "daemon", "stop")
	require.NoError(t, err, "stopping a stopped daemon is done, not failed")
	assert.Contains(t, out, "no daemon running")

	out, err = runCtx(ctx, "daemon", "restart")
	require.NoError(t, err, "restart starts a daemon that was not running")
	assert.Contains(t, out, "daemon running")
	assert.Equal(t, 3, p.runs, "start, restart and restart each spawned once; the second start did not")
}

// TestDaemonHealthText: the human form names the pid, the herdr protocol and
// the socket.
func TestDaemonHealthText(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	p := &inProcess{t: t, e: e, cancel: map[int]context.CancelFunc{}}
	var logs bytes.Buffer
	ctx := daemonCtx(t, e, p, &logs)
	_, err := runCtx(ctx, "daemon", "start")
	require.NoError(t, err)

	out, err := runCtx(ctx, "daemon", "health")
	require.NoError(t, err)
	assert.Contains(t, out, "pid")
	assert.Contains(t, out, "protocol 22")
}

// TestDaemonCommandsFailClearly: what each command reports when it cannot do
// its job. Every failure is an error, never a silent exit 0.
func TestDaemonCommandsFailClearly(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args []string
		env  func(e daemonEnv) map[string]string
		want string
	}{
		"health with no daemon": {args: []string{"daemon", "health"}, want: "no daemon"},
		"no state dir": {
			args: []string{"daemon", "start"},
			env:  func(daemonEnv) map[string]string { return map[string]string{"HERDR_PLUGIN_STATE_DIR": ""} },
			want: "HERDR_PLUGIN_STATE_DIR",
		},
		"no herdr": {
			args: []string{"daemon", "start"},
			env:  func(daemonEnv) map[string]string { return map[string]string{"HERDR_SOCKET_PATH": ""} },
			want: "HERDR_SOCKET_PATH",
		},
		"unknown subcommand": {args: []string{"daemon", "bogus"}, want: "bogus"},
		"bare group":         {args: []string{"daemon"}, want: "subcommand"},
		"positional args":    {args: []string{"daemon", "start", "now"}, want: "now"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnv(t)
			var logs bytes.Buffer
			ctx := daemonCtx(t, e, &inProcess{t: t, e: e, cancel: map[int]context.CancelFunc{}}, &logs)
			if tc.env != nil {
				ctx = cli.WithLookupEnv(ctx, e.lookup(tc.env(e)))
			}
			_, err := runCtx(ctx, tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestDaemonStateDirFlag: by hand, outside herdr, the state directory comes
// from --state-dir.
func TestDaemonStateDirFlag(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	p := &inProcess{t: t, e: e, cancel: map[int]context.CancelFunc{}}
	var logs bytes.Buffer
	ctx := daemonCtx(t, e, p, &logs)
	_, err := runCtx(ctx, "daemon", "start")
	require.NoError(t, err)

	ctx = cli.WithLookupEnv(ctx, e.lookup(map[string]string{"HERDR_PLUGIN_STATE_DIR": ""}))
	out, err := runCtx(ctx, "daemon", "health", "--state-dir", e.stateDir)
	require.NoError(t, err)
	assert.Contains(t, out, "protocol 22")
}

// TestDaemonRunLogsToItsCappedFile: `daemon run` is the daemon itself. It logs
// to daemon.log in the state directory, exits 0 when a daemon of this herdr is
// already running, and stops cleanly when its context ends.
func TestDaemonRunLogsToItsCappedFile(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(daemonCtx(t, e, nil, &logs))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runCtx(ctx, "daemon", "run")
		done <- err
	}()
	paths, err := daemon.PathsFor(e.stateDir)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := daemon.Health(t.Context(), paths.Socket)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond)

	out, err := runCtx(daemonCtx(t, e, nil, &logs), "daemon", "run")
	require.NoError(t, err, "a second run for the same herdr exits 0")
	assert.Empty(t, out)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon run did not stop")
	}
	raw, err := os.ReadFile(paths.Log)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "daemon running")
	assert.Contains(t, string(raw), "already running")
}

// TestDaemonRunRefusesAnUnverifiedProtocol is the pin through the command,
// and the override through configuration (HERDR_AGENTISAN_ALLOW_UNVERIFIED).
func TestDaemonRunRefusesAnUnverifiedProtocol(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "dc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := daemonEnv{herdrSock: filepath.Join(dir, "h.sock"), stateDir: filepath.Join(dir, "state")}
	herdrtest.StartAt(t, e.herdrSock, func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: map[string]any{"type": "pong", "version": "0.10.0", "protocol": 23}}
	})
	var logs bytes.Buffer

	_, err = runCtx(daemonCtx(t, e, nil, &logs), "daemon", "run")
	require.ErrorIs(t, err, daemon.ErrUnverifiedProtocol)

	ctx, cancel := context.WithCancel(config.Into(daemonCtx(t, e, nil, &logs),
		config.Config{DaemonLockWait: time.Second, AllowUnverified: true}))
	done := make(chan error, 1)
	go func() {
		_, err := runCtx(ctx, "daemon", "run")
		done <- err
	}()
	paths, err := daemon.PathsFor(e.stateDir)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		info, err := daemon.Health(t.Context(), paths.Socket)
		return err == nil && info.HerdrProtocol == 23
	}, 3*time.Second, 10*time.Millisecond, "the override runs protocol 23")
	cancel()
	<-done
}

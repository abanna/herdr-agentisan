package daemon_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// shortDir returns a fresh directory with a short path: unix socket paths
// are capped near 104 bytes, and t.TempDir embeds the whole test name.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ds")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func pong(protocol uint32) herdrtest.Handler {
	return func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: map[string]any{"type": "pong", "version": "0.9.3", "protocol": protocol}}
	}
}

// fakeHerdr is a herdrtest server at a path the test can restart it at. It
// answers ping, pane.list (with the pane the test says is focused) and
// events.subscribe, whose streams the test drives.
type fakeHerdr struct {
	t    *testing.T
	path string
	srv  *herdrtest.Server
	h    herdrtest.Handler

	mu      sync.Mutex
	focused string // the pane pane.list reports focused; "" for none
	listErr bool   // pane.list answers with an error
	refuse  int    // how many events.subscribe requests to refuse first
	// streams carries every subscription the server opens, in order.
	streams chan *stream
}

// stream is one events.subscribe connection the fake herdr is serving.
type stream struct {
	id    string
	lines chan []byte
}

func newHerdr(t *testing.T, protocol uint32) *fakeHerdr {
	t.Helper()
	f := &fakeHerdr{t: t, path: filepath.Join(shortDir(t), "h.sock"), streams: make(chan *stream, 64)}
	f.h = f.serve(protocol)
	f.srv = herdrtest.StartAt(t, f.path, f.h)
	return f
}

func (f *fakeHerdr) serve(protocol uint32) herdrtest.Handler {
	return func(r herdrtest.Request) herdrtest.Reply {
		switch r.Method {
		case "ping":
			return pong(protocol)(r)
		case "pane.list":
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.listErr {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
			}
			panes := []map[string]any{{"pane_id": "w9:p9", "workspace_id": "w9", "focused": false}}
			if f.focused != "" {
				panes = append(panes, map[string]any{"pane_id": f.focused, "workspace_id": "w1", "focused": true})
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": panes}}
		case "events.subscribe":
			f.mu.Lock()
			refused := f.refuse > 0
			if refused {
				f.refuse--
			}
			f.mu.Unlock()
			if refused {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "try again"}}
			}
			s := &stream{id: r.ID, lines: make(chan []byte, 64)}
			select {
			case f.streams <- s:
			default:
			}
			return herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), Stream: s.lines}
		}
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "invalid_request", Message: "unknown method " + r.Method}}
	}
}

// focus sets the pane pane.list reports focused.
func (f *fakeHerdr) focus(pane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.focused = pane
}

// nextStream waits for the daemon's next subscription.
func (f *fakeHerdr) nextStream(t *testing.T) *stream {
	t.Helper()
	select {
	case s := <-f.streams:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon did not subscribe to focus events")
		return nil
	}
}

// subscribes counts the events.subscribe requests the current server got.
func (f *fakeHerdr) subscribes() int {
	n := 0
	for _, r := range f.srv.Requests() {
		if r.Method == "events.subscribe" {
			n++
		}
	}
	return n
}

// restart replaces the socket at the same path, so its inode changes, as a
// herdr live handoff does.
func (f *fakeHerdr) restart() {
	f.srv.Close()
	f.srv = herdrtest.StartAt(f.t, f.path, f.h)
}

func (f *fakeHerdr) stop() { f.srv.Close() }

// crash kills the server but leaves its socket file behind, as a herdr that
// was SIGKILLed does: the identity is unchanged, nothing answers.
func (f *fakeHerdr) crash() { f.srv.Crash() }

// procs is a fake process table for start times: pid -> start.
type procs struct {
	mu    sync.Mutex
	start map[int]uint64
}

func (p *procs) startTime(pid int) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.start[pid]
	if !ok {
		return 0, errors.New("no such process")
	}
	return s, nil
}

func (p *procs) set(pid int, start uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.start[pid] = start
}

// opts builds Options for one daemon on h, with fast polling.
func opts(t *testing.T, stateDir string, h *fakeHerdr) daemon.Options {
	t.Helper()
	return daemon.Options{
		StateDir:    stateDir,
		HerdrSocket: h.path,
		Herdr:       herdr.Client{SocketPath: h.path},
		LockWait:    time.Second,
		Poll:        20 * time.Millisecond,
		StartWait:   2 * time.Second,
		StopWait:    2 * time.Second,
		Version:     "v-test",
		Commit:      "c-test",
		Logger:      zerolog.Nop(),
		Hooks: daemon.Hooks{
			StartTime: (&procs{start: map[int]uint64{os.Getpid(): 77}}).startTime,
		},
	}
}

// running is a daemon.Run in a goroutine, as `daemon run` would be.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func run(t *testing.T, o daemon.Options) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- daemon.Run(ctx, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop within 5 s of cancel")
		}
	})
	return r
}

// wait returns the daemon's exit error, failing the test after d.
func (r *running) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // keep it for Cleanup
		return err
	case <-time.After(d):
		t.Fatalf("daemon still running after %v", d)
		return nil
	}
}

func (r *running) alive() bool {
	select {
	case err := <-r.done:
		r.done <- err
		return false
	default:
		return true
	}
}

func mustIdentity(t *testing.T, sock string) daemon.Identity {
	t.Helper()
	id, err := daemon.HerdrIdentity(sock)
	require.NoError(t, err)
	return id
}

func socketPath(t *testing.T, stateDir, herdrSocket string) string {
	t.Helper()
	p, err := daemon.PathsFor(stateDir, herdrSocket)
	require.NoError(t, err)
	return p.Socket
}

// healthy waits until h's daemon in stateDir answers health.
func healthy(t *testing.T, stateDir string, h *fakeHerdr) daemon.HealthInfo {
	t.Helper()
	var info daemon.HealthInfo
	require.Eventually(t, func() bool {
		var err error
		info, err = daemon.Health(t.Context(), socketPath(t, stateDir, h.path))
		return err == nil
	}, 3*time.Second, 10*time.Millisecond, "the daemon never answered health")
	return info
}

func TestPathsForClasses(t *testing.T) {
	t.Parallel()

	const herdrSock = "/run/user/1000/herdr.sock"
	// A state dir whose socket path ("<dir>/srv-<12 hex>/daemon.sock") is
	// exactly n bytes.
	dirFor := func(n int) string {
		return "/" + strings.Repeat("d", n-len("/srv-0123456789ab/daemon.sock")-1)
	}
	tests := map[string]struct {
		dir, sock string
		want      error
	}{
		"socket path of 103 bytes":         {dir: dirFor(103), sock: herdrSock},
		"socket path of 104 bytes":         {dir: dirFor(104), sock: herdrSock, want: daemon.ErrSocketPathTooLong},
		"socket path of 105 bytes":         {dir: dirFor(105), sock: herdrSock, want: daemon.ErrSocketPathTooLong},
		"empty state dir":                  {dir: "", sock: herdrSock, want: daemon.ErrNoStateDir},
		"relative state dir":               {dir: "state/agentisan", sock: herdrSock, want: daemon.ErrNoStateDir},
		"non-ASCII counts bytes not runes": {dir: "/" + strings.Repeat("é", 37), sock: herdrSock, want: daemon.ErrSocketPathTooLong},
		"no herdr socket":                  {dir: "/state", sock: "", want: daemon.ErrHerdrGone},
		"relative herdr socket":            {dir: "/state", sock: "herdr.sock", want: daemon.ErrHerdrGone},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := daemon.PathsFor(tc.dir, tc.sock)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.dir, filepath.Dir(p.Dir), "each server's files live one level under the state dir")
			assert.Regexp(t, `^srv-[0-9a-f]{12}$`, filepath.Base(p.Dir))
			assert.Equal(t, filepath.Join(p.Dir, "daemon.lock"), p.Lock)
			assert.Equal(t, filepath.Join(p.Dir, "daemon.log"), p.Log)
			assert.Equal(t, filepath.Join(p.Dir, "daemon.sock"), p.Socket)
			assert.Equal(t, filepath.Join(p.Dir, "state.db"), p.DB)
			assert.Len(t, p.Socket, 103)
		})
	}
}

// TestPathsForKeysEachServer is D3's "one daemon per herdr server": every
// herdr session has its own socket path, and the daemon's files live in a
// directory keyed by it. The same server always maps to the same directory,
// across a live handoff too, because herdr keeps the socket path.
func TestPathsForKeysEachServer(t *testing.T) {
	t.Parallel()

	key := func(sock string) string {
		p, err := daemon.PathsFor("/state", sock)
		require.NoError(t, err)
		return p.Dir
	}
	a := key("/home/u/.config/herdr/herdr.sock")
	assert.Equal(t, a, key("/home/u/.config/herdr/herdr.sock"), "stable for one server")
	assert.Equal(t, a, key("/home/u/.config/herdr//herdr.sock"), "the path is cleaned first")
	assert.NotEqual(t, a, key("/home/u/.config/herdr/sessions/work/herdr.sock"), "each named session has its own")
}

// TestRunServesHealthUntilCancelled: a running daemon answers health over
// its socket, and on cancel (SIGTERM in production) it removes the socket
// and releases the lock.
func TestRunServesHealthUntilCancelled(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	dir := shortDir(t)
	r := run(t, opts(t, dir, h))

	info := healthy(t, dir, h)
	assert.Equal(t, os.Getpid(), info.PID)
	assert.Equal(t, "v-test", info.Version)
	assert.Equal(t, "c-test", info.Commit)
	assert.EqualValues(t, 22, info.HerdrProtocol)
	assert.Equal(t, h.path, info.HerdrSocket)
	assert.Equal(t, mustIdentity(t, h.path), info.Herdr)
	assert.WithinDuration(t, time.Now(), info.StartedAt, 5*time.Second)
	assert.Equal(t, store.SchemaVersion(), info.StoreVersion)
	assert.Zero(t, info.FocusRows, "no pane is focused, so nothing is recorded")

	st, err := os.Stat(socketPath(t, dir, h.path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "the socket is the owner's only")

	r.cancel()
	require.NoError(t, r.wait(t, 3*time.Second), "a cancelled daemon stops cleanly")
	assert.NoFileExists(t, socketPath(t, dir, h.path))
	_, err = daemon.Health(t.Context(), socketPath(t, dir, h.path))
	require.ErrorIs(t, err, daemon.ErrUnavailable)
}

// TestRunExitsWhenHerdrGoes: the daemon belongs to one herdr server. When
// that server's socket disappears or is replaced, the daemon exits, and the
// next server's startup hook launches a fresh one.
func TestRunExitsWhenHerdrGoes(t *testing.T) {
	t.Parallel()

	for name, end := range map[string]func(*fakeHerdr){
		"socket removed": (*fakeHerdr).stop,
		// ext4 often hands the new socket the old inode number; the birth
		// time still tells them apart.
		"socket replaced (inode often reused)":        (*fakeHerdr).restart,
		"server crashed, its socket file left behind": (*fakeHerdr).crash,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHerdr(t, 22)
			dir := shortDir(t)
			r := run(t, opts(t, dir, h))
			healthy(t, dir, h)

			end(h)
			require.ErrorIs(t, r.wait(t, 3*time.Second), daemon.ErrHerdrGone)
			assert.NoFileExists(t, socketPath(t, dir, h.path))
		})
	}
}

// TestHerdrIdentityClasses: the identity tells one bound socket from every
// later one at the same path, even when the filesystem reuses the inode
// number, and does not change for the same socket.
func TestHerdrIdentityClasses(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortDir(t), "h.sock")
	bind := func() {
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		require.NoError(t, ln.Close())
	}

	bind()
	first := mustIdentity(t, path)
	assert.Equal(t, first, mustIdentity(t, path), "stable for the same socket")
	require.NoError(t, os.Chmod(path, 0o600))
	assert.Equal(t, first, mustIdentity(t, path), "a chmod is not a new server")

	reused := false
	for range 20 {
		require.NoError(t, os.Remove(path))
		bind()
		next := mustIdentity(t, path)
		assert.NotEqual(t, first, next, "every bind is a new server")
		reused = reused || next.Ino == first.Ino
		first = next
	}
	t.Logf("inode number reused across rebinds: %v", reused)

	plain := filepath.Join(shortDir(t), "f")
	require.NoError(t, os.WriteFile(plain, nil, 0o600))
	_, err := daemon.HerdrIdentity(plain)
	require.ErrorIs(t, err, daemon.ErrHerdrGone, "a regular file is no server")
	_, err = daemon.HerdrIdentity(filepath.Join(shortDir(t), "absent.sock"))
	require.ErrorIs(t, err, daemon.ErrHerdrGone)
}

// TestTwoServersEachGetTheirOwnDaemon: two herdr sessions share the plugin
// state dir. Each gets its own daemon, health answers for the caller's
// server, and stopping one leaves the other running.
func TestTwoServersEachGetTheirOwnDaemon(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	a, b := newHerdr(t, 22), newHerdr(t, 22)
	ra, rb := run(t, opts(t, dir, a)), run(t, opts(t, dir, b))

	assert.Equal(t, mustIdentity(t, a.path), healthy(t, dir, a).Herdr)
	assert.Equal(t, mustIdentity(t, b.path), healthy(t, dir, b).Herdr)

	ob := opts(t, dir, b)
	ob.Hooks.Signal = func(int) error { rb.cancel(); return nil }
	require.NoError(t, daemon.Stop(t.Context(), ob))
	require.NoError(t, rb.wait(t, time.Second))
	assert.True(t, ra.alive(), "stopping B's daemon leaves A's running")
	assert.Equal(t, mustIdentity(t, a.path), healthy(t, dir, a).Herdr)
}

// TestRunNeedsHerdr: a daemon with no herdr socket to belong to never starts.
func TestRunNeedsHerdr(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	h.stop()
	o := opts(t, shortDir(t), h)

	err := daemon.Run(t.Context(), o)
	require.ErrorIs(t, err, daemon.ErrHerdrGone)

	o.HerdrSocket = ""
	require.ErrorIs(t, daemon.Run(t.Context(), o), daemon.ErrHerdrGone)
}

// TestTwoRunsLeaveOneDaemon is I6: whichever run takes the lock serves; the
// other finds it held by a daemon of the same herdr server and exits.
func TestTwoRunsLeaveOneDaemon(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	dir := shortDir(t)
	o := opts(t, dir, h)

	a, b := run(t, o), run(t, o)
	healthy(t, dir, h)
	require.Eventually(t, func() bool { return a.alive() != b.alive() }, 3*time.Second, 10*time.Millisecond,
		"exactly one daemon must keep running")
	loser := a
	if a.alive() {
		loser = b
	}
	require.ErrorIs(t, loser.wait(t, time.Second), daemon.ErrAlreadyRunning)
}

// holdLock takes daemon.lock the way a daemon does and writes info into it,
// standing in for a daemon of some other herdr server.
func holdLock(t *testing.T, stateDir string, h *fakeHerdr, info string) (release func()) {
	t.Helper()
	p, err := daemon.PathsFor(stateDir, h.path)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	f, err := os.OpenFile(p.Lock, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	require.NoError(t, f.Truncate(0))
	_, err = f.WriteAt([]byte(info), 0)
	require.NoError(t, err)
	var once sync.Once
	release = func() { once.Do(func() { _ = f.Close() }) }
	t.Cleanup(release)
	return release
}

// staleHolder is a lock held by a daemon bound to some earlier herdr socket.
const staleHolder = `{"v":1,"pid":1234567,"start":5,"herdr":{"dev":1,"ino":1},"started_at":"2026-10-09T00:00:00Z"}`

// TestRunWaitsForAStaleHolder is ADR-001 A3: during a live handoff the new
// server's startup hook runs while the old daemon still holds the lock. A
// run that finds the lock held for a different (stale) herdr socket waits
// for it, up to LockWait, instead of exiting.
func TestRunWaitsForAStaleHolder(t *testing.T) {
	t.Parallel()

	t.Run("the stale holder lets go: the new daemon takes over", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		release := holdLock(t, dir, h, staleHolder)
		o := opts(t, dir, h)
		o.LockWait = 5 * time.Second
		r := run(t, o)

		time.Sleep(150 * time.Millisecond)
		require.True(t, r.alive(), "the new daemon waits while the stale one holds the lock")
		release()
		info := healthy(t, dir, h)
		assert.Equal(t, os.Getpid(), info.PID)
	})

	for name, holder := range map[string]string{
		"a stale holder that never lets go":       staleHolder,
		"a holder whose record cannot be read":    `{"v":1,"pid":`,
		"a holder whose record is empty":          ``,
		"a holder whose record is another format": `not json`,
	} {
		t.Run(name+" times out", func(t *testing.T) {
			t.Parallel()
			h := newHerdr(t, 22)
			dir := shortDir(t)
			holdLock(t, dir, h, holder)
			o := opts(t, dir, h)
			o.LockWait = 300 * time.Millisecond

			start := time.Now()
			err := daemon.Run(t.Context(), o)
			require.ErrorIs(t, err, daemon.ErrLockTimeout)
			assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
			assert.Less(t, time.Since(start), 3*time.Second)
		})
	}
}

// TestRunProtocolPin is D2/A4: a herdr protocol this build has not been
// verified against stops the daemon with a clear error, unless overridden.
func TestRunProtocolPin(t *testing.T) {
	t.Parallel()

	t.Run("verified protocol runs", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		run(t, opts(t, dir, h))
		healthy(t, dir, h)
	})
	t.Run("unverified protocol stops with an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 23)
		err := daemon.Run(t.Context(), opts(t, shortDir(t), h))
		require.ErrorIs(t, err, daemon.ErrUnverifiedProtocol)
		assert.Contains(t, err.Error(), "23")
		assert.Contains(t, err.Error(), "HERDR_AGENTISAN_ALLOW_UNVERIFIED")
	})
	t.Run("the override runs an unverified protocol", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 23)
		dir := shortDir(t)
		o := opts(t, dir, h)
		o.AllowUnverified = true
		run(t, o)
		assert.EqualValues(t, 23, healthy(t, dir, h).HerdrProtocol)
	})
	t.Run("herdr not answering ping stops with its error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		h.h = func(herdrtest.Request) herdrtest.Reply {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "busy", Message: "try later"}}
		}
		h.restart()
		err := daemon.Run(t.Context(), opts(t, shortDir(t), h))
		require.ErrorIs(t, err, herdr.ErrAPI)
	})
}

// spawnInProcess stands in for exec'ing `daemon run`: it runs the daemon in a
// goroutine and reports this process's pid.
func spawnInProcess(t *testing.T, o daemon.Options, spawned *[]*running, mu *sync.Mutex) func(context.Context) (int, error) {
	t.Helper()
	return func(context.Context) (int, error) {
		r := run(t, o)
		mu.Lock()
		*spawned = append(*spawned, r)
		mu.Unlock()
		return os.Getpid(), nil
	}
}

func TestStartClasses(t *testing.T) {
	t.Parallel()

	t.Run("nothing running: spawns and waits for health", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		var spawned []*running
		var mu sync.Mutex
		o.Hooks.Spawn = spawnInProcess(t, o, &spawned, &mu)

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: os.Getpid(), Healthy: true}, res)
		assert.Len(t, spawned, 1)
	})
	t.Run("already running for this herdr: spawns nothing", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		run(t, o)
		healthy(t, dir, h)
		o.Hooks.Spawn = func(context.Context) (int, error) {
			t.Error("Start spawned a second daemon")
			return 0, nil
		}

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: os.Getpid(), AlreadyRunning: true, Healthy: true}, res)
	})
	// During a live handoff the old daemon still answers health until it
	// notices its socket is gone. Its answer names the old server, so it
	// must not count as the new server's daemon having started.
	t.Run("a stale daemon still serving does not count as started", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		old := opts(t, dir, h)
		old.Poll = time.Hour // it never notices the restart below
		oldRun := run(t, old)
		healthy(t, dir, h)
		h.restart()

		o := opts(t, dir, h)
		o.StartWait = 300 * time.Millisecond
		o.LockWait = 5 * time.Second
		var spawned []*running
		var mu sync.Mutex
		o.Hooks.Spawn = spawnInProcess(t, o, &spawned, &mu)

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.False(t, res.Healthy, "the old server's daemon answered, not the new one")

		oldRun.cancel()
		require.Eventually(t, func() bool {
			info, err := daemon.Health(t.Context(), socketPath(t, dir, h.path))
			return err == nil && info.Herdr == mustIdentity(t, h.path)
		}, 3*time.Second, 10*time.Millisecond, "the new daemon takes over once the old one lets go")
	})
	t.Run("a spawn that fails is an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		o.Hooks.Spawn = func(context.Context) (int, error) { return 0, errors.New("exec: no such file") }

		_, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrSpawn)
	})
	t.Run("a spawned daemon that never answers is reported, not an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		o.StartWait = 200 * time.Millisecond
		o.Hooks.Spawn = func(context.Context) (int, error) { return 4242, nil }

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: 4242}, res)
	})
	t.Run("no herdr: an error, nothing spawned", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		h.stop()
		o := opts(t, shortDir(t), h)
		o.Hooks.Spawn = func(context.Context) (int, error) {
			t.Error("spawned with no herdr")
			return 0, nil
		}
		_, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrHerdrGone)
	})
}

// TestTwoConcurrentStartsLeaveOneDaemon is I6 through the start path: both
// starts may spawn, but only one spawned daemon keeps running.
func TestTwoConcurrentStartsLeaveOneDaemon(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	dir := shortDir(t)
	o := opts(t, dir, h)
	var spawned []*running
	var mu sync.Mutex
	o.Hooks.Spawn = spawnInProcess(t, o, &spawned, &mu)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			res, err := daemon.Start(t.Context(), o)
			assert.NoError(t, err)
			assert.True(t, res.Healthy)
		})
	}
	wg.Wait()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		alive := 0
		for _, r := range spawned {
			if r.alive() {
				alive++
			}
		}
		return alive == 1
	}, 3*time.Second, 10*time.Millisecond, "exactly one daemon must keep running")
}

func TestStopClasses(t *testing.T) {
	t.Parallel()

	t.Run("not running", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		o.Hooks.Signal = func(int) error {
			t.Error("signalled with no daemon running")
			return nil
		}
		require.ErrorIs(t, daemon.Stop(t.Context(), o), daemon.ErrNotRunning)
	})
	t.Run("running: signals the lock holder and waits for the lock", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		r := run(t, o)
		healthy(t, dir, h)
		var got int
		o.Hooks.Signal = func(pid int) error { got = pid; r.cancel(); return nil }

		require.NoError(t, daemon.Stop(t.Context(), o))
		assert.Equal(t, os.Getpid(), got)
		require.NoError(t, r.wait(t, time.Second))
	})
	t.Run("a wedged daemon whose socket is gone is still stopped", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		r := run(t, o)
		healthy(t, dir, h)
		require.NoError(t, os.Remove(socketPath(t, dir, h.path)))
		o.Hooks.Signal = func(int) error { r.cancel(); return nil }

		require.NoError(t, daemon.Stop(t.Context(), o))
	})
	t.Run("a holder whose start time changed is never signalled", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		holdLock(t, dir, h, `{"v":1,"pid":4242,"start":5,"herdr":{"dev":1,"ino":1},"started_at":"2026-10-09T00:00:00Z"}`)
		o := opts(t, dir, h)
		o.Hooks.StartTime = (&procs{start: map[int]uint64{4242: 6}}).startTime
		o.Hooks.Signal = func(int) error {
			t.Error("signalled a pid that is no longer the holder")
			return nil
		}
		require.ErrorIs(t, daemon.Stop(t.Context(), o), daemon.ErrHolderChanged)
	})
	t.Run("a holder record without a pid is never signalled", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		holdLock(t, dir, h, `garbage`)
		o := opts(t, dir, h)
		o.StopWait = 200 * time.Millisecond
		o.Hooks.Signal = func(int) error {
			t.Error("signalled without a pid")
			return nil
		}
		require.ErrorIs(t, daemon.Stop(t.Context(), o), daemon.ErrHolderChanged)
	})
	t.Run("a holder that ignores the signal times out", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		run(t, o)
		healthy(t, dir, h)
		o.StopWait = 200 * time.Millisecond
		o.Hooks.Signal = func(int) error { return nil }

		require.ErrorIs(t, daemon.Stop(t.Context(), o), daemon.ErrStopTimeout)
	})
	t.Run("a signal that fails is an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		run(t, o)
		healthy(t, dir, h)
		o.Hooks.Signal = func(int) error { return syscall.EPERM }

		require.ErrorIs(t, daemon.Stop(t.Context(), o), syscall.EPERM)
	})
}

// request sends one raw line to the daemon socket and returns the raw reply.
func request(t *testing.T, sock string, line []byte) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer conn.Close() //nolint:errcheck // test connection
	_, err = conn.Write(line)
	require.NoError(t, err)
	if c, ok := conn.(*net.UnixConn); ok {
		_ = c.CloseWrite()
	}
	raw, err := bufio.NewReader(conn).ReadBytes('\n')
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// TestSocketProtocolClasses walks what a client can send the daemon socket
// (D6): one JSON request per connection, {"v":1,"op","args"}, answered with
// {"ok":true,"data"} or {"ok":false,"error":{"code","message"}}.
func TestSocketProtocolClasses(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	dir := shortDir(t)
	run(t, opts(t, dir, h))
	healthy(t, dir, h)
	sock := socketPath(t, dir, h.path)

	tests := map[string]struct {
		line string
		code string // "" means ok
	}{
		"health":                    {line: `{"v":1,"op":"health","args":{}}` + "\n"},
		"health without args":       {line: `{"v":1,"op":"health"}` + "\n"},
		"no trailing newline":       {line: `{"v":1,"op":"health"}`},
		"CRLF line ending":          {line: `{"v":1,"op":"health"}` + "\r\n"},
		"unknown op":                {line: `{"v":1,"op":"snapshot"}` + "\n", code: "unknown_op"},
		"op in another case":        {line: `{"v":1,"op":"HEALTH"}` + "\n", code: "unknown_op"},
		"missing version":           {line: `{"op":"health"}` + "\n", code: "unsupported_version"},
		"newer version":             {line: `{"v":2,"op":"health"}` + "\n", code: "unsupported_version"},
		"not JSON":                  {line: "health\n", code: "bad_request"},
		"empty line":                {line: "\n", code: "bad_request"},
		"two objects":               {line: `{"v":1,"op":"health"}{"v":1,"op":"health"}` + "\n", code: "bad_request"},
		"invalid UTF-8 in the op":   {line: "{\"v\":1,\"op\":\"he\xffalth\"}\n", code: "unknown_op"},
		"over the request size cap": {line: `{"v":1,"op":"health","args":"` + strings.Repeat("x", 1<<20) + `"}` + "\n", code: "bad_request"},
		"op is not a string":        {line: `{"v":1,"op":7}` + "\n", code: "bad_request"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := request(t, sock, []byte(tc.line))
			if tc.code == "" {
				assert.Equal(t, true, got["ok"])
				data, ok := got["data"].(map[string]any)
				require.True(t, ok, "ok responses carry data")
				assert.EqualValues(t, os.Getpid(), data["pid"])
				return
			}
			assert.Equal(t, false, got["ok"])
			e, ok := got["error"].(map[string]any)
			require.True(t, ok, "error responses carry an error object")
			assert.Equal(t, tc.code, e["code"])
			assert.NotEmpty(t, e["message"])
		})
	}
}

// TestHealthClientClasses: what the client makes of each way the socket can
// fail. Every failure is a sentinel a caller can branch on.
func TestHealthClientClasses(t *testing.T) {
	t.Parallel()

	serve := func(t *testing.T, reply string) string {
		t.Helper()
		sock := filepath.Join(shortDir(t), "x.sock")
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
		return sock
	}

	tests := map[string]struct {
		sock func(t *testing.T) string
		want error
	}{
		"no socket": {sock: func(t *testing.T) string { return filepath.Join(shortDir(t), "none.sock") }, want: daemon.ErrUnavailable},
		"not a socket": {sock: func(t *testing.T) string {
			p := filepath.Join(shortDir(t), "f")
			require.NoError(t, os.WriteFile(p, nil, 0o600))
			return p
		}, want: daemon.ErrUnavailable},
		"closed early":    {sock: func(t *testing.T) string { return serve(t, "") }, want: daemon.ErrUnavailable},
		"garbage reply":   {sock: func(t *testing.T) string { return serve(t, "nope\n") }, want: daemon.ErrProtocol},
		"ok without data": {sock: func(t *testing.T) string { return serve(t, `{"ok":true}`+"\n") }, want: daemon.ErrProtocol},
		"an error reply": {sock: func(t *testing.T) string {
			return serve(t, `{"ok":false,"error":{"code":"busy","message":"later"}}`+"\n")
		}, want: daemon.ErrRequest},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := daemon.Health(t.Context(), tc.sock(t))
			require.ErrorIs(t, err, tc.want)
		})
	}

	t.Run("an error reply carries its code", func(t *testing.T) {
		t.Parallel()
		sock := serve(t, `{"ok":false,"error":{"code":"busy","message":"later"}}`+"\n")
		_, err := daemon.Health(t.Context(), sock)
		var re *daemon.RequestError
		require.ErrorAs(t, err, &re)
		assert.Equal(t, "busy", re.Code)
		assert.Equal(t, "later", re.Message)
	})
}

// TestRunReplacesAStaleSocket: a daemon that crashed leaves its socket file
// behind. The next lock holder replaces it rather than failing to bind.
func TestRunReplacesAStaleSocket(t *testing.T) {
	t.Parallel()

	for name, plant := range map[string]func(t *testing.T, path string){
		"a dead socket": func(t *testing.T, path string) {
			ln, err := net.Listen("unix", path)
			require.NoError(t, err)
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			require.NoError(t, ln.Close())
		},
		"a regular file": func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHerdr(t, 22)
			dir := shortDir(t)
			sock := socketPath(t, dir, h.path)
			require.NoError(t, os.MkdirAll(filepath.Dir(sock), 0o700))
			plant(t, sock)
			run(t, opts(t, dir, h))
			healthy(t, dir, h)
		})
	}
}

// TestUnwritableStateDir: a state directory the daemon cannot write is an
// error from both run and start, never a hang or a silent exit.
func TestUnwritableStateDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	h := newHerdr(t, 22)
	dir := shortDir(t)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	o := opts(t, dir, h)
	o.Hooks.Spawn = func(context.Context) (int, error) {
		t.Error("spawned into an unwritable state dir")
		return 0, nil
	}

	require.ErrorIs(t, daemon.Run(t.Context(), o), os.ErrPermission)
	_, err := daemon.Start(t.Context(), o)
	require.ErrorIs(t, err, os.ErrPermission)
}

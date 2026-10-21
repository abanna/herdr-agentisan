package daemon_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// ready bounds how long a test waits for an in-process daemon to come up or
// stop, or for what it records to land: the daemon's own bounds, its ping's
// (herdr.CallTimeout) and its store's waits (store.BusyTimeout). It is not a
// literal few seconds because a fresh state.db costs ten fsyncs, which take
// seconds on a runner whose disk other suites keep busy: under an fsync hog a
// fresh store.Open took a median of 2.3 s and up to 5.3 s, against 40 ms
// idle. A daemon that is ready ends every wait at once, so the size costs
// nothing when nothing is wrong.
const ready = herdr.CallTimeout + store.BusyTimeout

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

	// What Back reads and moves (back_test.go). panes lists every pane;
	// nil keeps pane.list to one unfocused filler plus the focused pane.
	panes  []string
	zoomed map[string]bool
	outbox [][]byte          // pane_focused events not yet streamed
	eager  bool              // stream an event before answering, not after
	fail   map[string]string // method -> the error code it answers
	hang   chan struct{}     // pane.zoom waits for it to close
	live   *stream           // the newest subscription
	// before, when set, runs before each request is answered.
	before func(method string)
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
		f.mu.Lock()
		before := f.before
		f.mu.Unlock()
		if before != nil {
			before(r.Method)
		}
		switch r.Method {
		case "ping":
			return pong(protocol)(r)
		case "pane.list":
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.listErr {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
			}
			if f.panes != nil {
				return f.list()
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
			f.mu.Lock()
			f.live = s
			f.mu.Unlock()
			select {
			case f.streams <- s:
			default:
			}
			return herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), Stream: s.lines}
		case "pane.zoom", "pane.focus":
			return f.move(r)
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
	case <-time.After(ready):
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
		StartWait:   ready,
		StopWait:    ready,
		Version:     "v-test",
		Commit:      "c-test",
		Logger:      zerolog.Nop(),
		Hooks: daemon.Hooks{
			StartTime: (&procs{start: map[int]uint64{os.Getpid(): 77}}).startTime,
		},
	}
}

// restarted is o for a daemon that follows one which has exited, as the
// new process `daemon run` would be: its StartTime hook gives this process
// a later start than opts' 77. Every daemon a test runs shares the test's
// pid, so with the same start the record the last one left in daemon.lock
// names a holder still alive. A child forked anywhere in the test process
// while that daemon held the lock keeps a copy of its descriptor until the
// child execs, so the lock can outlive the daemon for a moment, and a
// daemon taking the record for a live holder stops at once with
// ErrAlreadyRunning (NERD-5335). A new process finds the holder gone and
// waits for the lock, up to LockWait, as it does outside a test.
func restarted(o daemon.Options) daemon.Options {
	o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 78}}).startTime
	return o
}

// running is a daemon.Run in a goroutine, as `daemon run` would be.
type running struct {
	cancel context.CancelFunc
	done   chan error
	// exited is closed once Run has returned, as a spawned process's
	// Spawned.Exited is once it has exited.
	exited chan struct{}
}

func run(t *testing.T, o daemon.Options) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, done: make(chan error, 1), exited: make(chan struct{})}
	go func() {
		defer close(r.exited)
		r.done <- daemon.Run(ctx, o)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(ready):
			t.Errorf("daemon did not stop within %v of cancel", ready)
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
	}, ready, 10*time.Millisecond, "the daemon never answered health")
	return info
}

// healthy waits, as healthy does, until r answers health in stateDir, and
// fails at once with r's exit error if r stops first, rather than waiting
// out ready for a daemon that is gone.
func (r *running) healthy(t *testing.T, stateDir string, h *fakeHerdr) daemon.HealthInfo {
	t.Helper()
	sock := socketPath(t, stateDir, h.path)
	deadline := time.Now().Add(ready)
	for {
		info, err := daemon.Health(t.Context(), sock)
		if err == nil {
			return info
		}
		select {
		case exit := <-r.done:
			r.done <- exit // keep it for Cleanup
			t.Fatalf("the daemon stopped before it answered health: %v", exit)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never answered health within %v: %v", ready, err)
		}
	}
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
	assert.WithinDuration(t, time.Now(), info.StartedAt, ready)
	assert.Equal(t, store.SchemaVersion(), info.StoreVersion)
	assert.Zero(t, info.FocusRows, "no pane is focused, so nothing is recorded")

	st, err := os.Stat(socketPath(t, dir, h.path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "the socket is the owner's only")

	r.cancel()
	require.NoError(t, r.wait(t, ready), "a cancelled daemon stops cleanly")
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
			require.ErrorIs(t, r.wait(t, ready), daemon.ErrHerdrGone)
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
	require.NoError(t, rb.wait(t, ready))
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
	require.Eventually(t, func() bool { return a.alive() != b.alive() }, ready, 10*time.Millisecond,
		"exactly one daemon must keep running")
	loser := a
	if a.alive() {
		loser = b
	}
	require.ErrorIs(t, loser.wait(t, time.Second), daemon.ErrAlreadyRunning)
}

// holdLock takes daemon.lock the way a daemon does and writes info into it,
// standing in for a daemon of some other herdr server, or for a child forked
// while a daemon ran. A lock that a child forked while an earlier daemon ran
// still holds for a moment (see restarted) is waited for, up to ready.
func holdLock(t *testing.T, stateDir string, h *fakeHerdr, info string) (release func()) {
	t.Helper()
	p, err := daemon.PathsFor(stateDir, h.path)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	f, err := os.OpenFile(p.Lock, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	var once sync.Once
	release = func() { once.Do(func() { _ = f.Close() }) }
	t.Cleanup(release)
	deadline := time.Now().Add(ready)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		require.ErrorIs(t, err, syscall.EWOULDBLOCK)
		require.True(t, time.Now().Before(deadline), "daemon.lock was still held after %v", ready)
		time.Sleep(5 * time.Millisecond)
	}
	require.NoError(t, f.Truncate(0))
	_, err = f.WriteAt([]byte(info), 0)
	require.NoError(t, err)
	return release
}

// TestRunAfterADaemonExits: a daemon's lock can outlive it for a moment,
// held by a child forked while it ran, which keeps a copy of the lock's
// descriptor until it execs, with the daemon's record still in the file. A
// new process finds that record's holder gone and waits for the lock. In a
// test, whose daemons share one pid, the predecessor's own start time makes
// the record read as a live daemon of this server: ErrAlreadyRunning at
// once, the flake in NERD-5335, which restarted avoids.
func TestRunAfterADaemonExits(t *testing.T) {
	t.Parallel()

	// lingering runs a daemon, stops it, then holds its lock as a forked
	// child would: through another descriptor, its record left as written.
	lingering := func(t *testing.T) (dir string, h *fakeHerdr, release func()) {
		t.Helper()
		h = newHerdr(t, 22)
		dir = shortDir(t)
		first := run(t, opts(t, dir, h))
		healthy(t, dir, h)
		first.cancel()
		require.NoError(t, first.wait(t, ready))
		record, err := os.ReadFile(paths(t, dir, h).Lock)
		require.NoError(t, err)
		require.Contains(t, string(record), fmt.Sprintf(`"pid":%d,"start":77,`, os.Getpid()))
		return dir, h, holdLock(t, dir, h, string(record))
	}

	t.Run("a new process waits for the lock, then serves", func(t *testing.T) {
		t.Parallel()
		dir, h, release := lingering(t)
		o := restarted(opts(t, dir, h))
		o.LockWait = 5 * time.Second
		r := run(t, o)

		time.Sleep(150 * time.Millisecond)
		require.True(t, r.alive(), "it waits while the lock outlives its holder")
		_, err := daemon.Health(t.Context(), socketPath(t, dir, h.path))
		require.Error(t, err, "and serves nothing until it has the lock")
		release()
		assert.Equal(t, os.Getpid(), r.healthy(t, dir, h).PID)
	})
	t.Run("the predecessor's start time reads as a live daemon", func(t *testing.T) {
		t.Parallel()
		dir, h, _ := lingering(t)
		ctx, cancel := context.WithTimeout(t.Context(), ready)
		defer cancel()
		require.ErrorIs(t, daemon.Run(ctx, opts(t, dir, h)), daemon.ErrAlreadyRunning)
	})
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
// goroutine and reports this process's pid, and the daemon's return as its
// exit.
func spawnInProcess(t *testing.T, o daemon.Options, spawned *[]*running, mu *sync.Mutex) func(context.Context) (daemon.Spawned, error) {
	t.Helper()
	return func(context.Context) (daemon.Spawned, error) {
		r := run(t, o)
		mu.Lock()
		*spawned = append(*spawned, r)
		mu.Unlock()
		return daemon.Spawned{PID: os.Getpid(), Exited: r.exited}, nil
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
	// A fresh state.db under fsync load has taken 5.3 s to open, and a live
	// handoff waits up to a poll for the old daemon to let go (A48): the
	// default wait outlasts both.
	t.Run("a daemon slower than 3 s to answer still counts as started", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		o.StartWait = 0 // the default
		var slow *time.Timer
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			slow = time.AfterFunc(4*time.Second, func() { run(t, o) })
			return daemon.Spawned{PID: os.Getpid()}, nil
		}
		t.Cleanup(func() { slow.Stop() })

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: os.Getpid(), Healthy: true}, res)
	})
	// The old daemon lets go of the lock only once its poll notices the
	// replaced socket, up to PollInterval after the handoff (A3, A48).
	t.Run("a live handoff whose old daemon lets go after 4 s still counts as started", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		old := opts(t, dir, h)
		old.Poll = time.Hour // it lets go below, when the test says
		oldRun := run(t, old)
		healthy(t, dir, h)
		h.restart()
		notices := time.AfterFunc(4*time.Second, oldRun.cancel)
		t.Cleanup(func() { notices.Stop() })

		o := restarted(opts(t, dir, h))
		o.StartWait = 0 // the default
		o.LockWait = ready
		var spawned []*running
		var mu sync.Mutex
		o.Hooks.Spawn = spawnInProcess(t, o, &spawned, &mu)

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: os.Getpid(), Healthy: true}, res)
		assert.False(t, oldRun.alive(), "the new server's daemon answered only once the old one let go")
	})
	t.Run("already running for this herdr: spawns nothing", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		run(t, o)
		healthy(t, dir, h)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			t.Error("Start spawned a second daemon")
			return daemon.Spawned{}, nil
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
		}, ready, 10*time.Millisecond, "the new daemon takes over once the old one lets go")
	})
	t.Run("a spawn that fails is an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			return daemon.Spawned{}, errors.New("exec: no such file")
		}

		_, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrSpawn)
	})
	t.Run("a spawned daemon that never answers is reported, not an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		o.StartWait = 200 * time.Millisecond
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) { return daemon.Spawned{PID: 4242}, nil }

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: 4242}, res)
	})
	t.Run("a start whose context ends while it waits is that error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		ctx, cancel := context.WithCancel(t.Context())
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			time.AfterFunc(100*time.Millisecond, cancel)
			return daemon.Spawned{PID: 4242}, nil
		}

		res, err := daemon.Start(ctx, o)
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, daemon.StartResult{PID: 4242}, res)
	})
	// Before A48 this waited out StartWait and read as still starting.
	t.Run("a spawned daemon that exits before it answers is an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		gone := make(chan struct{})
		close(gone)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			return daemon.Spawned{PID: 4242, Exited: gone}, nil
		}

		res, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrDaemonExited)
		assert.Equal(t, daemon.StartResult{PID: 4242}, res)
	})
	// A concurrent start's daemon took the lock first, so this start's own
	// spawn exited (ErrAlreadyRunning): the daemon to wait on is the holder.
	t.Run("a spawn that loses the lock to another start waits on the holder", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		pinged, release := holdPing(t, h)
		gone := make(chan struct{})
		close(gone)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			run(t, o)
			awaitPing(t, pinged) // it holds the lock and has not answered health
			time.AfterFunc(300*time.Millisecond, release)
			return daemon.Spawned{PID: 4242, Exited: gone}, nil
		}

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: os.Getpid(), Healthy: true}, res)
	})
	// The new daemon gave up waiting for the lock (ErrLockTimeout) while the
	// old server's daemon still holds it: a live holder, of another server.
	t.Run("a spawn that gives up on an old server's lock is an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		old := opts(t, dir, h)
		old.Poll = time.Hour
		run(t, old)
		healthy(t, dir, h)
		h.restart()
		o := opts(t, dir, h) // the old daemon reads as alive: same pid, same start
		gone := make(chan struct{})
		close(gone)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			return daemon.Spawned{PID: 4242, Exited: gone}, nil
		}

		res, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrDaemonExited)
		assert.Equal(t, daemon.StartResult{PID: 4242}, res)
	})
	t.Run("a holder still starting at StartWait is reported by its pid, not an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		o.StartWait = 300 * time.Millisecond
		pinged, _ := holdPing(t, h)
		gone := make(chan struct{})
		close(gone)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			run(t, o)
			awaitPing(t, pinged)
			return daemon.Spawned{PID: 4242, Exited: gone}, nil
		}

		res, err := daemon.Start(t.Context(), o)
		require.NoError(t, err)
		assert.Equal(t, daemon.StartResult{PID: os.Getpid()}, res)
	})
	// During `daemon restart` the old daemon has let go of the lock but may
	// still be exiting, and its record names it: it is waited on until it
	// is gone, and then the spawn that exited is what failed.
	t.Run("a spawn that exits after the old daemon lets go names the spawn", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		table := &procs{start: map[int]uint64{os.Getpid(): 77}}
		o.Hooks.StartTime = table.startTime
		old := run(t, o)
		healthy(t, dir, h)
		old.cancel()
		<-old.exited // it let go; its record names it, still alive
		gone := make(chan struct{})
		close(gone)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			time.AfterFunc(300*time.Millisecond, func() { table.set(os.Getpid(), 78) }) // the old process exits
			return daemon.Spawned{PID: 4242, Exited: gone}, nil
		}

		res, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrDaemonExited)
		assert.Equal(t, daemon.StartResult{PID: 4242}, res, "the spawn, not the old daemon")
	})
	t.Run("a daemon already starting that exits before it answers is an error", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		o := opts(t, shortDir(t), h)
		table := &procs{start: map[int]uint64{os.Getpid(): 77}}
		o.Hooks.StartTime = table.startTime
		pinged, _ := holdPing(t, h)
		holder := run(t, o)
		awaitPing(t, pinged)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			t.Error("Start spawned beside a live holder")
			return daemon.Spawned{}, nil
		}
		time.AfterFunc(300*time.Millisecond, func() {
			holder.cancel()
			<-holder.exited
			table.set(os.Getpid(), 78) // its pid is gone, or another process's
		})

		res, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrDaemonExited)
		assert.Equal(t, daemon.StartResult{PID: os.Getpid(), AlreadyRunning: true}, res)
	})
	t.Run("no herdr: an error, nothing spawned", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		h.stop()
		o := opts(t, shortDir(t), h)
		o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
			t.Error("spawned with no herdr")
			return daemon.Spawned{}, nil
		}
		_, err := daemon.Start(t.Context(), o)
		require.ErrorIs(t, err, daemon.ErrHerdrGone)
	})
}

// holdPing makes h hold every ping until release is called, so a daemon that
// has taken its lock waits there before it serves. pinged receives once a
// ping is held. The test's cleanup releases it.
func holdPing(t *testing.T, h *fakeHerdr) (pinged <-chan struct{}, release func()) {
	t.Helper()
	gate := make(chan struct{})
	once := sync.OnceFunc(func() { close(gate) })
	seen := make(chan struct{}, 1)
	h.mu.Lock()
	h.before = func(method string) {
		if method != "ping" {
			return
		}
		select {
		case seen <- struct{}{}:
		default:
		}
		<-gate
	}
	h.mu.Unlock()
	t.Cleanup(once)
	return seen, once
}

// awaitPing waits, up to ready, for a ping holdPing holds.
func awaitPing(t *testing.T, pinged <-chan struct{}) {
	t.Helper()
	select {
	case <-pinged:
	case <-time.After(ready):
		t.Error("the daemon never pinged herdr")
	}
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
	}, ready, 10*time.Millisecond, "exactly one daemon must keep running")
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
		require.NoError(t, r.wait(t, ready))
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

// TestStopChecksTheHolderThroughTheLineageReader: a holder's start time comes
// from the reader the lineage uses, so on darwin, as on Linux, a lock record
// whose pid now belongs to another process names no holder (NERD-5307). Such
// a record is what a daemon that has just taken the lock still shows before
// it writes its own. The reader here is darwin's, over a fixture kinfo_proc:
// pid 4242, forked at 1760000000.123456.
func TestStopChecksTheHolderThroughTheLineageReader(t *testing.T) {
	t.Parallel()
	start := daemon.StartTimeFrom(report.KinfoStat(func(pid int) (report.Kinfo, error) {
		if pid != 4242 {
			return report.Kinfo{}, errors.New("no such process")
		}
		return report.Kinfo{PID: 4242, PPID: 1, StartSec: 1_760_000_000, StartUsec: 123_456}, nil
	}))
	require.NotNil(t, start)
	got, err := start(4242)
	require.NoError(t, err)
	assert.Equal(t, uint64(1_760_000_000_123_456), got)
	_, err = start(4243)
	require.ErrorIs(t, err, report.ErrProcStat)

	t.Run("a pid another process now has is never signalled", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		// The holder that wrote this forked a microsecond before pid 4242's
		// present owner.
		holdLock(t, dir, h, `{"v":1,"pid":4242,"start":1760000000123455,"herdr":{"dev":1,"ino":1},"started_at":"2026-10-09T00:00:00Z"}`)
		o := opts(t, dir, h)
		o.StopWait = 200 * time.Millisecond
		o.Hooks.StartTime = start
		o.Hooks.Signal = func(int) error {
			t.Error("signalled a pid that is no longer the holder")
			return nil
		}
		require.ErrorIs(t, daemon.Stop(t.Context(), o), daemon.ErrHolderChanged)
	})
	t.Run("the holder itself is signalled", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		release := holdLock(t, dir, h, `{"v":1,"pid":4242,"start":1760000000123456,"herdr":{"dev":1,"ino":1},"started_at":"2026-10-09T00:00:00Z"}`)
		o := opts(t, dir, h)
		o.Hooks.StartTime = start
		signalled := 0
		o.Hooks.Signal = func(pid int) error { signalled = pid; release(); return nil }
		require.NoError(t, daemon.Stop(t.Context(), o))
		assert.Equal(t, 4242, signalled)
	})
	// A darwin build before NERD-5307 had no start time to record, so its
	// record has 0, and its pid alone names the holder until it restarts.
	t.Run("a record without a start time is checked by pid alone", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		release := holdLock(t, dir, h, `{"v":1,"pid":4242,"start":0,"herdr":{"dev":1,"ino":1},"started_at":"2026-10-09T00:00:00Z"}`)
		o := opts(t, dir, h)
		o.Hooks.StartTime = start
		signalled := 0
		o.Hooks.Signal = func(pid int) error { signalled = pid; release(); return nil }
		require.NoError(t, daemon.Stop(t.Context(), o))
		assert.Equal(t, 4242, signalled)
	})
}

// TestStartTimeComesFromTheLineageReader: with no hook injected, a daemon
// reads start times through report.SystemStat, the lineage's reader (/proc
// on Linux, sysctl on darwin). Only where the OS has none is a holder
// checked by pid alone.
func TestStartTimeComesFromTheLineageReader(t *testing.T) {
	t.Parallel()
	assert.Nil(t, daemon.StartTimeFrom(nil), "with no reader there is no start time")
	stat := report.SystemStat()
	def := daemon.DefaultStartTime()
	if stat == nil {
		assert.Nil(t, def)
		return
	}
	require.NotNil(t, def)
	want, err := stat(os.Getpid())
	require.NoError(t, err)
	got, err := def(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, want.Start, got)
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
		"health":                 {line: `{"v":1,"op":"health","args":{}}` + "\n"},
		"health without args":    {line: `{"v":1,"op":"health"}` + "\n"},
		"no trailing newline":    {line: `{"v":1,"op":"health"}`},
		"CRLF line ending":       {line: `{"v":1,"op":"health"}` + "\r\n"},
		"unknown op":             {line: `{"v":1,"op":"teleport"}` + "\n", code: "unknown_op"},
		"snapshot, no team poll": {line: `{"v":1,"op":"snapshot"}` + "\n", code: "not_ready"},
		// The args are checked before the cache is: a malformed selector is
		// the client's mistake, whatever the daemon holds (A24).
		"snapshot args not an object":   {line: `{"v":1,"op":"snapshot","args":[1]}` + "\n", code: "bad_request"},
		"snapshot project not a string": {line: `{"v":1,"op":"snapshot","args":{"project":7}}` + "\n", code: "bad_request"},
		"snapshot workspace not a string": {
			line: `{"v":1,"op":"snapshot","args":{"workspace_id":true}}` + "\n", code: "bad_request",
		},
		"snapshot args null":             {line: `{"v":1,"op":"snapshot","args":null}` + "\n", code: "not_ready"},
		"snapshot args it does not know": {line: `{"v":1,"op":"snapshot","args":{"team":"x"}}` + "\n", code: "not_ready"},
		"snapshot in another case":       {line: `{"v":1,"op":"Snapshot"}` + "\n", code: "unknown_op"},
		"op in another case":             {line: `{"v":1,"op":"HEALTH"}` + "\n", code: "unknown_op"},
		"missing version":                {line: `{"op":"health"}` + "\n", code: "unsupported_version"},
		"newer version":                  {line: `{"v":2,"op":"health"}` + "\n", code: "unsupported_version"},
		"not JSON":                       {line: "health\n", code: "bad_request"},
		"empty line":                     {line: "\n", code: "bad_request"},
		"two objects":                    {line: `{"v":1,"op":"health"}{"v":1,"op":"health"}` + "\n", code: "bad_request"},
		"invalid UTF-8 in the op":        {line: "{\"v\":1,\"op\":\"he\xffalth\"}\n", code: "unknown_op"},
		"over the request size cap":      {line: `{"v":1,"op":"health","args":"` + strings.Repeat("x", 1<<20) + `"}` + "\n", code: "bad_request"},
		"op is not a string":             {line: `{"v":1,"op":7}` + "\n", code: "bad_request"},
		"back with no history":           {line: `{"v":1,"op":"back","args":{}}` + "\n", code: "no_history"},
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
			sock := tc.sock(t)
			_, err := daemon.Health(t.Context(), sock)
			require.ErrorIs(t, err, tc.want)
			_, err = daemon.Back(t.Context(), sock)
			require.ErrorIs(t, err, tc.want, "back fails the same way")
		})
	}

	t.Run("a back answer naming no pane", func(t *testing.T) {
		t.Parallel()
		for _, reply := range []string{`{"ok":true,"data":{"from":"w1:p1"}}`, `{"ok":true,"data":{"to":7}}`} {
			_, err := daemon.Back(t.Context(), serve(t, reply+"\n"))
			require.ErrorIs(t, err, daemon.ErrProtocol, reply)
		}
	})

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
	o.Hooks.Spawn = func(context.Context) (daemon.Spawned, error) {
		t.Error("spawned into an unwritable state dir")
		return daemon.Spawned{}, nil
	}

	require.ErrorIs(t, daemon.Run(t.Context(), o), os.ErrPermission)
	_, err := daemon.Start(t.Context(), o)
	require.ErrorIs(t, err, os.ErrPermission)
}

// swapIn removes h's socket, keeping its server serving the connections it
// has, and starts a replacement at the same path answering the same layout:
// herdr's live handoff, with the old server not yet gone.
func swapIn(t *testing.T, h *fakeHerdr) *herdrtest.Server {
	t.Helper()
	require.NoError(t, os.Remove(h.path))
	return herdrtest.StartAt(t, h.path, h.serve(22))
}

// TestRunPollPingNeverReachesAReplacement is A3 for Run's poll: the ping that
// checks the server goes through the daemon's guarded client, so once the
// socket names a replacement the ping is refused before anything is sent, and
// the daemon ends at that poll.
func TestRunPollPingNeverReachesAReplacement(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	dir := shortDir(t)
	r := run(t, opts(t, dir, h))
	healthy(t, dir, h)

	replacement := swapIn(t, h)
	require.ErrorIs(t, r.wait(t, ready), daemon.ErrHerdrGone)
	replacement.Settle(t)
	assert.Empty(t, replacement.Requests(), "the replacement, which the next daemon belongs to, is asked nothing")
}

// TestRunStartupPingNeverReachesAReplacement: a daemon that waited for the
// lock through a live handoff finds its server replaced when it gets the
// lock: its first ping is refused before it is sent, and it never runs.
func TestRunStartupPingNeverReachesAReplacement(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	dir := shortDir(t)
	release := holdLock(t, dir, h, staleHolder)
	o := opts(t, dir, h)
	o.LockWait = ready
	r := run(t, o)
	time.Sleep(150 * time.Millisecond)
	require.True(t, r.alive(), "the daemon waits for the stale holder")

	replacement := swapIn(t, h)
	release()
	require.ErrorIs(t, r.wait(t, ready), daemon.ErrHerdrGone)
	replacement.Settle(t)
	assert.Empty(t, replacement.Requests())
	assert.Empty(t, h.srv.Requests(), "nor is the original, whose socket is gone")
}

// TestRunRecordsItsStartTimeInTheLock: a running daemon writes its own start
// time, read through the StartTime hook (77 for this process in opts), into
// daemon.lock, so a later stop or start can tell it from a process that
// reused its pid. The record is decoded by the lock file's field names.
func TestRunRecordsItsStartTimeInTheLock(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	dir := shortDir(t)
	run(t, opts(t, dir, h))
	healthy(t, dir, h)
	raw, err := os.ReadFile(paths(t, dir, h).Lock)
	require.NoError(t, err)
	var rec struct {
		PID   int    `json:"pid"`
		Start uint64 `json:"start"`
	}
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, os.Getpid(), rec.PID)
	assert.Equal(t, uint64(77), rec.Start, "the record carries the hook's start time")
}

// TestStillServedClasses is Run's poll check (A3), through the daemon's own
// guarded client: it pings first and checks the server's identity after
// every ping. A replaced, removed or dead socket, or a protocol change, is
// ErrHerdrGone; a ping that fails any other way, such as a busy herdr, is not.
func TestStillServedClasses(t *testing.T) {
	t.Parallel()
	answer := func(protocol uint32, busy bool) herdrtest.Handler {
		return func(r herdrtest.Request) herdrtest.Reply {
			if busy {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
			}
			return pong(protocol)(r)
		}
	}
	tests := map[string]struct {
		// setup returns the socket; ident is the socket's identity first.
		setup func(t *testing.T, sock string) *herdrtest.Server
		gone  bool
		quiet bool // the ping times out
	}{
		"its own server": {setup: func(t *testing.T, sock string) *herdrtest.Server {
			return herdrtest.StartAt(t, sock, answer(22, false))
		}},
		"its own server, busy": {setup: func(t *testing.T, sock string) *herdrtest.Server {
			return herdrtest.StartAt(t, sock, answer(22, true))
		}},
		"its own server, not answering in time": {quiet: true, setup: func(t *testing.T, sock string) *herdrtest.Server {
			return herdrtest.StartAt(t, sock, func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} })
		}},
		"its own server, now on another protocol": {gone: true, setup: func(t *testing.T, sock string) *herdrtest.Server {
			return herdrtest.StartAt(t, sock, answer(23, false))
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sock := filepath.Join(shortDir(t), "h.sock")
			srv := tc.setup(t, sock)
			ctx := t.Context()
			if tc.quiet {
				c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
				ctx = c
			}
			err := daemon.StillServedOver(ctx, sock, mustIdentity(t, sock), 22)
			if tc.gone {
				require.ErrorIs(t, err, daemon.ErrHerdrGone)
			} else {
				require.NoError(t, err)
			}
			assert.Len(t, srv.Requests(), 1, "one ping")
		})
	}

	// The socket is no longer the server the daemon belongs to.
	for name, swap := range map[string]func(t *testing.T, sock string, srv *herdrtest.Server) *herdrtest.Server{
		"replaced": func(t *testing.T, sock string, _ *herdrtest.Server) *herdrtest.Server {
			require.NoError(t, os.Remove(sock))
			return herdrtest.StartAt(t, sock, answer(22, false))
		},
		"removed": func(t *testing.T, sock string, _ *herdrtest.Server) *herdrtest.Server {
			require.NoError(t, os.Remove(sock))
			return nil
		},
		"crashed, its socket file left behind": func(_ *testing.T, _ string, srv *herdrtest.Server) *herdrtest.Server {
			srv.Crash()
			return nil
		},
		"behind a parent it cannot search": func(t *testing.T, sock string, _ *herdrtest.Server) *herdrtest.Server {
			dir := filepath.Dir(sock)
			require.NoError(t, os.Chmod(dir, 0o600))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(name, "search") && os.Geteuid() == 0 {
				t.Skip("root searches any directory")
			}
			sock := filepath.Join(shortDir(t), "h.sock")
			srv := herdrtest.StartAt(t, sock, answer(22, false))
			ident := mustIdentity(t, sock)
			replacement := swap(t, sock, srv)
			err := daemon.StillServedOver(t.Context(), sock, ident, 22)
			require.ErrorIs(t, err, daemon.ErrHerdrGone)
			if replacement != nil {
				replacement.Settle(t)
				assert.Empty(t, replacement.Requests(), "the replacement is asked nothing")
			}
		})
	}
}

// TestStillServedChecksTheServerAfterThePing: the socket is replaced while
// the original answers the poll's ping. The identity check after the ping
// must end the daemon at this poll, not the next one.
func TestStillServedChecksTheServerAfterThePing(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortDir(t), "h.sock")
	swapped := make(chan net.Listener, 1)
	var once sync.Once
	srv := herdrtest.StartAt(t, sock, func(r herdrtest.Request) herdrtest.Reply {
		if r.Method == "ping" {
			once.Do(func() {
				if err := os.Remove(sock); err != nil {
					t.Error(err)
				}
				l, err := net.Listen("unix", sock)
				if err != nil {
					t.Error(err)
					return
				}
				swapped <- l
			})
		}
		return pong(22)(r)
	})
	ident := mustIdentity(t, sock)
	err := daemon.StillServedOver(t.Context(), sock, ident, 22)
	var replacement net.Listener
	select {
	case replacement = <-swapped:
	default:
		t.Fatal("no swap happened during the ping")
	}
	t.Cleanup(func() { _ = replacement.Close() })
	assert.Len(t, srv.Requests(), 1, "the ping reached the original")
	require.ErrorIs(t, err, daemon.ErrHerdrGone, "a swap during the ping ends the daemon at this poll")
}

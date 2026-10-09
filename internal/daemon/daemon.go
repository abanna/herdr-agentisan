package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// VerifiedProtocols are the herdr API protocol versions this build has been
// verified against (D2). herdr 0.9.3 speaks protocol 22.
var VerifiedProtocols = []uint32{22}

// AllowUnverifiedEnv names the variable that lets the daemon run against an
// unverified protocol after a herdr update, until the pin is bumped (A4).
const AllowUnverifiedEnv = "HERDR_AGENTISAN_ALLOW_UNVERIFIED"

// Defaults for the Options durations left at zero.
const (
	// PollInterval is how often the daemon checks its herdr server (D3).
	PollInterval = 3 * time.Second
	// DefaultLockWait is how long a new daemon waits for a stale holder (A3).
	DefaultLockWait = 15 * time.Second
	// DefaultStartWait is how long Start waits for the new daemon's health.
	DefaultStartWait = 3 * time.Second
	// DefaultStopWait is how long Stop waits for the daemon to let go.
	DefaultStopWait = 10 * time.Second
)

// recheck is how often a waiting caller retries the lock.
const recheck = 50 * time.Millisecond

// More sentinel errors.
var (
	// ErrAlreadyRunning means a daemon of the same herdr server holds the
	// lock. Starting one is then done, not failed.
	ErrAlreadyRunning = errors.New("a daemon is already running for this herdr server")
	// ErrLockTimeout means the lock stayed held, by a daemon of another (or
	// an unknown) herdr server, for longer than the lock wait.
	ErrLockTimeout = errors.New("timed out waiting for the daemon lock")
	// ErrUnverifiedProtocol means herdr speaks a protocol version this build
	// has not been verified against (D2).
	ErrUnverifiedProtocol = errors.New("unverified herdr protocol")
	// ErrNotRunning means no daemon holds the lock.
	ErrNotRunning = errors.New("no daemon is running")
	// ErrHolderChanged means the lock record does not name a live holder, so
	// there is no pid that is safe to signal.
	ErrHolderChanged = errors.New("the daemon lock record does not name its holder")
	// ErrStopTimeout means the daemon was signalled but kept the lock.
	ErrStopTimeout = errors.New("timed out waiting for the daemon to stop")
	// ErrSpawn means the daemon process could not be started.
	ErrSpawn = errors.New("could not start the daemon")
)

// Pinger is the slice of the herdr client the daemon needs.
type Pinger interface {
	Ping(ctx context.Context) (herdr.Pong, error)
}

// Hooks are the process operations, injected so tests can run daemons in
// process. A nil hook uses the real operation.
type Hooks struct {
	// Spawn starts a detached `daemon run` and returns its pid (ExecSpawn).
	Spawn func(ctx context.Context) (int, error)
	// Signal asks the daemon with pid to stop (SIGTERM).
	Signal func(pid int) error
	// StartTime returns a process's start time (/proc on Linux). With none,
	// a holder is checked by pid alone.
	StartTime func(pid int) (uint64, error)
}

// Options configure one daemon and the operations on it.
type Options struct {
	// StateDir is the plugin state directory (HERDR_PLUGIN_STATE_DIR).
	StateDir string
	// HerdrSocket is the herdr server's socket (HERDR_SOCKET_PATH).
	HerdrSocket string
	// Herdr is the client for that socket.
	Herdr Pinger
	// LockWait bounds the wait for a stale holder; zero is DefaultLockWait.
	LockWait time.Duration
	// Poll is how often the daemon checks its herdr server; zero is PollInterval.
	Poll time.Duration
	// StartWait bounds Start's wait for health; zero is DefaultStartWait.
	StartWait time.Duration
	// StopWait bounds Stop's wait for the lock; zero is DefaultStopWait.
	StopWait time.Duration
	// AllowUnverified runs against an unverified protocol (A4).
	AllowUnverified bool
	// Version and Commit are reported by health.
	Version, Commit string
	Logger          zerolog.Logger
	Hooks           Hooks
}

func (o Options) withDefaults() Options {
	if o.LockWait <= 0 {
		o.LockWait = DefaultLockWait
	}
	if o.Poll <= 0 {
		o.Poll = PollInterval
	}
	if o.StartWait <= 0 {
		o.StartWait = DefaultStartWait
	}
	if o.StopWait <= 0 {
		o.StopWait = DefaultStopWait
	}
	if o.Hooks.Signal == nil {
		o.Hooks.Signal = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
	}
	if o.Hooks.StartTime == nil && runtime.GOOS == "linux" {
		stat := report.ProcStat(os.DirFS("/proc"))
		o.Hooks.StartTime = func(pid int) (uint64, error) {
			s, err := stat(pid)
			return s.Start, err
		}
	}
	return o
}

// selfStart is this process's start time, or 0 where it cannot be read.
func (o Options) selfStart() uint64 {
	if o.Hooks.StartTime == nil {
		return 0
	}
	s, err := o.Hooks.StartTime(os.Getpid())
	if err != nil {
		return 0
	}
	return s
}

// alive reports whether the record's holder is still the process that wrote
// it: same pid, and the same start time where start times can be read.
func (o Options) alive(info LockInfo) bool {
	if !info.valid() {
		return false
	}
	if o.Hooks.StartTime == nil {
		err := syscall.Kill(info.PID, 0)
		return err == nil || errors.Is(err, syscall.EPERM)
	}
	s, err := o.Hooks.StartTime(info.PID)
	return err == nil && (info.Start == 0 || s == info.Start)
}

// liveHolder reports whether a live daemon holds the lock, and its record.
func (o Options) liveHolder(path string) (LockInfo, bool, error) {
	isHeld, err := held(path)
	if err != nil || !isHeld {
		return LockInfo{}, false, err
	}
	info := readInfo(path)
	return info, o.alive(info), nil
}

// Run is the daemon: it takes the lock, checks the herdr protocol, serves its
// socket and returns when ctx ends (nil) or its herdr server goes
// (ErrHerdrGone). A daemon of the same server already holding the lock is
// ErrAlreadyRunning. One bound to a different server is waited for, up to
// LockWait, because during a live handoff the new server's startup hook can
// run before the old daemon notices its server has gone (A3).
func Run(ctx context.Context, o Options) error {
	o = o.withDefaults()
	paths, err := PathsFor(o.StateDir, o.HerdrSocket)
	if err != nil {
		return err
	}
	ident, err := HerdrIdentity(o.HerdrSocket)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", paths.Dir, err)
	}
	f, err := openLock(paths.Lock)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // closing releases the lock; the record stays for the next holder to replace
	if err := o.acquire(ctx, f, paths.Lock, ident); err != nil {
		return err
	}
	startedAt := time.Now().UTC()
	if err := writeInfo(f, LockInfo{V: lockVersion, PID: os.Getpid(), Start: o.selfStart(), Herdr: ident, StartedAt: startedAt}); err != nil {
		return err
	}

	pong, err := o.Herdr.Ping(ctx)
	if err != nil {
		return fmt.Errorf("ping herdr: %w", err)
	}
	if !slices.Contains(VerifiedProtocols, pong.Protocol) && !o.AllowUnverified {
		return fmt.Errorf("%w: herdr %s speaks protocol %d; this build is verified against %v. Set %s=1 to run anyway",
			ErrUnverifiedProtocol, pong.Version, pong.Protocol, VerifiedProtocols, AllowUnverifiedEnv)
	}

	srv, err := listen(paths.Socket, HealthInfo{
		PID: os.Getpid(), Version: o.Version, Commit: o.Commit, StartedAt: startedAt,
		HerdrProtocol: pong.Protocol, HerdrSocket: o.HerdrSocket, Herdr: ident,
	}, o.Logger)
	if err != nil {
		return err
	}
	defer srv.close()
	o.Logger.Info().Int("pid", os.Getpid()).Uint32("protocol", pong.Protocol).Str("socket", paths.Socket).Msg("daemon running")

	tick := time.NewTicker(o.Poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			o.Logger.Info().Msg("daemon stopping")
			return nil
		case <-tick.C:
			if err := o.stillServed(ctx, ident, pong.Protocol); err != nil {
				o.Logger.Info().Err(err).Msg("herdr server gone; daemon exiting")
				return err
			}
		}
	}
}

// stillServed reports ErrHerdrGone unless the daemon's herdr server is still
// there: the same socket (identity), answering (a refused connection means it
// died and left its socket file behind), on the protocol it was pinned at. A
// ping that fails any other way, such as a busy herdr timing out, is not
// taken as the server going.
func (o Options) stillServed(ctx context.Context, ident Identity, protocol uint32) error {
	now, err := HerdrIdentity(o.HerdrSocket)
	if err != nil || now != ident {
		return fmt.Errorf("%w: the socket the daemon belongs to was removed or replaced", ErrHerdrGone)
	}
	pong, err := o.Herdr.Ping(ctx)
	switch {
	case errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT):
		return fmt.Errorf("%w: nothing answers its socket: %w", ErrHerdrGone, err)
	case err != nil:
		o.Logger.Debug().Err(err).Msg("herdr ping failed; still treating the server as up")
	case pong.Protocol != protocol:
		return fmt.Errorf("%w: the server now speaks protocol %d, not %d", ErrHerdrGone, pong.Protocol, protocol)
	}
	return nil
}

// acquire takes the lock, or reports why not.
func (o Options) acquire(ctx context.Context, f *os.File, path string, ident Identity) error {
	deadline := time.Now().Add(o.LockWait)
	for {
		err := tryLock(f)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errLocked) {
			return err
		}
		if info := readInfo(path); info.Herdr == ident && o.alive(info) {
			return fmt.Errorf("%w (pid %d)", ErrAlreadyRunning, info.PID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %v: held by %+v", ErrLockTimeout, o.LockWait, readInfo(path))
		}
		if err := pause(ctx); err != nil {
			return err
		}
	}
}

// StartResult reports what Start found or did.
type StartResult struct {
	// PID is the daemon's pid, or the spawned process's when it has not
	// answered health yet.
	PID int
	// AlreadyRunning means a daemon of this herdr server was already running.
	AlreadyRunning bool
	// Healthy means the daemon of this herdr server answered health.
	Healthy bool
}

// Start makes sure a daemon runs for the herdr server: it spawns `daemon run`
// unless a live daemon of this server holds the lock, then waits up to
// StartWait for the daemon to answer health. It never takes the lock itself,
// so the daemon it spawns cannot end up waiting on its parent.
func Start(ctx context.Context, o Options) (StartResult, error) {
	o = o.withDefaults()
	paths, err := PathsFor(o.StateDir, o.HerdrSocket)
	if err != nil {
		return StartResult{}, err
	}
	ident, err := HerdrIdentity(o.HerdrSocket)
	if err != nil {
		return StartResult{}, err
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return StartResult{}, fmt.Errorf("create %s: %w", paths.Dir, err)
	}
	info, live, err := o.liveHolder(paths.Lock)
	if err != nil {
		return StartResult{}, err
	}
	if live && info.Herdr == ident {
		// It may have taken the lock a moment ago and not be listening yet.
		res := o.awaitHealth(ctx, paths.Socket, ident, info.PID)
		res.AlreadyRunning = true
		return res, nil
	}
	if o.Hooks.Spawn == nil {
		return StartResult{}, fmt.Errorf("%w: no spawner configured", ErrSpawn)
	}
	pid, err := o.Hooks.Spawn(ctx)
	if err != nil {
		return StartResult{}, fmt.Errorf("%w: %w", ErrSpawn, err)
	}
	return o.awaitHealth(ctx, paths.Socket, ident, pid), nil
}

// awaitHealth waits up to StartWait for the daemon of the herdr server ident
// to answer health. A stale daemon still serving answers too, naming its own
// server; only this server's counts. Without an answer it reports pid
// unhealthy.
func (o Options) awaitHealth(ctx context.Context, socket string, ident Identity, pid int) StartResult {
	deadline := time.Now().Add(o.StartWait)
	for {
		if h, err := Health(ctx, socket); err == nil && h.Herdr == ident {
			return StartResult{PID: h.PID, Healthy: true}
		}
		if time.Now().After(deadline) || pause(ctx) != nil {
			return StartResult{PID: pid}
		}
	}
}

// Stop signals the daemon holding the lock and waits up to StopWait for it to
// let go. It signals only a pid the lock record names while the lock is held
// and, where start times can be read, only if that pid's start time is the
// one recorded: a pid that has since been reused is never signalled. Health
// is not consulted, so a wedged daemon is still stopped.
func Stop(ctx context.Context, o Options) error {
	o = o.withDefaults()
	paths, err := PathsFor(o.StateDir, o.HerdrSocket)
	if err != nil {
		return err
	}
	isHeld, err := held(paths.Lock)
	if err != nil {
		return err
	}
	if !isHeld {
		return ErrNotRunning
	}
	// A holder that has just taken the lock may not have written its record
	// yet; give it a moment.
	var info LockInfo
	settle := time.Now().Add(min(time.Second, o.StopWait))
	for info = readInfo(paths.Lock); !o.alive(info) && time.Now().Before(settle); info = readInfo(paths.Lock) {
		if err := pause(ctx); err != nil {
			return err
		}
	}
	if !o.alive(info) {
		return fmt.Errorf("%w: %+v", ErrHolderChanged, info)
	}
	if err := o.Hooks.Signal(info.PID); err != nil {
		return fmt.Errorf("signal daemon %d: %w", info.PID, err)
	}
	deadline := time.Now().Add(o.StopWait)
	for {
		isHeld, err := held(paths.Lock)
		if err != nil {
			return err
		}
		if !isHeld {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: pid %d still holds the lock after %v", ErrStopTimeout, info.PID, o.StopWait)
		}
		if err := pause(ctx); err != nil {
			return err
		}
	}
}

// pause waits one recheck interval or until ctx ends.
func pause(ctx context.Context) error {
	t := time.NewTimer(recheck)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("daemon: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

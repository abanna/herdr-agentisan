package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/preflight"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
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
	// DefaultStartWait is the longest Start waits for the daemon's health
	// (A48): a poll for an old daemon to notice a live handoff and let go of
	// the lock (A3), then the new daemon's own bounds before it serves, its
	// ping (herdr.CallTimeout) and its store's waits (store.BusyTimeout).
	// Start returns as soon as the daemon answers or exits, so only a daemon
	// still starting waits it out.
	DefaultStartWait = PollInterval + herdr.CallTimeout + store.BusyTimeout
	// DefaultStopWait is how long Stop waits for the daemon to let go.
	DefaultStopWait = 10 * time.Second
	// PruneInterval is how often, by Options.Now, the daemon prunes finished
	// handoffs and messages past retention (A1), and the GitHub notices' rows
	// past theirs (A45). It also prunes once when it starts.
	PruneInterval = 24 * time.Hour
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
	// ErrDaemonExited means the daemon Start was waiting on exited before it
	// answered health, and no other daemon of the herdr server holds the
	// lock: nothing is running (A48).
	ErrDaemonExited = errors.New("the daemon exited before it answered health")
)

// HerdrClient is the slice of the herdr client the daemon needs.
type HerdrClient interface {
	Ping(ctx context.Context) (herdr.Pong, error)
	ListPanes(ctx context.Context) ([]herdr.PaneInfo, error)
	Subscribe(ctx context.Context, types ...string) (*herdr.Subscription, error)
	UnzoomPane(ctx context.Context, paneID string) (herdr.PaneZoom, error)
	FocusPane(ctx context.Context, paneID string) (herdr.PaneInfo, error)
}

// Hooks are the process operations, injected so tests can run daemons in
// process. A nil hook uses the real operation.
type Hooks struct {
	// Spawn starts a detached `daemon run` (ExecSpawn).
	Spawn func(ctx context.Context) (Spawned, error)
	// Signal asks the daemon with pid to stop (SIGTERM).
	Signal func(pid int) error
	// StartTime returns a process's start time, by default through
	// report.SystemStat (/proc on Linux, sysctl on darwin). With none, a
	// holder is checked by pid alone.
	StartTime func(pid int) (uint64, error)
	// Preflight is the check a spawn's repo must pass before any pane opens
	// (A34); nil means preflight.HostCheck.
	Preflight preflight.Check
	// StartedAt puts a start StartTime returned on the wall clock, by
	// default through report.SystemStartedAt (/proc/stat's boot time on
	// Linux; darwin's start already is one). With none, the boss shows no
	// runtime (A47).
	StartedAt func(start uint64) (time.Time, error)
}

// Spawned is the daemon process a Spawn hook started.
type Spawned struct {
	PID int
	// Exited is closed once the process has exited. Nil means the hook cannot
	// tell, and Start waits out StartWait for a daemon that never answers.
	Exited <-chan struct{}
}

// Options configure one daemon and the operations on it.
type Options struct {
	// StateDir is the plugin state directory (HERDR_PLUGIN_STATE_DIR).
	StateDir string
	// HerdrSocket is the herdr server's socket (HERDR_SOCKET_PATH).
	HerdrSocket string
	// Herdr is the client for that socket. Run dials through a client of
	// its own on HerdrSocket, guarded against a replaced server (forServer),
	// and keeps only this one's Timeout.
	Herdr HerdrClient
	// LockWait bounds the wait for a stale holder; zero is DefaultLockWait.
	LockWait time.Duration
	// Poll is how often the daemon checks its herdr server; zero is PollInterval.
	Poll time.Duration
	// StartWait bounds Start's wait for health; zero is DefaultStartWait.
	StartWait time.Duration
	// StopWait bounds Stop's wait for the lock; zero is DefaultStopWait.
	StopWait time.Duration
	// BackBudget bounds one back op; zero is DefaultBackBudget.
	BackBudget time.Duration
	// AllowUnverified runs against an unverified protocol (A4).
	AllowUnverified bool
	// Version and Commit are reported by health.
	Version, Commit string
	// Now is the clock focus times and handoff pruning read; nil is time.Now.
	Now    func() time.Time
	Logger zerolog.Logger
	Hooks  Hooks
	// Team turns on the team poll and the $team push; nil leaves it off.
	Team *TeamOptions

	// closeStore closes the state database when Run exits; nil is
	// (*store.Store).Close.
	closeStore func(*store.Store) error
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
	if o.BackBudget <= 0 {
		o.BackBudget = DefaultBackBudget
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Hooks.Signal == nil {
		o.Hooks.Signal = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
	}
	if o.Hooks.StartTime == nil {
		o.Hooks.StartTime = startTimeFrom(report.SystemStat())
	}
	if o.Hooks.StartedAt == nil {
		o.Hooks.StartedAt = report.SystemStartedAt()
	}
	if o.closeStore == nil {
		o.closeStore = (*store.Store).Close
	}
	return o
}

// startTimeFrom is the StartTime hook over stat, the reader the lineage uses,
// so a holder is checked as a lineage checks its processes. It is nil when
// stat is, on an OS with no reader.
func startTimeFrom(stat report.StatFunc) func(pid int) (uint64, error) {
	if stat == nil {
		return nil
	}
	return func(pid int) (uint64, error) {
		s, err := stat(pid)
		return s.Start, err
	}
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

// Run is the daemon: it takes the lock, checks the herdr protocol, opens the
// state database, serves its socket, follows focus events and returns when
// ctx ends (nil) or its herdr server goes (ErrHerdrGone). A daemon of the same
// server already holding the lock is ErrAlreadyRunning. One bound to a
// different server is waited for, up to LockWait, because during a live
// handoff the new server's startup hook can run before the old daemon notices
// its server has gone (A3). A state database it cannot open, or one a newer
// build has migrated (store.ErrSchemaTooNew), stops it before it serves.
// Once the state database is open, Run closes it last whichever way it
// exits, and a close that fails is returned too, joined with its own result.
func Run(ctx context.Context, o Options) (err error) {
	o = o.withDefaults()
	paths, err := PathsFor(o.StateDir, o.HerdrSocket)
	if err != nil {
		return err
	}
	ident, err := HerdrIdentity(o.HerdrSocket)
	if err != nil {
		return err
	}
	o = o.forServer(ident)
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

	st, err := store.Open(ctx, paths.DB)
	if err != nil {
		return fmt.Errorf("daemon state: %w", err)
	}
	// Deferred first, so it runs last: after focus collection has stopped
	// and the socket's handlers have finished, nothing uses the store.
	defer func() {
		if cerr := o.closeStore(st); cerr != nil {
			err = errors.Join(err, fmt.Errorf("daemon state: %w", cerr))
		}
	}()
	o.recoverDispatching(ctx, st)

	w := newWalker() // shared: the collector tells it where focus goes
	snapshots := &snapshotCache{}
	idle := &team.IdleClock{} // shared: the poll times rests, team status reads them
	ready := &atomic.Bool{}   // shared: the poll's first reconcile opens the workforce ops (A35)
	in := newInbox()          // shared: the poll feeds the dispatcher, and send wakes it
	// Shared: the notices ops set them, and the poll's notices obey them (A44).
	mutes := notice.NewMutes()
	var ops map[string]op
	if svc := o.newWorkforce(st, ident, idle); svc != nil {
		ops = workforceOps(svc, ready, o.queued(in))
		maps.Copy(ops, o.noticeOps(in, mutes))
	}
	opsCtx, stopOps := context.WithCancel(ctx)
	srv, err := listen(opsCtx, paths.Socket, o.health(ctx, st, HealthInfo{
		PID: os.Getpid(), Version: o.Version, Commit: o.Commit, StartedAt: startedAt,
		HerdrProtocol: pong.Protocol, HerdrSocket: o.HerdrSocket, Herdr: ident,
	}), func() (BackResult, error) { return o.back(ctx, st, w) }, snapshots.load, ops, o.Logger)
	if err != nil {
		stopOps()
		return err
	}
	defer srv.close()
	// Deferred after the close, so it runs first: an op waiting out a
	// handoff ends now rather than holding the close.
	defer stopOps()

	focusCtx, stopFocus := context.WithCancel(ctx)
	var focus sync.WaitGroup
	views := newViewSync() // the collector tells it focus, the team poll the teams
	focus.Go(func() { o.followFocus(focusCtx, st, w, views, ident) })
	defer func() {
		stopFocus()
		focus.Wait()
	}()
	stopTeam := o.startTeam(ctx, st, ident, snapshots, idle, views, ready, in, mutes)
	defer stopTeam()
	o.Logger.Info().Int("pid", os.Getpid()).Uint32("protocol", pong.Protocol).Str("socket", paths.Socket).
		Int("store_version", st.UserVersion()).Msg("daemon running")

	nextPrune := o.prune(ctx, st)
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
			if !o.Now().Before(nextPrune) {
				nextPrune = o.prune(ctx, st)
			}
		}
	}
}

// health returns the health op's answer: base, plus the state database's
// schema version and focus rows read when asked.
func (o Options) health(ctx context.Context, st *store.Store, base HealthInfo) func() HealthInfo {
	return func() HealthInfo {
		h := base
		h.StoreVersion = st.UserVersion()
		ctx, cancel := context.WithTimeout(ctx, ioTimeout)
		defer cancel()
		n, err := st.FocusRows(ctx)
		if err != nil {
			o.Logger.Debug().Err(err).Msg("health: count focus rows")
			n = -1
		}
		h.FocusRows = n
		return h
	}
}

// prune deletes the finished handoffs and messages past retention (A1), and
// the GitHub notices' rows past theirs (A45), and returns when the next
// prune is due. A failure is logged and retried at the
// next one.
func (o Options) prune(ctx context.Context, st *store.Store) time.Time {
	now := o.Now()
	for _, p := range []struct {
		what  string
		prune func(context.Context, time.Time) (int64, error)
	}{{"finished handoffs", st.PruneHandoffs}, {"finished messages", st.PruneMessages}, {"GitHub notice rows not observed for 30 days", st.PruneGitHub}} {
		n, err := p.prune(ctx, now)
		switch {
		case err != nil:
			o.Logger.Warn().Err(err).Msg("prune " + p.what)
		case n > 0:
			o.Logger.Info().Int64("deleted", n).Msg("pruned " + p.what)
		}
	}
	return now.Add(PruneInterval)
}

// stillServed reports ErrHerdrGone unless the daemon's herdr server is still
// there: the same socket (identity), answering (a refused connection means it
// died and left its socket file behind), on the protocol it was pinned at. A
// ping that fails any other way, such as a busy herdr timing out, is not
// taken as the server going.
//
// It pings first, through the daemon's guarded client, which checks the
// identity once connected and sends nothing to a replacement (A3), and checks
// the identity again after every ping, whatever it answered.
func (o Options) stillServed(ctx context.Context, ident Identity, protocol uint32) error {
	pong, err := o.Herdr.Ping(ctx)
	switch {
	case !o.sameServer(ident):
		return fmt.Errorf("%w: the socket the daemon belongs to was removed or replaced", ErrHerdrGone)
	case errors.Is(err, ErrHerdrGone):
		return fmt.Errorf("poll check: %w", err)
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
	// PID is the daemon's pid, or, when it has not answered health yet, the
	// pid of the daemon holding the lock or else of the spawned process.
	PID int
	// AlreadyRunning means a daemon of this herdr server was already running.
	AlreadyRunning bool
	// Healthy means the daemon of this herdr server answered health. Without
	// an error, false means it is still starting after StartWait.
	Healthy bool
}

// Start makes sure a daemon runs for the herdr server: it spawns `daemon run`
// unless a live daemon of this server holds the lock, then waits for the
// daemon to answer health, for up to StartWait (A48). A daemon that exits
// before it answers, with no other daemon of this server holding the lock,
// is ErrDaemonExited. Start never takes the lock itself, so the daemon it
// spawns cannot end up waiting on its parent.
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
		// No process of Start's own is coming, so only the holder counts.
		res, err := o.awaitHealth(ctx, paths, ident, Spawned{PID: info.PID, Exited: closed})
		res.AlreadyRunning = true
		return res, err
	}
	if o.Hooks.Spawn == nil {
		return StartResult{}, fmt.Errorf("%w: no spawner configured", ErrSpawn)
	}
	spawned, err := o.Hooks.Spawn(ctx)
	if err != nil {
		return StartResult{}, fmt.Errorf("%w: %w", ErrSpawn, err)
	}
	return o.awaitHealth(ctx, paths, ident, spawned)
}

// closed is a channel that is already closed.
var closed = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// awaitHealth waits up to StartWait for the daemon of the herdr server ident
// to answer health (A48). A stale daemon still serving answers too, naming
// its own server; only this server's counts. While the lock record names a
// live daemon of this server, that daemon is starting, whatever process it
// is (a concurrent start's spawn, say), and awaitHealth waits on it.
// Otherwise it waits on p, which may still be waiting for the lock, as during
// a live handoff, until p exits: that is ErrDaemonExited. Without an answer
// by StartWait it reports the daemon unhealthy, still starting; a ctx that
// ends first is its error. The pid it reports is the live holder's, or else
// p's.
//
// It reads the record without probing the lock: a probe takes a free lock
// for an instant, and a daemon trying it then would read the record of one
// that has let go but not yet exited as a live holder, and give up.
func (o Options) awaitHealth(ctx context.Context, paths Paths, ident Identity, p Spawned) (StartResult, error) {
	deadline := time.Now().Add(o.StartWait)
	var pid int
	for {
		if h, err := Health(ctx, paths.Socket); err == nil && h.Herdr == ident {
			return StartResult{PID: h.PID, Healthy: true}, nil
		}
		// The exit is read before the record: a spawn that exits as
		// already running read a live holder's record first, so the read
		// after this one sees it.
		gone := exited(p.Exited)
		if info := readInfo(paths.Lock); info.Herdr == ident && o.alive(info) {
			pid = info.PID
		} else {
			pid = p.PID // no live holder to name
			if gone {
				return StartResult{PID: pid}, fmt.Errorf("%w: pid %d", ErrDaemonExited, pid)
			}
		}
		if time.Now().After(deadline) {
			return StartResult{PID: pid}, nil
		}
		if err := pause(ctx); err != nil {
			return StartResult{PID: pid}, err
		}
	}
}

// exited reports whether c is closed. A nil c never is.
func exited(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
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

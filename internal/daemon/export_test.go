package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/testslots"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// Backoff is the wait before resubscribe attempt n, counted from 0.
var Backoff = backoff

// MaxSnapshot is the largest encoded snapshot the op serves.
const MaxSnapshot = maxSnapshot

// Held is the probe Start and Stop make of the lock at path.
var Held = held

// HeldKeepingACopy is Held, with a copy of the probe's open file kept past
// the probe, as a child forked while the probe had the lock keeps one until
// it execs. The copy is a dup, which shares the open file as a fork's does;
// letGo closes it. A probe that finds the lock held makes no copy. It is
// Held's own probe: only the kept it passes differs from Held's.
func HeldKeepingACopy(path string) (isHeld bool, letGo func() error, err error) {
	fd, dupErr := -1, error(nil)
	isHeld, err = heldKeeping(path, func(f *os.File) {
		fd, dupErr = syscall.Dup(int(f.Fd()))
	})
	letGo = func() error {
		if fd < 0 {
			return nil
		}
		return syscall.Close(fd)
	}
	if err == nil && dupErr != nil {
		err = dupErr
	}
	return isHeld, letGo, err
}

// HealthOver is the health answer Run serves, base plus what it reads from
// st under ctx.
func HealthOver(ctx context.Context, st *store.Store, base HealthInfo) HealthInfo {
	return Options{Logger: zerolog.Nop()}.health(ctx, st, base)()
}

// BackCode is the refusal code the back op answers err with.
var BackCode = backCode

// BackOver runs one back op on st through h, as a fresh daemon would.
func BackOver(ctx context.Context, h HerdrClient, st *store.Store) (BackResult, error) {
	return Options{Herdr: h, Logger: zerolog.Nop()}.withDefaults().back(ctx, st, newWalker())
}

// StillServedOver is Run's poll check for a daemon on socket, bound to the
// server ident names and pinned at protocol, through the daemon's own
// client.
func StillServedOver(ctx context.Context, socket string, ident Identity, protocol uint32) error {
	o := Options{HerdrSocket: socket, Logger: zerolog.Nop()}.withDefaults().forServer(ident)
	return o.stillServed(ctx, ident, protocol)
}

// StillServedLogging is StillServedOver logging to log, where a ping that
// failed and still leaves the server up is a debug line. dialed, when set,
// runs once the ping is connected and before it is sent.
func StillServedLogging(ctx context.Context, socket string, ident Identity, protocol uint32, log zerolog.Logger, dialed func()) error {
	o := Options{HerdrSocket: socket, Logger: log}.withDefaults().forServer(ident)
	if c, ok := o.Herdr.(herdr.Client); ok && dialed != nil {
		check := c.Dialed
		c.Dialed = func() error { dialed(); return check() }
		o.Herdr = c
	}
	return o.stillServed(ctx, ident, protocol)
}

// FocusSnapshotOver takes one focus snapshot into st as the focus collector
// of a daemon on socket, bound to the server ident names, takes it: through
// the daemon's own client.
func FocusSnapshotOver(ctx context.Context, socket string, ident Identity, st *store.Store) error {
	o := Options{HerdrSocket: socket, Logger: zerolog.Nop()}.withDefaults().forServer(ident)
	return o.snapshotFocus(ctx, st, newWalker(), newViewSync(), ident)
}

// DialGuard is the check the daemon's client runs on every connection, for
// a daemon on socket bound to the server ident names; nil if that client does
// not dial socket.
func DialGuard(socket string, ident Identity) func() error {
	c := ServerClient(Options{HerdrSocket: socket}, ident)
	if c.SocketPath != socket {
		return nil
	}
	return c.Dialed
}

// ServerClient is the client a daemon of the server ident names dials
// through, built from o as Run builds it.
func ServerClient(o Options, ident Identity) herdr.Client {
	c, _ := o.forServer(ident).Herdr.(herdr.Client)
	return c
}

// CacheSnapshot encodes s as the team poll caches a team's snapshot, and
// returns what the snapshot op would then answer with.
func CacheSnapshot(s snapshot.Snapshot) (json.RawMessage, error) {
	return encodeSnapshot(s)
}

// TeamCache is the snapshot op's cache, filled as the team poll fills it,
// with no stage clock and no GitHub header.
type TeamCache struct{ c snapshotCache }

// Store caches every team in m, built at at, with spaces the team an op
// selecting nothing gets.
func (t *TeamCache) Store(m team.Model, spaces string, at time.Time) error {
	return t.c.store(m, spaces, at, nil, nil, nil)
}

// Load is what the snapshot op answers sel with.
func (t *TeamCache) Load(sel Selector) (json.RawMessage, error) { return t.c.load(sel) }

// WithGitHubTicks returns g with ticks standing in for its interval's
// ticker, so a test says when each round after the first runs.
func WithGitHubTicks(g GitHubOptions, ticks <-chan time.Time) GitHubOptions {
	g.ticks = ticks
	return g
}

// WithGitHubRounds returns g sending on rounds at each round's end, once
// every repo's calls and notices are done, so a test waits for the round
// itself rather than for its gh calls.
func WithGitHubRounds(g GitHubOptions, rounds chan<- struct{}) GitHubOptions {
	g.rounds = rounds
	return g
}

// WithStoreClose returns o with closeStore standing in for closing the state
// database when Run exits.
func WithStoreClose(o Options, closeStore func(*store.Store) error) Options {
	o.closeStore = closeStore
	return o
}

// WithProcLocks returns t with procLocks standing in for /proc/locks when
// the team poll counts the test slots.
func WithProcLocks(t TeamOptions, procLocks string) TeamOptions {
	t.procLocks = procLocks
	return t
}

// SlotsOfOnePoll counts the test slots as one team poll does, for locks,
// with procLocks and proc standing in for /proc/locks and /proc: it reads
// the lock list once, and gives each project's slots from that reading.
func SlotsOfOnePoll(procLocks, proc string, locks settings.TestLocks) func(project string) *snapshot.Slots {
	c := &slotCounter{locks: locks, procLocks: procLocks, proc: proc, read: testslots.Read, failingFor: map[string]string{}}
	return Options{Logger: zerolog.Nop()}.countSlots(c)
}

// WithLockReads returns t with read standing in for the team poll's one
// reading of the held locks, so a test sees each reading the poll makes.
func WithLockReads(t TeamOptions, read func(procLocks, proc string) (*testslots.Reading, error)) TeamOptions {
	t.readLocks = read
	return t
}

// WithProc returns t with proc standing in for /proc when the team poll
// tells a hold from its holder.
func WithProc(t TeamOptions, proc string) TeamOptions {
	t.proc = proc
	return t
}

// StartTimeFrom is the StartTime hook a daemon builds from a process table
// reader: nil when stat is nil.
var StartTimeFrom = startTimeFrom

// DefaultStartTime is the StartTime hook a daemon has when none is injected.
func DefaultStartTime() func(pid int) (uint64, error) {
	return Options{}.withDefaults().Hooks.StartTime
}

// DefaultStartedAt is the StartedAt hook a daemon has when none is injected.
func DefaultStartedAt() func(start uint64) (time.Time, error) {
	return Options{}.withDefaults().Hooks.StartedAt
}

// ServedBudget is the budget the daemon's own op map gives op with args,
// which the server enforces; OpBudget is the client's copy.
func ServedBudget(op string, args json.RawMessage) time.Duration {
	return workforceOps(&workforce.Service{}, new(atomic.Bool), nil)[op].budget(args)
}

// WorkforceOpNames is every op the daemon's workforce op map serves.
func WorkforceOpNames() []string {
	return slices.Sorted(maps.Keys(workforceOps(&workforce.Service{}, new(atomic.Bool), nil)))
}

// GroupDescriptions is the team poll's reader of the group descriptions
// (A36), driven one poll at a time.
type GroupDescriptions struct{ d *groupDescriptions }

// NewGroupDescriptions reads the shared file in dir; "" reads nothing.
func NewGroupDescriptions(dir string) GroupDescriptions {
	return GroupDescriptions{d: newGroupDescriptions(dir)}
}

// NewGroupDescriptionsReading is NewGroupDescriptions with read standing in
// for settings.Teams.
func NewGroupDescriptionsReading(dir string, read func(dir string) ([]settings.Team, error)) GroupDescriptions {
	d := newGroupDescriptions(dir)
	d.read = read
	return GroupDescriptions{d: d}
}

// Refresh is one poll's look at the file.
func (g GroupDescriptions) Refresh(log zerolog.Logger) { g.d.refresh(log) }

// Of is project's descriptions, by group.
func (g GroupDescriptions) Of(project string) map[string]string { return g.d.of(project) }

// WorkforceCode is the refusal code a workforce op answers err with.
var WorkforceCode = workforceCode

// ServeOp serves the daemon socket at path with one op, name, of budget,
// running run under ctx and answering each of its errors with code. It
// returns the function that closes the socket.
func ServeOp(ctx context.Context, path, name, code string, budget time.Duration, run func(context.Context) (any, error)) (func(), error) {
	ops := map[string]op{name: {
		budget: func(json.RawMessage) time.Duration { return budget },
		run:    func(ctx context.Context, _ json.RawMessage) (any, error) { return run(ctx) },
		code:   func(error) string { return code },
	}}
	srv, err := listen(ctx, path, func() HealthInfo { return HealthInfo{} }, func() (BackResult, error) { return BackResult{}, nil },
		func(Selector) (json.RawMessage, error) { return nil, ErrNotReady }, ops, zerolog.Nop())
	if err != nil {
		return nil, err
	}
	return srv.close, nil
}

// CallWith is one request with args, waited for up to timeout.
var CallWith = callWith

// WorkforceOf is the workforce service a daemon run with o builds, for the
// herdr server ident names, in o's directory for that server when o names
// one.
func WorkforceOf(o Options, ident Identity) *workforce.Service {
	paths, _ := PathsFor(o.StateDir, o.HerdrSocket)
	return o.newWorkforce(nil, ident, &team.IdleClock{}, paths.Dir)
}

// LogNotice writes m as the log sink does (A44).
func LogNotice(log zerolog.Logger, m notice.Message) error {
	return logSink{log}.Send(context.Background(), m)
}

// FlushNotices flushes b into s as the poll does.
func FlushNotices(o Options, b *notice.Batcher, s notice.Sink) {
	o.flushNotices(context.Background(), &notices{batch: b, sink: s})
}

// LaunchModels is the team poll's launch models (A39), driven one poll at a
// time.
type LaunchModels struct{ m *launchModels }

// NewLaunchModels is a cache with nothing in it.
func NewLaunchModels() LaunchModels { return LaunchModels{m: newLaunchModels()} }

// Refresh is one poll's look at panes, each a team's agent pane, with no
// process_info reply held from earlier in the poll.
func (l LaunchModels) Refresh(ctx context.Context, c herdr.Client, panes []herdr.PaneInfo, startTime func(int) (uint64, error), now time.Time, log zerolog.Logger) (map[string]string, error) {
	return l.m.refresh(ctx, c, paneProcesses{}, panes, startTime, now, log)
}

// RefreshHeld is Refresh with the replies pp already holds from earlier in
// the poll, as the reconcile leaves them.
func (l LaunchModels) RefreshHeld(ctx context.Context, c herdr.Client, pp PaneProcesses, panes []herdr.PaneInfo, startTime func(int) (uint64, error), now time.Time, log zerolog.Logger) (map[string]string, error) {
	return l.m.refresh(ctx, c, pp.pp, panes, startTime, now, log)
}

// SetClock measures the budget by clock instead of time.Now.
func (l LaunchModels) SetClock(clock func() time.Time) { l.m.clock = clock }

// Started is when each pane's agent process started, on the wall clock
// through wall, as the last Refresh left the entries (A47).
func (l LaunchModels) Started(wall func(uint64) (time.Time, error)) map[string]time.Time {
	return l.m.started(wall)
}

// PaneProcesses are one poll's process_info replies.
type PaneProcesses struct{ pp paneProcesses }

// NewPaneProcesses holds no reply yet.
func NewPaneProcesses() PaneProcesses { return PaneProcesses{pp: paneProcesses{}} }

// Ask is pane's reply this poll, asking herdr only the first time.
func (p PaneProcesses) Ask(ctx context.Context, c herdr.Client, pane string) (herdr.ProcessInfo, error) {
	return p.pp.ask(ctx, c, pane)
}

// AgentPanes are the panes of m's teams where herdr detects an agent.
var AgentPanes = agentPanes

// ViewSync is the agent view's state between the goroutines that feed it and
// syncView (A28 addendum, A49), for tests of its bookkeeping.
type ViewSync struct{ v *viewSync }

// NewViewSync is a fresh view state: no focus, no model, nothing sent.
func NewViewSync() ViewSync { return ViewSync{v: newViewSync()} }

// Focus is a focus event the collector reads.
func (s ViewSync) Focus(workspace string) { s.v.focus(workspace) }

// Resynced is the collector reading focus afresh.
func (s ViewSync) Resynced(workspace string) { s.v.resynced(workspace) }

// Seen is what a poll reads before it lists the panes.
func (s ViewSync) Seen() uint64 { return s.v.focusSeen() }

// Polled is a poll's model and the workspace its list had focused, read
// after seen.
func (s ViewSync) Polled(m team.Model, workspace string, seen uint64) { s.v.polled(m, workspace, seen) }

// Forget is a call that failed.
func (s ViewSync) Forget() { s.v.forget() }

// Next is syncView's decision: whether a view is due, whether it is a
// clear, and the function that records it sent, as a call that succeeds
// does.
func (s ViewSync) Next() (clear bool, sent func(), ok bool) {
	want, forgets, ok := s.v.next()
	return want.clear, func() { s.v.sentAt(want, forgets) }, ok
}

// GitHubSource is the GitHub collector's notice source (A45) on its own.
type GitHubSource struct{ g *githubNotices }

// NewGitHubSource is o's source on st, with o's notices settings.
func NewGitHubSource(o Options, st *store.Store) GitHubSource {
	return GitHubSource{o.newGitHubNotices(st, o.newNotices())}
}

// Collected diffs h's list, open, as the collector does after a list.
func (s GitHubSource) Collected(ctx context.Context, gh github.Runner, h settings.Header, open github.Open) {
	s.g.collected(ctx, gh, h, open)
}

// Take is what the source has handed the poll since the last Take.
func (s GitHubSource) Take() []notice.Notice { return s.g.n.github.take() }

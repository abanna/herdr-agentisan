package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// Backoff is the wait before resubscribe attempt n, counted from 0.
var Backoff = backoff

// MaxSnapshot is the largest encoded snapshot the op serves.
const MaxSnapshot = maxSnapshot

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

// FocusSnapshotOver takes one focus snapshot into st as the focus collector
// of a daemon on socket, bound to the server ident names, takes it: through
// the daemon's own client.
func FocusSnapshotOver(ctx context.Context, socket string, ident Identity, st *store.Store) error {
	o := Options{HerdrSocket: socket, Logger: zerolog.Nop()}.withDefaults().forServer(ident)
	return o.snapshotFocus(ctx, st, newWalker(), ident)
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

// CacheSnapshot caches s as the team poll does, and returns what the
// snapshot op would then answer with.
func CacheSnapshot(s snapshot.Snapshot) (json.RawMessage, error) {
	var c snapshotCache
	if err := c.store(s); err != nil {
		return nil, err
	}
	return c.load()
}

// WithGitHubTicks returns g with ticks standing in for its interval's
// ticker, so a test says when each round after the first runs.
func WithGitHubTicks(g GitHubOptions, ticks <-chan time.Time) GitHubOptions {
	g.ticks = ticks
	return g
}

// StartTimeFrom is the StartTime hook a daemon builds from a process table
// reader: nil when stat is nil.
var StartTimeFrom = startTimeFrom

// DefaultStartTime is the StartTime hook a daemon has when none is injected.
func DefaultStartTime() func(pid int) (uint64, error) {
	return Options{}.withDefaults().Hooks.StartTime
}

// WorkforceCode is the refusal code a workforce op answers err with.
var WorkforceCode = workforceCode

// ServeOp serves the daemon socket at path with one op, name, of budget,
// running run under ctx. It returns the function that closes the socket.
func ServeOp(ctx context.Context, path, name string, budget time.Duration, run func(context.Context) (any, error)) (func(), error) {
	ops := map[string]op{name: {
		budget: func(json.RawMessage) time.Duration { return budget },
		run:    func(ctx context.Context, _ json.RawMessage) (any, error) { return run(ctx) },
		code:   func(error) string { return "failed" },
	}}
	srv, err := listen(ctx, path, func() HealthInfo { return HealthInfo{} }, func() (BackResult, error) { return BackResult{}, nil },
		func() (json.RawMessage, error) { return nil, ErrNotReady }, ops, zerolog.Nop())
	if err != nil {
		return nil, err
	}
	return srv.close, nil
}

// CallWith is one request with args, waited for up to timeout.
var CallWith = callWith

// WorkforceOf is the workforce service a daemon run with o builds, for the
// herdr server ident names.
func WorkforceOf(o Options, ident Identity) *workforce.Service {
	return o.newWorkforce(nil, ident, &team.IdleClock{})
}

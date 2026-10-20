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

// FocusSnapshotOver takes one focus snapshot into st as the focus collector
// of a daemon on socket, bound to the server ident names, takes it: through
// the collector's own client.
func FocusSnapshotOver(ctx context.Context, socket string, ident Identity, st *store.Store) error {
	o := Options{HerdrSocket: socket, Logger: zerolog.Nop()}.withDefaults().forFocus(ident)
	return o.snapshotFocus(ctx, st, newWalker(), ident)
}

// FocusDialGuard is the check the focus collector's client runs on every
// connection, for a daemon on socket bound to the server ident names; nil if
// the collector's client is not a herdr.Client.
func FocusDialGuard(socket string, ident Identity) func() error {
	c, ok := Options{HerdrSocket: socket}.forFocus(ident).Herdr.(herdr.Client)
	if !ok || c.SocketPath != socket {
		return nil
	}
	return c.Dialed
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
	return t.c.store(m, spaces, at, nil, nil)
}

// Load is what the snapshot op answers sel with.
func (t *TeamCache) Load(sel Selector) (json.RawMessage, error) { return t.c.load(sel) }

// WithGitHubTicks returns g with ticks standing in for its interval's
// ticker, so a test says when each round after the first runs.
func WithGitHubTicks(g GitHubOptions, ticks <-chan time.Time) GitHubOptions {
	g.ticks = ticks
	return g
}

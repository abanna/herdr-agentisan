package daemon

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// Backoff is the wait before resubscribe attempt n, counted from 0.
var Backoff = backoff

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

package daemon

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// Backoff is the wait before resubscribe attempt n, counted from 0.
var Backoff = backoff

// HealthOver is the health answer Run serves, base plus what it reads from
// st under ctx.
func HealthOver(ctx context.Context, st *store.Store, base HealthInfo) HealthInfo {
	return Options{Logger: zerolog.Nop()}.health(ctx, st, base)()
}

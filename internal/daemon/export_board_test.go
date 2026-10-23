package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// BoardCaller is where a board op's caller runs, for the tests.
type BoardCaller = boardCaller

// ServeBoard serves the board ops alone on st, each caller placed with
// place, on a socket of its own in dir. stop ends it as a daemon stops:
// the ops' context first, then the server, once every op has answered.
// waiting reports whether a board wait from pane is in force.
func ServeBoard(t testing.TB, dir string, st *store.Store, place func(context.Context, Peer) (BoardCaller, error)) (socket string, stop func(), waiting func(pane string) bool) {
	t.Helper()
	opsCtx, stopOps := context.WithCancel(context.Background())
	socket = filepath.Join(dir, "d.sock")
	b := newBoard(st, place)
	srv, err := listen(opsCtx, socket, func() HealthInfo { return HealthInfo{} }, nil, nil, b.ops(), zerolog.Nop())
	if err != nil {
		stopOps()
		t.Fatalf("serve the board ops: %v", err)
	}
	var once sync.Once
	stop = func() { once.Do(func() { stopOps(); srv.close() }) }
	t.Cleanup(stop)
	waiting = func(pane string) bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		_, ok := b.waits[pane]
		return ok
	}
	return socket, stop, waiting
}

// PlaceBoardCaller is the daemon's placement of a board op's caller (A60).
func PlaceBoardCaller(view func() (*team.Model, map[string]string), c herdr.Client, stat report.StatFunc, uid int) func(context.Context, Peer) (BoardCaller, error) {
	return placeBoardCaller(view, c, stat, uid)
}

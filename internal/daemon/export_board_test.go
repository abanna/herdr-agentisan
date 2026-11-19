package daemon

import (
	"context"
	"database/sql"
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

// WaitOn is a board wait on st with ctx, its caller placed with place.
func WaitOn(ctx context.Context, st *store.Store, place func(context.Context, Peer) (BoardCaller, error), req BoardWaitRequest) (BoardStateResult, error) {
	return newBoard(st, place).wait(ctx, req)
}

// FailBoardInserts makes the board refuse every post inserted into the
// state.db at dbPath, or those matching when, an SQL condition on NEW,
// until drop is called; the test's cleanup calls it too. It works on a
// connection of its own, as the daemon's store holds its only one, so the
// trigger is not TEMP.
func FailBoardInserts(t testing.TB, dbPath, when string) (drop func()) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	cond := ""
	if when != "" {
		cond = " WHEN " + when
	}
	if _, err := db.ExecContext(context.Background(), "CREATE TRIGGER test_board_insert_fails BEFORE INSERT ON board_posts"+cond+
		" BEGIN SELECT RAISE(ABORT, 'test: the board refuses this post'); END"); err != nil {
		_ = db.Close()
		t.Fatalf("create the failing trigger: %v", err)
	}
	var once sync.Once
	drop = func() {
		once.Do(func() {
			defer db.Close() //nolint:errcheck // test cleanup
			if _, err := db.ExecContext(context.Background(), "DROP TRIGGER test_board_insert_fails"); err != nil {
				t.Errorf("drop the failing trigger: %v", err)
			}
		})
	}
	t.Cleanup(drop)
	return drop
}

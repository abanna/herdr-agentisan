package daemon_test

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// sqlHooks are the callbacks route_hook(name) runs, by test name: a
// trigger calls it inside the store's own statement, on the store's own
// connection, so a callback sees what is held while SQLite writes.
var sqlHooks sync.Map

func init() {
	sqlite.MustRegisterScalarFunction("route_hook", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		f, ok := sqlHooks.Load(args[0])
		if !ok {
			return nil, fmt.Errorf("no hook %v", args[0])
		}
		f.(func())()
		return nil, nil
	})
}

// onSQL runs f inside every statement trigger (a CREATE TRIGGER's timing
// and event, such as "BEFORE INSERT ON board_posts") fires for, in the
// state.db at r.db, until the test ends.
func onSQL(t *testing.T, r *boardRig, trigger string, f func()) {
	t.Helper()
	sqlHooks.Store(t.Name(), f)
	t.Cleanup(func() { sqlHooks.Delete(t.Name()) })
	rawExec(t, r.db, "CREATE TRIGGER route_probe "+trigger+" BEGIN SELECT route_hook('"+strings.ReplaceAll(t.Name(), "'", "''")+"'); END")
}

// TestBoardModeCriticalSectionsHoldRoute pins the whole of each critical
// section the route lock guards (#187's pre-check, NERD-5533 a15c65f8,
// must-fix 3), where the schedule tests can pass on a lock that covers
// only part of one: a board-key boss's delivery read is made with route
// held, and so is the durable write it decides, the typed claim's UPDATE
// to dispatching for a Codex boss and the board post's INSERT for a Claude
// one; remember takes route before mu, holding neither when it asks.
func TestBoardModeCriticalSectionsHoldRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ agent, trigger string }{
		"the typed claim": {"codex", "BEFORE UPDATE OF state ON messages WHEN new.state = 'dispatching'"},
		"the board post":  {"claude", "BEFORE INSERT ON board_posts"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
			var (
				reads, writes []bool
				order         []string
			)
			r.p.SetProbe(func(at string) {
				switch at {
				case "delivery":
					reads = append(reads, r.p.RouteHeld())
				case "remember":
					if r.p.RouteHeld() || r.p.MuHeld() {
						order = append(order, "remember asked for route with a lock held")
					}
				}
			})
			r.p.EndPoll(haModel(bossPane("ha-boss", c.agent)))
			m := sent(t, r.st, haBoss, coder, "done")
			onSQL(t, r, c.trigger, func() { writes = append(writes, r.p.RouteHeld()) })
			r.p.Pass()
			r.p.SetProbe(nil)

			assert.Empty(t, order, "remember takes route first, holding nothing")
			require.NotEmpty(t, reads, "the pass read the delivery")
			for i, held := range reads {
				assert.True(t, held, "delivery read %d is made with route held", i)
			}
			require.Len(t, writes, 1, "one durable write")
			assert.True(t, writes[0], "the durable write is made with route held")
			assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, m.ID).State)
		})
	}
}

// TestBoardModeLogsRunOffRoute: what a board-key boss's pass logs after
// its decision, a held queue's warning and a post's Info line, is written
// with route released, so a poll never waits behind a log writer (#187's
// pre-check, M11).
func TestBoardModeLogsRunOffRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ agent, line string }{
		"held":   {"", heldMsg},
		"posted": {"claude", "messages: posted to the board"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfgs := boardOn("ha")
			r := newBoardRig(t, []settings.Team{haTabs}, cfgs, nil)
			var seen, held atomic.Bool
			var p *daemon.Poller
			r.o.Logger = zerolog.New(writerFunc(func(b []byte) (int, error) {
				if strings.Contains(string(b), c.line) {
					seen.Store(true)
					held.Store(p.RouteHeld())
				}
				return len(b), nil
			}))
			p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, nil, nil)
			p.EndPoll(haModel(bossPane("ha-boss", c.agent)))
			sent(t, r.st, haBoss, coder, "done")
			p.Pass()
			require.True(t, seen.Load(), "the pass logged %q", c.line)
			assert.False(t, held.Load(), "logged with route released")
		})
	}
}

// writerFunc is a log writer made of f.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

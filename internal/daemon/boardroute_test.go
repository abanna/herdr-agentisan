package daemon_test

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/notice"
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
// must-fix 3, and its re-check ab09c058), where the schedule tests can pass
// on a lock that covers only part of one. A board-key boss's delivery read
// is made with route held, and so is the durable write it decides: the
// typed claim's UPDATE to dispatching for a Codex boss, and the board
// post's INSERT for a Claude one. Both are made within ONE hold of route,
// the one the read was made in, so no unlock and relock between the
// decision and the write, wherever it is put, gets past this.
func TestBoardModeCriticalSectionsHoldRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ agent, trigger string }{
		"the typed claim": {"codex", "BEFORE UPDATE OF state ON messages WHEN new.state = 'dispatching'"},
		"the board post":  {"claude", "BEFORE INSERT ON board_posts"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
			type seen struct {
				held  bool
				holds uint64
			}
			var (
				reads, writes []seen
				before        int // reads made before the write
			)
			r.p.EndPoll(haModel(bossPane("ha-boss", c.agent)))
			m := sent(t, r.st, haBoss, coder, "done")
			r.p.SetDeliveryProbe(func() { reads = append(reads, seen{r.p.RouteHeld(), r.p.RouteHolds()}) })
			onSQL(t, r, c.trigger, func() {
				before = len(reads)
				writes = append(writes, seen{r.p.RouteHeld(), r.p.RouteHolds()})
			})
			r.p.Pass()
			r.p.SetDeliveryProbe(nil)

			require.NotEmpty(t, reads, "the pass read the delivery")
			for i, read := range reads {
				assert.True(t, read.held, "delivery read %d is made with route held", i)
			}
			require.Len(t, writes, 1, "one durable write")
			assert.True(t, writes[0].held, "the durable write is made with route held")
			require.Positive(t, before, "a delivery read came before the write")
			assert.Equal(t, reads[before-1].holds, writes[0].holds, "the write is made in the same hold of route as the read it acts on")
			assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, m.ID).State)
		})
	}
}

// TestRememberTakesRouteBeforeMu pins the poll's lock order where its
// locks are taken (#187's re-check ab09c058, M05 and M06): with mu held
// elsewhere, a poll's remember still takes route, then waits for mu. A
// remember that took mu first, or took no route, would never hold route
// here; and the dispatcher, which takes route and then mu, could deadlock
// against the first.
func TestRememberTakesRouteBeforeMu(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	release := r.p.HoldMu()
	t.Cleanup(release)
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	}()
	took := assert.Eventually(t, r.p.RouteHeld, ready, time.Millisecond, "remember takes route while mu is held elsewhere")
	release()
	select {
	case <-polled:
	case <-time.After(ready):
		t.Fatal("the poll did not end")
	}
	if took {
		assert.Equal(t, notice.DeliverBoard, r.p.Delivery("ha"), "the poll remembered Claude")
	}
}

// TestBoardModeLogsRunOffRoute: what a board-key boss's pass logs after
// its decision, a held queue's warning and a post's Info line, is written
// with route released (#187's pre-check, M11). Two checks, so one that
// is lost leaves the other: route is free when the line is written, and a
// real poll that changes the remembered agent completes while the log
// writer is stalled on that line (re-check ab09c058, C2).
func TestBoardModeLogsRunOffRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ agent, line, then string }{
		"held":   {"", heldMsg, "claude"},
		"posted": {"claude", "messages: posted to the board", "codex"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfgs := boardOn("ha")
			r := newBoardRig(t, []settings.Team{haTabs}, cfgs, nil)
			var (
				p     *daemon.Poller
				held  atomic.Bool
				once  sync.Once
				atLog = make(chan struct{})
				go_   = make(chan struct{})
			)
			unstall := sync.OnceFunc(func() { close(go_) })
			t.Cleanup(unstall)
			r.o.Logger = zerolog.New(writerFunc(func(b []byte) (int, error) {
				if strings.Contains(string(b), c.line) {
					once.Do(func() {
						held.Store(p.RouteHeld())
						close(atLog)
						<-go_
					})
				}
				return len(b), nil
			}))
			p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, nil, nil)
			p.EndPoll(haModel(bossPane("ha-boss", c.agent)))
			sent(t, r.st, haBoss, coder, "done")
			passed := make(chan struct{})
			go func() {
				defer close(passed)
				p.Pass()
			}()
			select {
			case <-atLog:
			case <-time.After(ready):
				t.Fatal("the pass never logged " + c.line)
			}
			assert.False(t, held.Load(), "logged with route released")
			polled := make(chan struct{})
			go func() {
				defer close(polled)
				p.EndPoll(haModel(bossPane("ha-boss", c.then)))
			}()
			select {
			case <-polled:
			case <-time.After(ready):
				t.Error("a poll waited for the pass's log line")
			}
			unstall()
			<-passed
			<-polled
		})
	}
}

// writerFunc is a log writer made of f.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

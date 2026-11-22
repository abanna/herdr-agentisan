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
// the one the read was made in, and route is taken exactly once per
// decision with one read in each hold, so no unlock and relock between
// the decision and the write gets past this, even one followed by a
// second, weaker read (the third re-check, 1a1bbe3f).
func TestBoardModeCriticalSectionsHoldRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		agent, trigger string
		holds          uint64 // route's holds in the pass: one per decision
	}{
		// untyped's decision, then claimTyped's: the claim, and the next
		// attempt, which finds the queue empty.
		"the typed claim": {"codex", "BEFORE UPDATE OF state ON messages WHEN new.state = 'dispatching'", 3},
		// untyped's decision, which posts.
		"the board post": {"claude", "BEFORE INSERT ON board_posts", 1},
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
			start := r.p.RouteHolds()
			r.p.SetDeliveryProbe(func() { reads = append(reads, seen{r.p.RouteHeld(), r.p.RouteHolds()}) })
			onSQL(t, r, c.trigger, func() {
				before = len(reads)
				writes = append(writes, seen{r.p.RouteHeld(), r.p.RouteHolds()})
			})
			r.p.Pass()
			r.p.SetDeliveryProbe(nil)

			assert.Equal(t, c.holds, r.p.RouteHolds()-start, "route is taken once per decision, and never let go and retaken within one")
			assert.Len(t, reads, int(c.holds), "one delivery read in each hold")
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

// TestBoardModeDecisionsSurviveAPollInEveryGap puts a real poll in every
// gap the test can reach between a board-key boss's delivery read and the
// durable write it decides (#187's third re-check, NERD-5533 1a1bbe3f, as
// ruled). Its checkpoints are each delivery read (the probe) and each clock
// read (the claim's, the post's and the settlements'). At the first one
// where route is free, a real poll changes the remembered agent and
// flushes a notice. With route held from the decision through the write,
// that poll can only come after the write: the typed boss is typed the row
// as it stood when claimed, and the board boss's post holds only what was
// queued before.
func TestBoardModeDecisionsSurviveAPollInEveryGap(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ from, to string }{
		"the typed claim": {"codex", "claude"},
		"the board post":  {"claude", "codex"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfgs := boardOn("ha")
			b := notice.NewBatcher(cfgs)
			r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
			var armed, switched bool
			checkpoint := func() {
				if !armed || switched || r.p.RouteHeld() {
					return
				}
				switched = true
				b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Gone, Text: "after the switch"}})
				r.p.EndPoll(haModel(bossPane("ha-boss", c.to)))
			}
			r.o.Now = func() time.Time {
				checkpoint()
				return time.Now()
			}
			r.p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, b, nil)
			// Queue the older notice on the typed path before arming the pass.
			b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "before"}})
			r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
			r.p.EndPoll(haModel(bossPane("ha-boss", c.from)))
			r.p.SetDeliveryProbe(checkpoint)
			armed = true
			r.p.Pass()
			armed = false
			r.p.SetDeliveryProbe(nil)

			var bodies, texts []string
			for _, p := range r.posts(t, "ha") {
				bodies = append(bodies, p.Body)
			}
			for _, p := range r.pr.all() {
				texts = append(texts, p.Text)
			}
			if c.from == "codex" {
				assert.Equal(t, []string{"[agentisan] notices:\nbefore"}, texts, "typed as claimed: no notice flushed by a poll after the decision")
				assert.Empty(t, bodies, "nothing posted in this pass")
			} else {
				assert.Equal(t, []string{"before"}, bodies, "posted as decided: no notice flushed by a poll after the decision")
				assert.Empty(t, texts, "nothing typed in this pass")
			}
		})
	}
}

// TestRememberTakesRouteBeforeMu pins the poll's lock order where its
// locks are taken (#187's re-check ab09c058, M05 and M06): with mu held
// elsewhere, a poll's remember still takes route, then waits for mu with
// route held. A remember that took mu first, or took no route, would never
// take route here; and the dispatcher, which takes route and then mu, could
// deadlock against the first. One that let route go before taking mu (the
// round-5 mutant N2) takes it, then frees it while it waits: route's count
// says when it was taken, so its release is seen at once.
func TestRememberTakesRouteBeforeMu(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	release := r.p.HoldMu()
	t.Cleanup(release)
	start := r.p.RouteHolds()
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	}()
	took := assert.Eventually(t, func() bool { return r.p.RouteHolds() > start }, 2*time.Second, time.Millisecond, "remember takes route while mu is held elsewhere")
	if took {
		assert.Never(t, func() bool { return !r.p.RouteHeld() }, 100*time.Millisecond, time.Millisecond, "and keeps it while it waits for mu")
	}
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

// TestRememberWaitsForAHeldRoute: a poll that comes while route is held
// does not change the remembered agent until route is released (#187's
// fourth re-check, NERD-5533 b200696a, F2). A remember that went on without
// route when it found it taken (MV5) changes it at once.
func TestRememberWaitsForAHeldRoute(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	release := r.p.HoldRoute()
	t.Cleanup(release)
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	}()
	select {
	case <-polled:
		t.Error("the poll ended while route was held")
	case <-time.After(300 * time.Millisecond):
	}
	assert.Equal(t, notice.DeliverTyped, r.p.Delivery("ha"), "the remembered agent waits for route")
	release()
	select {
	case <-polled:
	case <-time.After(ready):
		t.Fatal("the poll did not end once route was released")
	}
	assert.Equal(t, notice.DeliverBoard, r.p.Delivery("ha"), "then the poll remembers Claude")
}

// TestAPollWaitsForAPassHoldingRoute: a poll that comes while a board-key
// boss's pass holds route, from its decision to its write, waits for the
// pass (#187's fourth re-check, b200696a, F2). The pass is paused at its
// first clock read, the claim's or the post's, which it makes with route
// held; a poll that changes the remembered agent and flushes a notice is
// started then and given 300 ms before the pass goes on. With route waited
// for, the typed boss is typed the row as it stood, and the board boss's
// post holds only what was queued before. A remember that went on without
// route (MV5) switches the boss mid-decision, and the notice it flushes is
// appended to the row being claimed or posted.
func TestAPollWaitsForAPassHoldingRoute(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ from, to string }{
		"the typed claim": {"codex", "claude"},
		"the board post":  {"claude", "codex"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfgs := boardOn("ha")
			b := notice.NewBatcher(cfgs)
			r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
			var pause atomic.Bool
			atClock, resume := make(chan struct{}), make(chan struct{})
			r.o.Now = func() time.Time {
				if pause.Swap(false) {
					close(atClock)
					<-resume
				}
				return time.Now()
			}
			r.p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, b, nil)
			// Queue the older notice on the typed path before arming the pass.
			b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "before"}})
			r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
			r.p.EndPoll(haModel(bossPane("ha-boss", c.from)))
			// Keep any claimed prompt in flight until the switching poll has flushed.
			releasePrompt := r.pr.hold(t, "ha-boss")
			pause.Store(true)
			passed := make(chan struct{})
			go func() {
				defer close(passed)
				r.p.Pass()
			}()
			select {
			case <-atClock:
			case <-time.After(ready):
				close(resume)
				t.Fatal("the pass never read the clock")
			}
			held := r.p.RouteHeld()
			b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Gone, Text: "after the switch"}})
			polled := make(chan struct{})
			go func() {
				defer close(polled)
				r.p.EndPoll(haModel(bossPane("ha-boss", c.to)))
			}()
			select {
			case <-polled:
				t.Error("the poll ended while the pass held route")
			case <-time.After(300 * time.Millisecond):
			}
			close(resume)
			<-polled
			releasePrompt()
			<-passed
			assert.True(t, held, "the pass was paused with route held")

			var bodies, texts []string
			for _, p := range r.posts(t, "ha") {
				bodies = append(bodies, p.Body)
			}
			for _, p := range r.pr.all() {
				texts = append(texts, p.Text)
			}
			if c.from == "codex" {
				assert.Equal(t, []string{"[agentisan] notices:\nbefore"}, texts, "typed as claimed")
				assert.Empty(t, bodies, "nothing posted in this pass")
			} else {
				assert.Equal(t, []string{"before"}, bodies, "posted as decided")
				assert.Empty(t, texts, "nothing typed in this pass")
			}
		})
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

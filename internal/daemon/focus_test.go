package daemon_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // the driver the store registers, for read-only looks at state.db

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/store"
)

func paths(t *testing.T, stateDir string, h *fakeHerdr) daemon.Paths {
	t.Helper()
	p, err := daemon.PathsFor(stateDir, h.path)
	require.NoError(t, err)
	return p
}

// query runs a read-only query on the daemon's state.db the way a person with
// sqlite3 would (A1), and returns the first column of every row.
func query(t *testing.T, path, q string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck // read-only test connection
	rows, err := db.QueryContext(t.Context(), q)
	if err != nil {
		return nil // not created yet
	}
	defer rows.Close() //nolint:errcheck // read-only test query
	out := []string{}
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// focusIs waits until the daemon's focus history, newest first, is want.
func focusIs(t *testing.T, stateDir string, h *fakeHerdr, want ...string) {
	t.Helper()
	db := paths(t, stateDir, h).DB
	var got []string
	if !assert.Eventually(t, func() bool {
		got = query(t, db, "SELECT pane_id FROM focus ORDER BY seq DESC")
		return assert.ObjectsAreEqual(want, got)
	}, 3*time.Second, 10*time.Millisecond) {
		t.Fatalf("focus history is %q, want %q", got, want)
	}
}

func panes(from, to int) []string {
	var out []string
	for i := to; i >= from; i-- {
		out = append(out, fmt.Sprintf("w1:p%d", i))
	}
	return out
}

// TestRunRecordsFocusEvents is D3's focus collection: the daemon subscribes
// to pane.focused, records the pane focused when it starts, then every focus
// event, and the history keeps the newest 32.
func TestRunRecordsFocusEvents(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	h.focus("w1:p0")
	dir := shortDir(t)
	run(t, opts(t, dir, h))

	s := h.nextStream(t)
	focusIs(t, dir, h, "w1:p0")
	for i := 1; i <= 40; i++ {
		s.lines <- herdrtest.PaneFocused(fmt.Sprintf("w1:p%d", i), "w1")
	}
	focusIs(t, dir, h, panes(9, 40)...)

	info := healthy(t, dir, h)
	assert.Equal(t, store.FocusCap, info.FocusRows)
	assert.Equal(t, store.SchemaVersion(), info.StoreVersion)
	assert.Equal(t, 1, h.subscribes())
	for _, r := range h.srv.Requests() {
		if r.Method == "events.subscribe" {
			assert.JSONEq(t, `{"subscriptions":[{"type":"pane.focused"}]}`, string(r.Params))
		}
	}
	st, err := os.Stat(paths(t, dir, h).DB)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "state.db is the owner's only")
}

// TestRunResyncsWhenTheSubscriptionEnds is A3 and A11: when herdr drops
// events (events_lost) or the subscription's connection drops while the
// server is unchanged, the daemon resubscribes and records the pane focused
// now, since focus may have moved while it was not listening.
func TestRunResyncsWhenTheSubscriptionEnds(t *testing.T) {
	t.Parallel()

	for name, end := range map[string]func(*stream){
		"events lost": func(s *stream) {
			s.lines <- herdrtest.EventsLost(s.id)
			close(s.lines)
		},
		"connection dropped": func(s *stream) { close(s.lines) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHerdr(t, 22)
			h.focus("w1:p0")
			dir := shortDir(t)
			r := run(t, opts(t, dir, h))

			first := h.nextStream(t)
			focusIs(t, dir, h, "w1:p0")
			first.lines <- herdrtest.PaneFocused("w1:p1", "w1")
			focusIs(t, dir, h, "w1:p1", "w1:p0")

			h.focus("w2:p7") // focus moves while no event reaches the daemon
			end(first)
			second := h.nextStream(t)
			focusIs(t, dir, h, "w2:p7", "w1:p1", "w1:p0")
			second.lines <- herdrtest.PaneFocused("w2:p8", "w2")
			focusIs(t, dir, h, "w2:p8", "w2:p7", "w1:p1", "w1:p0")

			assert.Equal(t, 2, h.subscribes())
			assert.True(t, r.alive(), "losing the subscription does not stop the daemon")
		})
	}
}

// TestRunFollowsFocusWhenTheSnapshotFails: a pane.list that fails costs the
// snapshot row, not the subscription.
func TestRunFollowsFocusWhenTheSnapshotFails(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	h.mu.Lock()
	h.listErr = true
	h.mu.Unlock()
	dir := shortDir(t)
	run(t, opts(t, dir, h))

	s := h.nextStream(t)
	s.lines <- herdrtest.PaneFocused("w1:p1", "w1")
	focusIs(t, dir, h, "w1:p1")
}

// TestFocusFollowsOnlyItsOwnServer: when its herdr server is replaced, focus
// collection stops rather than subscribing to the new server, which the next
// daemon belongs to, even before the daemon's poll notices and exits.
func TestFocusFollowsOnlyItsOwnServer(t *testing.T) {
	t.Parallel()

	t.Run("before the poll notices", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		o := opts(t, dir, h)
		o.Poll = time.Hour // the poll never runs: only the collector can react
		r := run(t, o)
		h.nextStream(t)

		h.restart()
		time.Sleep(time.Second) // several backoffs' worth
		assert.Zero(t, h.subscribes(), "the collector never subscribed to the server that replaced its own")
		assert.True(t, r.alive())
	})
	t.Run("the daemon exits", func(t *testing.T) {
		t.Parallel()
		h := newHerdr(t, 22)
		dir := shortDir(t)
		r := run(t, opts(t, dir, h))
		h.nextStream(t)

		h.restart()
		require.ErrorIs(t, r.wait(t, 3*time.Second), daemon.ErrHerdrGone)
		assert.Zero(t, h.subscribes())
	})
}

// TestFocusIgnoresAServerReplacedWhileSubscribing is DB6's check-to-use race,
// landed deterministically: herdr's socket is replaced while the daemon's
// subscribe is in flight, after its identity check and before the ack. The
// subscription made across the swap is dropped unread: neither the event it
// streams nor the replacement's focused pane ever reaches state.db, and the
// replacement, which the next daemon belongs to, is never asked anything.
func TestFocusIgnoresAServerReplacedWhileSubscribing(t *testing.T) {
	t.Parallel()
	h := &fakeHerdr{t: t, path: filepath.Join(shortDir(t), "h.sock"), streams: make(chan *stream, 64)}
	h.focus("w1:p0")
	normal := h.serve(22)
	swap, swapped := make(chan struct{}), make(chan struct{})
	var first sync.Once
	h.h = func(r herdrtest.Request) herdrtest.Reply {
		if r.Method != "events.subscribe" {
			return normal(r)
		}
		first.Do(func() {
			close(swap)
			<-swapped
		})
		lines := make(chan []byte, 1)
		lines <- herdrtest.PaneFocused("w9:streamed", "w9")
		return herdrtest.Reply{Result: herdrtest.SubscriptionStarted(), Stream: lines}
	}
	h.srv = herdrtest.StartAt(t, h.path, h.h)
	dir := shortDir(t)
	o := opts(t, dir, h)
	o.Poll = time.Hour // the poll never runs: only the collector can react
	r := run(t, o)

	select {
	case <-swap:
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon did not subscribe")
	}
	require.NoError(t, os.Remove(h.path))
	replacement := &fakeHerdr{t: t, path: h.path, streams: make(chan *stream, 64)}
	replacement.focus("w2:replacement")
	replacement.h = replacement.serve(22)
	replacement.srv = herdrtest.StartAt(t, replacement.path, replacement.h)
	close(swapped)

	time.Sleep(time.Second)
	assert.Empty(t, query(t, paths(t, dir, h).DB, "SELECT pane_id FROM focus"),
		"nothing from a subscription made across the swap is recorded")
	assert.Empty(t, replacement.srv.Requests(), "the replacement is never asked for its panes or its events")
	assert.True(t, r.alive(), "the collector stops; Run's poll ends the daemon")
}

// TestRunStoreFailures: a state database the daemon cannot use stops it at
// startup with an error naming why, before it serves its socket. It leaves
// the lock free, so a daemon started once the cause is gone runs.
func TestRunStoreFailures(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		plant func(t *testing.T, db string)
		fix   func(t *testing.T, db string)
		want  error
	}{
		"state.db cannot be opened": {
			plant: func(t *testing.T, db string) { require.NoError(t, os.Mkdir(db, 0o700)) },
			fix:   func(t *testing.T, db string) { require.NoError(t, os.Remove(db)) },
			want:  store.ErrOpen,
		},
		"state.db is from a newer build": {
			plant: func(t *testing.T, db string) {
				raw, err := sql.Open("sqlite", db)
				require.NoError(t, err)
				defer raw.Close() //nolint:errcheck // test setup
				_, err = raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", store.SchemaVersion()+1))
				require.NoError(t, err)
			},
			fix:  func(t *testing.T, db string) { require.NoError(t, os.Remove(db)) },
			want: store.ErrSchemaTooNew,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHerdr(t, 22)
			dir := shortDir(t)
			p := paths(t, dir, h)
			require.NoError(t, os.MkdirAll(p.Dir, 0o700))
			tc.plant(t, p.DB)

			r := run(t, opts(t, dir, h))
			require.ErrorIs(t, r.wait(t, 3*time.Second), tc.want)
			assert.NoFileExists(t, p.Socket, "a daemon without its store never serves")

			tc.fix(t, p.DB)
			run(t, opts(t, dir, h))
			assert.Equal(t, store.SchemaVersion(), healthy(t, dir, h).StoreVersion)
		})
	}
}

// clock is a test clock the daemon reads instead of the wall clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestRunPrunesFinishedHandoffs is A1's retention, run by the daemon: once at
// start, then once every PruneInterval of its clock, and not in between.
func TestRunPrunesFinishedHandoffs(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	h := newHerdr(t, 22)
	dir := shortDir(t)
	p := paths(t, dir, h)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	st, err := store.Open(t.Context(), p.DB)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	raw, err := sql.Open("sqlite", p.DB)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at) VALUES ('w', 'p', 'g', 'w1:p1', 1, 'active', 0)")
	require.NoError(t, err)
	for label, h := range map[string]struct {
		state   string
		updated time.Time
	}{
		"old":     {state: "acknowledged", updated: t0.Add(-15 * day)},
		"soon":    {state: "failed", updated: t0.Add(-14*day + time.Hour)},
		"pending": {state: "pending", updated: t0.Add(-30 * day)},
	} {
		_, err = raw.ExecContext(t.Context(),
			"INSERT INTO handoffs (logical_id, old_pane, new_pane, state, created_at, updated_at) VALUES ('w', ?, 'w1:p2', ?, ?, ?)",
			label, h.state, h.updated.UnixNano(), h.updated.UnixNano())
		require.NoError(t, err)
	}
	require.NoError(t, raw.Close())

	clk := &clock{now: t0}
	o := opts(t, dir, h)
	o.Now = clk.Now
	run(t, o)
	handoffs := func() []string { return query(t, p.DB, "SELECT old_pane FROM handoffs ORDER BY old_pane") }
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"pending", "soon"}, handoffs()) },
		3*time.Second, 10*time.Millisecond, "the prune at start removes the handoff past retention")

	clk.advance(2 * time.Hour) // "soon" is now past retention, but no prune is due
	time.Sleep(15 * o.Poll)
	assert.Equal(t, []string{"pending", "soon"}, handoffs(), "no prune runs before PruneInterval has passed")

	clk.advance(daemon.PruneInterval - 2*time.Hour - time.Nanosecond)
	time.Sleep(15 * o.Poll)
	assert.Equal(t, []string{"pending", "soon"}, handoffs(), "one nanosecond before PruneInterval, still no prune")

	clk.advance(time.Nanosecond) // exactly PruneInterval after the first prune
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"pending"}, handoffs()) },
		3*time.Second, 10*time.Millisecond, "the next prune removes it; a pending handoff stays")
	assert.Equal(t, 24*time.Hour, daemon.PruneInterval)
}

// TestRunClosesTheStoreOnEveryExit: however the daemon exits, it closes the
// state database after the focus collector and the socket have stopped.
// SQLite removes state.db-wal and -shm when the last connection closes
// cleanly, so their absence is the proof.
func TestRunClosesTheStoreOnEveryExit(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		end  func(r *running, h *fakeHerdr)
		want error
	}{
		"cancelled":                  {end: func(r *running, _ *fakeHerdr) { r.cancel() }},
		"herdr's socket removed":     {end: func(_ *running, h *fakeHerdr) { h.stop() }, want: daemon.ErrHerdrGone},
		"herdr replaced":             {end: func(_ *running, h *fakeHerdr) { h.restart() }, want: daemon.ErrHerdrGone},
		"herdr crashed, socket left": {end: func(_ *running, h *fakeHerdr) { h.crash() }, want: daemon.ErrHerdrGone},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHerdr(t, 22)
			h.focus("w1:p0")
			dir := shortDir(t)
			r := run(t, opts(t, dir, h))
			h.nextStream(t)
			focusIs(t, dir, h, "w1:p0")
			db := paths(t, dir, h).DB
			require.FileExists(t, db+"-wal", "the running daemon holds the database open")

			tc.end(r, h)
			err := r.wait(t, 3*time.Second)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			assert.NoFileExists(t, db+"-wal", "the store was closed")
			assert.NoFileExists(t, db+"-shm", "the store was closed")
			assert.Equal(t, []string{"w1:p0"}, query(t, db, "SELECT pane_id FROM focus"), "what it recorded is durable")
		})
	}
}

// TestRunRetriesARefusedSubscription: herdr refusing events.subscribe is
// retried with backoff, not given up on, and focus is followed once it
// succeeds.
func TestRunRetriesARefusedSubscription(t *testing.T) {
	t.Parallel()
	h := newHerdr(t, 22)
	h.mu.Lock()
	h.refuse = 2
	h.mu.Unlock()
	h.focus("w1:p0")
	dir := shortDir(t)
	r := run(t, opts(t, dir, h))

	s := h.nextStream(t)
	focusIs(t, dir, h, "w1:p0")
	s.lines <- herdrtest.PaneFocused("w1:p1", "w1")
	focusIs(t, dir, h, "w1:p1", "w1:p0")
	assert.Equal(t, 3, h.subscribes(), "two refused, the third followed")
	assert.True(t, r.alive())
}

// TestHealthReportsTheStore: health adds the store's schema version and its
// focus rows, read when asked, to what the daemon knows of itself. Rows that
// cannot be read are -1, never a plausible count.
func TestHealthReportsTheStore(t *testing.T) {
	t.Parallel()
	base := daemon.HealthInfo{PID: 4242, Version: "v", HerdrProtocol: 22}

	tests := map[string]struct {
		prepare func(t *testing.T, st *store.Store) context.Context
		rows    int
	}{
		"an empty history": {prepare: func(t *testing.T, _ *store.Store) context.Context { return t.Context() }, rows: 0},
		"three focus rows": {prepare: func(t *testing.T, st *store.Store) context.Context {
			for _, p := range []string{"a", "b", "c"} {
				require.NoError(t, st.RecordFocus(t.Context(), p, time.Now()))
			}
			return t.Context()
		}, rows: 3},
		"a store that cannot be read": {prepare: func(t *testing.T, st *store.Store) context.Context {
			require.NoError(t, st.Close())
			return t.Context()
		}, rows: -1},
		"a daemon that is stopping": {prepare: func(t *testing.T, _ *store.Store) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}, rows: -1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = st.Close() })
			ctx := tc.prepare(t, st)

			got := daemon.HealthOver(ctx, st, base)
			assert.Equal(t, tc.rows, got.FocusRows)
			assert.Equal(t, store.SchemaVersion(), got.StoreVersion)
			got.FocusRows, got.StoreVersion = 0, 0
			assert.Equal(t, base, got, "everything else is the daemon's own")
		})
	}
}

// TestBackoffIsBounded: the wait before each resubscribe doubles from 100 ms
// and never exceeds 30 s, however many attempts have failed.
func TestBackoffIsBounded(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		attempt int
		want    time.Duration
	}{
		"first":            {attempt: 0, want: 100 * time.Millisecond},
		"second":           {attempt: 1, want: 200 * time.Millisecond},
		"sixth":            {attempt: 5, want: 3200 * time.Millisecond},
		"last below cap":   {attempt: 8, want: 25600 * time.Millisecond},
		"at the cap":       {attempt: 9, want: 30 * time.Second},
		"far past the cap": {attempt: 1 << 30, want: 30 * time.Second},
		"negative":         {attempt: -1, want: 100 * time.Millisecond},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, daemon.Backoff(tc.attempt))
		})
	}
}

package workforce_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestRetire: by logical id or pane id the worker's pane closes and its row
// is retired; retiring it again changes nothing; a pane started by hand
// closes with no row to retire; the boss's tab and names nobody holds are
// refused. A worker's tab with a shell split beside the agent closes the
// agent's pane; a stale pane id closes the pane herdr resolves it to; a tab
// so labelled in another workspace is no worker of the team's.
func TestRetire(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	w2 := f.worker(t, "ha-coder-2", "coder")
	hand := f.h.addTab(f.ws, "ha-qa-3", "claude", "idle")
	shell := f.h.addTab(f.ws, "notes", "", "unknown")

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Changed: true, Outcome: workforce.OutcomeRetired}, res)
	assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
	assert.Nil(t, f.h.tabByLabel(w.ID), "its tab closes with its only pane")

	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Outcome: workforce.OutcomeAlreadyRetired}, res)

	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w2.PaneID})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w2.ID, PaneID: w2.PaneID, Changed: true, Outcome: workforce.OutcomeRetired}, res, "a pane id finds the worker's row")
	assert.Equal(t, store.StatusRetired, f.row(t, w2.ID).Status)

	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "ha-qa-3"})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: "ha-qa-3", PaneID: hand, Changed: true, Outcome: workforce.OutcomeRetired}, res, "a hand-started worker by its tab's label")

	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: shell})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{PaneID: shell, Changed: true, Outcome: workforce.OutcomeRetired}, res, "a pane with no logical id")

	split := f.h.addTab(f.ws, "ha-qa-4", "", "unknown")
	agent := f.h.addPane(split, "claude", "idle")
	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "ha-qa-4"})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: "ha-qa-4", PaneID: agent, Changed: true, Outcome: workforce.OutcomeRetired}, res, "the agent's pane, not the shell's")
	f.h.mu.Lock()
	assert.NotNil(t, f.h.pane(split), "the shell split stays")
	f.h.mu.Unlock()

	stale := f.h.addTab(f.ws, "notes-2", "", "unknown")
	f.h.mu.Lock()
	f.h.alias["w3:p1"] = stale
	f.h.mu.Unlock()
	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "w3:p1"})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{PaneID: stale, Changed: true, Outcome: workforce.OutcomeRetired}, res, "a frozen pane id, as herdr resolves it")

	theirs := f.h.addTab(f.h.addWorkspace("elsewhere"), "ha-coder-6", "claude", "idle")
	_, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "ha-coder-6"})
	require.ErrorIs(t, err, workforce.ErrUnknownWorker)
	f.h.mu.Lock()
	assert.NotNil(t, f.h.pane(theirs), "another workspace's tab is left alone")
	f.h.mu.Unlock()

	_, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: f.boss})
	require.ErrorIs(t, err, workforce.ErrNotAWorker)
	for _, name := range []string{"ha-coder-9", "HA-CODER-2", "ha-cöder-2", "ha-coder-2\n", "ha-coder-\xff", "-x"} {
		_, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: name})
		require.ErrorIs(t, err, workforce.ErrUnknownWorker, "%q", name)
	}
	_, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: " "})
	require.ErrorIs(t, err, workforce.ErrBadRequest)
	f.h.mu.Lock()
	assert.NotNil(t, f.h.pane(f.boss), "the boss's pane is still open")
	f.h.mu.Unlock()
}

// TestRetireTrustsTheRowOverTheTab: a live row decides which pane is its
// worker's, whatever tab is labelled with its id.
func TestRetireTrustsTheRowOverTheTab(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	bound := f.h.addTab(f.ws, "notes-1", "claude", "idle")
	f.bindRow(t, "ha-coder-3", "coder", bound)
	labelled := f.h.addTab(f.ws, "ha-coder-3", "claude", "idle")

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "ha-coder-3"})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: "ha-coder-3", PaneID: bound, Changed: true, Outcome: workforce.OutcomeRetired}, res)
	f.h.mu.Lock()
	assert.NotNil(t, f.h.pane(labelled), "the tab labelled with the id stays")
	f.h.mu.Unlock()
}

// TestRetireARowBoundToNoPane: a live row bound to no pane, whether lost,
// written with no terminal, or with a terminal herdr lists nowhere, is
// retired with nothing closed (A35), even while the pane at its pane id is
// still open.
func TestRetireARowBoundToNoPane(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	lost := f.worker(t, "ha-coder-1", "coder")
	f.markLost(t, lost.ID)
	f.bindRow(t, "ha-coder-2", "coder", "w10:p98")
	f.bindRow(t, "ha-qa-1", "qa", "w10:p99")
	f.exec(t, "UPDATE workers SET terminal_id = 'term_closed' WHERE logical_id = 'ha-qa-1'")

	f.bindRow(t, "ha-qa-2", "qa", "w10:p97")
	// A pane herdr lists with no terminal, its handoff written: a row with
	// no pane waits on nothing, so it never takes this one for its own.
	blank := f.h.addTab(f.ws, "notes", "", "unknown")
	f.h.setPane(blank, func(p *fakePane) { p.terminal, p.tokens = "", map[string]string{team.HandoffKey: "⟳ READY"} })
	for id, after := range map[string]bool{lost.ID: false, "ha-coder-2": false, "ha-qa-1": false, "ha-qa-2": true} {
		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: id, AfterHandoff: after})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{ID: id, Changed: true, Outcome: workforce.OutcomeUnbound}, res, "%s, after its handoff: %t, with no pane to wait on", id, after)
		assert.Equal(t, store.StatusRetired, f.row(t, id).Status)
	}
	assert.Empty(t, f.h.params("pane.close"))
	assert.NotNil(t, f.h.tabByLabel(lost.ID), "a lost row's old pane is no proven pane of its")
}

// TestRetireRefusesAPendingHandoff: a row bound to no pane while its handoff
// is pending is refused with ErrHandoffPending and left live, since ending it
// would strand the handoff (NERD-5261).
func TestRetireRefusesAPendingHandoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.bindRow(t, "ha-coder-1", "coder", "w10:p99")
	f.exec(t, "UPDATE workers SET terminal_id = 'term_away' WHERE logical_id = 'ha-coder-1'")
	_, err := f.st.BeginHandoff(t.Context(), "ha-coder-1", "w10:p98", "term_next", time.Now())
	require.NoError(t, err)

	_, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "ha-coder-1"})
	require.ErrorIs(t, err, workforce.ErrHandoffPending)
	assert.Equal(t, store.StatusActive, f.row(t, "ha-coder-1").Status)
	assert.True(t, f.row(t, "ha-coder-1").RetiredAt.IsZero(), "still live")
}

// TestRetireClosesOnlyTheRowsTerminal: retire closes the pane herdr lists
// with the row's terminal, at whatever id it has now, and never the pane at
// the row's pane id once herdr has given that id to another terminal.
func TestRetireClosesOnlyTheRowsTerminal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	moved := f.worker(t, "ha-coder-1", "coder")
	f.h.setPane(moved.PaneID, func(p *fakePane) { p.id = "w30:p1" })
	reissued := f.worker(t, "ha-coder-2", "coder")
	f.h.setPane(reissued.PaneID, func(p *fakePane) { p.terminal = "term_stranger" })

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: moved.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: moved.ID, PaneID: "w30:p1", Changed: true, Outcome: workforce.OutcomeRetired}, res)

	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: reissued.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.OutcomeUnbound, res.Outcome)
	assert.NotNil(t, f.h.tabByLabel(reissued.ID), "the stranger at the old id stays open")

	w := f.worker(t, "ha-qa-1", "qa")
	f.h.setPane(w.PaneID, func(p *fakePane) { p.id = "w30:p2" })
	res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "w30:p2"})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: "w30:p2", Changed: true, Outcome: workforce.OutcomeRetired}, res,
		"a pane id finds its row by terminal")
	assert.Equal(t, []map[string]any{{"pane_id": "w30:p1"}, {"pane_id": "w30:p2"}}, f.h.params("pane.close"))
}

// TestRetireRechecksThePaneBeforeClosing: between the list retire reads and
// its close, pane.get must still show the pane with the target's terminal.
// A pane that moved closes nothing and refuses ErrPaneChanged, its row left
// live; a pane that went, its id reissued or not, or that goes between the
// pane.get and the close, is the worker gone.
func TestRetireRechecksThePaneBeforeClosing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		method string
		change func(f *fakeHerdr, pane string)
		want   string
	}{
		"moved": {method: "pane.get", change: func(f *fakeHerdr, pane string) {
			p := *f.pane(pane)
			f.pane(pane).terminal = "term_stranger"
			p.id = "w30:p1"
			f.panes = append(f.panes, &p)
		}},
		"reissued":                             {method: "pane.get", change: func(f *fakeHerdr, pane string) { f.pane(pane).terminal = "term_stranger" }, want: workforce.OutcomeGone},
		"closed":                               {method: "pane.get", change: (*fakeHerdr).dropPane, want: workforce.OutcomeGone},
		"closed between the get and the close": {method: "pane.close", change: (*fakeHerdr).dropPane, want: workforce.OutcomeGone},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			f.h.onCall = func(method string) { // under the fake's lock
				if method == tc.method {
					tc.change(f.h, w.PaneID)
				}
			}

			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
			if tc.want == "" {
				require.ErrorIs(t, err, workforce.ErrPaneChanged)
				assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
				assert.Empty(t, f.h.params("pane.close"))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Changed: true, Outcome: tc.want}, res)
			assert.Equal(t, store.StatusGone, f.row(t, w.ID).Status)
			f.h.mu.Lock()
			defer f.h.mu.Unlock()
			if p := f.h.pane(w.PaneID); p != nil {
				assert.Equal(t, "term_stranger", p.terminal, "the stranger at the old id stays open")
			}
		})
	}
}

// TestRetireAWorkerRetiredMeanwhile: a worker another op retires while this
// retire closes its pane is retired all the same: its row stays retired, and
// no error says it was not live.
func TestRetireAWorkerRetiredMeanwhile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	row := f.row(t, w.ID).Worker
	var otherErr error
	f.h.onCall = func(method string) {
		if method == "pane.close" {
			otherErr = f.st.SetWorkerStatus(context.Background(), row, store.StatusRetired, time.Now())
		}
	}

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	f.h.mu.Lock()
	gotOther := otherErr
	f.h.mu.Unlock()
	require.NoError(t, gotOther)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Changed: true, Outcome: workforce.OutcomeRetired}, res)
	assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
}

// polling returns a channel closed once the fake answers its second
// pane.list: a lone retire has read the layout and its handoff wait has
// begun.
func polling(h *fakeHerdr) <-chan struct{} {
	began := make(chan struct{})
	lists := 0
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onCall = func(method string) { // under the fake's lock
		if method == "pane.list" {
			if lists++; lists == 2 {
				close(began)
			}
		}
	}
	return began
}

// TestRetireAfterHandoff: the pane closes once it carries a handoff token,
// and not before; one that goes while waited for is the worker gone; one
// with no handoff in time stays open, ErrHandoffTimeout.
func TestRetireAfterHandoff(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 2*time.Second, workforce.DefaultHandoffPoll, "the daemon polls every 2 s (A23)")
	t.Run("handoff written", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		closedEarly := make(chan bool, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			closedEarly <- slices.Contains(f.h.methods(), "pane.close")
			f.h.setPane(w.PaneID, func(p *fakePane) { p.tokens = map[string]string{team.HandoffKey: "⟳ READY"} })
		}()

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
		require.NoError(t, err)
		assert.False(t, <-closedEarly, "no pane.close before the handoff")
		assert.Len(t, f.h.params("pane.close"), 1)
		assert.Equal(t, workforce.OutcomeRetired, res.Outcome)
		assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
	})
	t.Run("pane moved meanwhile", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		go func() {
			time.Sleep(50 * time.Millisecond)
			f.h.setPane(w.PaneID, func(p *fakePane) {
				p.id, p.tokens = "w30:p1", map[string]string{team.HandoffKey: "⟳ READY"}
			})
		}()

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: "w30:p1", Changed: true, Outcome: workforce.OutcomeRetired}, res,
			"the pane closes at its id now")
	})
	t.Run("another retire meanwhile", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		other := f.worker(t, "ha-qa-1", "qa")
		waiting := polling(f.h)
		done := make(chan error, 1)
		go func() {
			_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
			done <- err
		}()
		<-waiting

		type answer struct {
			res workforce.RetireResult
			err error
		}
		otherDone := make(chan answer, 1)
		go func() {
			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: other.ID})
			otherDone <- answer{res, err}
		}()
		select {
		case got := <-otherDone:
			require.NoError(t, got.err)
			assert.Equal(t, workforce.OutcomeRetired, got.res.Outcome)
		case <-time.After(2 * time.Second):
			t.Fatal("the other retire waited: the handoff wait holds the lock")
		}
		select {
		case <-done:
			t.Fatal("the first retire ended before its handoff")
		default:
		}
		f.h.setPane(w.PaneID, func(p *fakePane) { p.tokens = map[string]string{team.HandoffKey: "⟳ READY"} })
		require.NoError(t, <-done)
	})
	t.Run("herdr away meanwhile", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		path := f.h.srv.Path
		type answer struct {
			res workforce.RetireResult
			err error
		}
		waiting := polling(f.h)
		done := make(chan answer, 1)
		go func() {
			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
			done <- answer{res, err}
		}()
		<-waiting
		f.h.srv.Close()
		time.Sleep(100 * time.Millisecond) // it polls a herdr that is not there
		f.h.setPane(w.PaneID, func(p *fakePane) { p.tokens = map[string]string{team.HandoffKey: "⟳ READY"} })
		herdrtest.StartAt(t, path, f.h.handle)

		got := <-done
		require.NoError(t, got.err, "a herdr away for a while does not end the wait")
		assert.Equal(t, workforce.OutcomeRetired, got.res.Outcome)
	})
	t.Run("a call that times out", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		var lists atomic.Int32
		wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
			if r.Method == "pane.list" && lists.Add(1) == 2 { // the wait's first poll, held past its call's timeout
				time.Sleep(2 * time.Second)
				f.h.setPane(w.PaneID, func(p *fakePane) { p.tokens = map[string]string{team.HandoffKey: "⟳ READY"} })
			}
			return next()
		})
		f.svc.Herdr.Timeout = 500 * time.Millisecond

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
		require.NoError(t, err, "a call that times out does not end the wait (QA #78, F1)")
		assert.Equal(t, workforce.OutcomeRetired, res.Outcome)
	})
	t.Run("pane gone first", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		waiting := polling(f.h)
		go func() {
			<-waiting
			assert.NoError(t, f.h.client().ClosePane(context.Background(), w.PaneID))
		}()
		// A regression that waits on fails here, not at the suite's timeout.
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		res, err := f.svc.Retire(ctx, workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Changed: true, Outcome: workforce.OutcomeGone}, res)
		assert.Equal(t, store.StatusGone, f.row(t, w.ID).Status)
		// A gone pane's id can be reissued, so retire closes nothing more.
		assert.Len(t, f.h.params("pane.close"), 1, "only the test's own close")
	})
	t.Run("polled every HandoffPoll", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		f.svc.HandoffPoll = 20 * time.Millisecond
		lists := 0
		f.h.onCall = func(method string) { // under the fake's lock
			if method != "pane.list" {
				return
			}
			if lists++; lists == 4 {
				f.h.pane(w.PaneID).tokens = map[string]string{team.HandoffKey: "⟳ READY"}
			}
		}
		// The third poll sees the handoff: in time at 20 ms, never at the
		// 2 s default.
		ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
		defer cancel()

		res, err := f.svc.Retire(ctx, workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
		require.NoError(t, err)
		assert.Equal(t, workforce.OutcomeRetired, res.Outcome)
	})
	t.Run("no handoff in time", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		f.svc.HandoffPoll = 20 * time.Millisecond
		// Long enough that the deadline ends the wait, not the reads before it.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		_, err := f.svc.Retire(ctx, workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
		require.ErrorIs(t, err, workforce.ErrHandoffTimeout)
		assert.NotNil(t, f.h.tabByLabel(w.ID), "its pane stays open")
		assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
	})
}

// replacing opens a replacement pane in w's tab and begins w's handoff onto
// it, as 2b's spawn --replaces will, and returns the pane and the handoff.
func replacing(t *testing.T, f *fixture, w worker) (string, int64) {
	t.Helper()
	next := f.h.addPane(w.PaneID, "claude", "idle")
	f.h.mu.Lock()
	terminal := f.h.pane(next).terminal
	f.h.mu.Unlock()
	h, err := f.st.BeginHandoff(t.Context(), w.ID, next, terminal, time.Now())
	require.NoError(t, err)
	return next, h
}

// handoffState is the state the handoffs row h is in.
func handoffState(t *testing.T, f *fixture, h int64) string {
	t.Helper()
	pending, err := f.st.PendingHandoffs(t.Context())
	require.NoError(t, err)
	if slices.ContainsFunc(pending, func(p store.Handoff) bool { return p.ID == h }) {
		return "pending"
	}
	return "ended"
}

// TestRetireRefusesWhileAHandoffIsPending: a worker whose handoff is
// pending is retired by no path, by logical id, by pane id or after its
// handoff, which is refused at once rather than after a wait: each answers
// ErrHandoffPending, naming the replacement, and closes nothing (A33 part
// 2a).
func TestRetireRefusesWhileAHandoffIsPending(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	next, h := replacing(t, f, w)

	for name, req := range map[string]workforce.RetireRequest{
		"by logical id": {Target: w.ID},
		"by pane id":    {Target: w.PaneID},
		"after handoff": {Target: w.ID, AfterHandoff: true},
	} {
		// A wait would end in ErrHandoffTimeout first.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		_, err := f.svc.Retire(ctx, req)
		cancel()
		require.ErrorIs(t, err, workforce.ErrHandoffPending, name)
		assert.Contains(t, err.Error(), next, "%s: it names the replacement", name)
	}
	assert.Empty(t, f.h.params("pane.close"))
	assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
	assert.Equal(t, "pending", handoffState(t, f, h))
}

// TestRetireChecksTheHandoffUnderTheLock: a handoff begun once retire has
// read the rows, while it waits for the worker's handoff token, is seen under
// the workforce lock, where a handoff begins, before anything closes or
// ends: whether the pane is still there or gone, the row stays live.
func TestRetireChecksTheHandoffUnderTheLock(t *testing.T) {
	t.Parallel()
	for name, paneGoes := range map[string]bool{"pane still there": false, "pane gone": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			lists := 0
			f.h.onCall = func(method string) { // under the fake's lock
				if method != "pane.list" {
					return
				}
				if lists++; lists == 2 { // the wait's first poll
					_, err := f.st.BeginHandoff(context.Background(), w.ID, "w10:p50", "term_next", time.Now())
					assert.NoError(t, err)
					if paneGoes {
						f.h.dropPane(w.PaneID)
					} else {
						f.h.pane(w.PaneID).tokens = map[string]string{team.HandoffKey: "⟳ READY"}
					}
				}
			}

			_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
			require.ErrorIs(t, err, workforce.ErrHandoffPending)
			assert.Empty(t, f.h.params("pane.close"))
			row := f.row(t, w.ID)
			assert.Equal(t, store.StatusActive, row.Status, "never retired, never gone: the ack completes it onto the replacement")
			assert.True(t, row.RetiredAt.IsZero())
		})
	}
}

// TestRetireMeetsTheStoresBackstop: a handoff begun outside the workforce
// lock, after retire's check, as no op does, still never strands: state.db
// refuses to end the row, and retire answers ErrHandoffPending with the row
// live, as the old pane gone mid-rotation leaves it.
func TestRetireMeetsTheStoresBackstop(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	f.h.onCall = func(method string) {
		if method == "pane.close" {
			_, err := f.st.BeginHandoff(context.Background(), w.ID, "w10:p50", "term_next", time.Now())
			assert.NoError(t, err)
		}
	}

	_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.ErrorIs(t, err, workforce.ErrHandoffPending)
	assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
}

// TestRetireAbandonsARotation: retiring a pending handoff's replacement pane
// fails the handoff, then closes the pane: the worker keeps its row and its
// pane, its inbox is released there, and it can be retired again. The
// replacement is matched by its terminal, else by its pane id while herdr
// lists the recorded terminal nowhere (a herdr live handoff renews it), and
// one herdr lists nowhere has nothing to close. A pane id herdr reissued,
// whose recorded terminal is listed elsewhere, is no replacement.
func TestRetireAbandonsARotation(t *testing.T) {
	t.Parallel()
	t.Run("by its terminal", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		next, h := replacing(t, f, w)
		to := store.Address{Kind: store.KindWorker, ID: w.ID}
		_, err := f.st.Send(t.Context(), to, store.Address{Kind: store.KindBoss, ID: "ha-boss"}, "held during the handoff", time.Now())
		require.NoError(t, err)

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: next})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{PaneID: next, Changed: true, Outcome: workforce.OutcomeRetired, HandoffFailed: w.ID}, res)
		assert.Equal(t, "ended", handoffState(t, f, h))
		f.h.mu.Lock()
		assert.Nil(t, f.h.pane(next), "the replacement closed")
		assert.NotNil(t, f.h.pane(w.PaneID), "the worker's own pane stays")
		f.h.mu.Unlock()
		row := f.row(t, w.ID)
		assert.Equal(t, store.StatusActive, row.Status)
		assert.Equal(t, w.PaneID, row.PaneID)
		m, ok, err := f.st.Claim(t.Context(), to, time.Now())
		require.NoError(t, err)
		require.True(t, ok, "the inbox is released")
		assert.Equal(t, "held during the handoff", m.Body)

		res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
		require.NoError(t, err)
		assert.Equal(t, workforce.OutcomeRetired, res.Outcome, "with no handoff pending the worker retires")
	})
	t.Run("by its pane id, its terminal renewed", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		next, h := replacing(t, f, w)
		f.h.setPane(next, func(p *fakePane) { p.terminal = "term_renewed" })

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: next})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{PaneID: next, Changed: true, Outcome: workforce.OutcomeRetired, HandoffFailed: w.ID}, res)
		assert.Equal(t, "ended", handoffState(t, f, h))
	})
	t.Run("herdr fails before the handoff does", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		next, h := replacing(t, f, w)
		f.h.setPane(next, func(p *fakePane) { p.terminal = "term_renewed" })
		f.h.mu.Lock()
		f.h.fail["pane.list"], f.h.failAfter["pane.list"] = "internal_error", 1 // the read under the lock, after the layout's
		f.h.mu.Unlock()

		_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: next})
		require.ErrorIs(t, err, workforce.ErrHerdr)
		assert.Equal(t, "pending", handoffState(t, f, h), "nothing failed")
		assert.Empty(t, f.h.params("pane.close"), "nothing closed")
	})
	t.Run("listed nowhere", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		h, err := f.st.BeginHandoff(t.Context(), w.ID, "w10:p77", "term_closed", time.Now())
		require.NoError(t, err)

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "w10:p77"})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{PaneID: "w10:p77", Changed: true, Outcome: workforce.OutcomeGone, HandoffFailed: w.ID}, res)
		assert.Equal(t, "ended", handoffState(t, f, h))
		assert.Empty(t, f.h.params("pane.close"))
		assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)

		_, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "w10:p77"})
		require.ErrorIs(t, err, workforce.ErrUnknownWorker, "once failed, the id names nothing")
	})
	t.Run("listed nowhere, then found under another id", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		h, err := f.st.BeginHandoff(t.Context(), w.ID, "w10:p77", "term_moved", time.Now())
		require.NoError(t, err)
		lists := 0
		f.h.onCall = func(method string) { // under the fake's lock
			if method == "pane.list" {
				if lists++; lists == 2 { // under the workforce lock: it moved meanwhile
					p := f.h.newPane(f.ws, f.h.pane(w.PaneID).tab)
					p.terminal = "term_moved"
				}
			}
		}

		_, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: "w10:p77"})
		require.ErrorIs(t, err, workforce.ErrUnknownWorker, "the replacement is the pane with its terminal now")
		assert.Equal(t, "pending", handoffState(t, f, h))
	})
	t.Run("a reissued pane id", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		stranger := f.h.addPane(w.PaneID, "", "unknown")
		elsewhere := f.h.addTab(f.ws, "notes", "", "unknown")
		f.h.mu.Lock()
		moved := f.h.pane(elsewhere).terminal
		f.h.mu.Unlock()
		h, err := f.st.BeginHandoff(t.Context(), w.ID, stranger, moved, time.Now())
		require.NoError(t, err)

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: stranger})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{PaneID: stranger, Changed: true, Outcome: workforce.OutcomeRetired}, res)
		assert.Equal(t, "pending", handoffState(t, f, h), "the replacement is the pane with its terminal")
	})
	t.Run("the close fails after the fail", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		next, h := replacing(t, f, w)
		f.h.mu.Lock()
		f.h.fail["pane.close"] = "internal_error"
		f.h.mu.Unlock()

		_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: next})
		require.ErrorIs(t, err, workforce.ErrHerdr)
		assert.Equal(t, "ended", handoffState(t, f, h), "failed first: no close error holds the inbox")

		f.h.mu.Lock()
		delete(f.h.fail, "pane.close")
		f.h.mu.Unlock()
		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: next})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{PaneID: next, Changed: true, Outcome: workforce.OutcomeRetired}, res, "the retry closes an ordinary pane")
	})
}

// TestRetireRereadsTheRowUnderTheLock: a worker rebound while retire waited
// for its handoff, by a handoff completed meanwhile or by the team poll's
// re-key after a herdr live handoff, is another session's now: retire
// closes nothing and ends nothing (ErrPaneChanged), and a retry reads it
// afresh.
func TestRetireRereadsTheRowUnderTheLock(t *testing.T) {
	t.Parallel()
	for name, rebind := range map[string]func(f *fixture, w worker, row store.Worker) error{
		"a handoff completed meanwhile": func(f *fixture, w worker, _ store.Worker) error {
			h, err := f.st.BeginHandoff(context.Background(), w.ID, "w10:p50", "term_next", time.Now())
			if err != nil {
				return err
			}
			f.h.pane(w.PaneID).tokens = map[string]string{team.HandoffKey: "⟳ READY"}
			return f.st.CompleteHandoff(context.Background(), h, time.Now())
		},
		"re-keyed by the poll meanwhile": func(f *fixture, w worker, row store.Worker) error {
			f.h.dropPane(w.PaneID) // a live handoff renews its terminal: the old one is listed nowhere
			return f.st.RekeyWorker(context.Background(), row, "w10:p51", "term_renewed")
		},
		"bound again at a new generation meanwhile": func(f *fixture, w worker, row store.Worker) error {
			if err := f.st.SetWorkerStatus(context.Background(), row, store.StatusRetired, time.Now()); err != nil {
				return err
			}
			again := store.Worker{LogicalID: w.ID, Project: "herdr-agentisan", Group: "coder", PaneID: w.PaneID, TerminalID: row.TerminalID}
			if _, err := f.st.BindWorker(context.Background(), again, time.Now()); err != nil {
				return err
			}
			f.h.pane(w.PaneID).tokens = map[string]string{team.HandoffKey: "⟳ READY"}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			row := f.row(t, w.ID).Worker
			lists := 0
			f.h.onCall = func(method string) { // under the fake's lock
				if method == "pane.list" {
					if lists++; lists == 2 { // the wait's first poll
						assert.NoError(t, rebind(f, w, row))
					}
				}
			}

			_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
			require.ErrorIs(t, err, workforce.ErrPaneChanged)
			assert.Empty(t, f.h.params("pane.close"))
			now := f.row(t, w.ID)
			assert.True(t, now.RetiredAt.IsZero(), "still live")
			assert.NotEqual(t, []any{row.Generation, row.TerminalID}, []any{now.Generation, now.TerminalID}, "bound anew")
		})
	}
}

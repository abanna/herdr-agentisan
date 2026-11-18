package workforce_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// shellWorker is a worker whose row has its shell, as the team poll records
// it once the row is bound (A35): pid 100, the fake's shell in every pane.
func shellWorker(t *testing.T, f *fixture, id, group string) worker {
	t.Helper()
	w := f.worker(t, id, group)
	f.exec(t, "UPDATE workers SET shell_pid = 100, shell_start = 1 WHERE logical_id = ?", id)
	return w
}

// liveHandoff renews pane's terminal and keeps its id, as a herdr live
// handoff does (A35): the PTY is handed over, so the shell stays, and every
// token is lost. Call it under the fake's lock.
func liveHandoff(h *fakeHerdr, pane string) {
	p := h.pane(pane)
	p.terminal, p.tokens = "term_renewed", nil
}

// restart renews pane's terminal on a new shell, as a herdr restart does: a
// pane at the worker's id that runs another shell is not the worker's. Call
// it under the fake's lock.
func restart(h *fakeHerdr, pane string) {
	p := h.pane(pane)
	p.terminal, p.tokens, p.shell = "term_restarted", nil, 300
}

// TestRetireWaitsForTheRekeyOfALiveHandoff (NERD-5534): a herdr live
// handoff renews a worker pane's terminal and keeps its id and shell, and
// the team poll re-keys the row up to a poll later. In that window retire
// reads the row's terminal listed nowhere, yet the worker still runs: by
// logical id, by pane id, during --after-handoff's wait or at the close,
// retire closes nothing and ends nothing (ErrPaneChanged), and the row stays
// live for the poll to re-key.
func TestRetireWaitsForTheRekeyOfALiveHandoff(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		// at is the fake call the live handoff lands on; "" before retire.
		at           string
		nth          int
		byPane       bool
		afterHandoff bool
	}{
		"during the handoff wait":          {at: "pane.list", nth: 2, afterHandoff: true},
		"at the close":                     {at: "pane.get", nth: 1},
		"before retire, by its id":         {},
		"before retire, by its pane":       {byPane: true},
		"before retire, after its handoff": {afterHandoff: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := shellWorker(t, f, "ha-coder-1", "coder")
			if tc.at == "" {
				f.h.mu.Lock()
				liveHandoff(f.h, w.PaneID)
				f.h.mu.Unlock()
			} else {
				n := 0
				f.h.onCall = func(method string) { // under the fake's lock
					if method == tc.at {
						if n++; n == tc.nth {
							liveHandoff(f.h, w.PaneID)
						}
					}
				}
			}
			target := w.ID
			if tc.byPane {
				target = w.PaneID
			}

			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: target, AfterHandoff: tc.afterHandoff})
			assert.ErrorIs(t, err, workforce.ErrPaneChanged, "answered %+v", res)
			assert.Empty(t, f.h.params("pane.close"), "the worker's pane is not closed")
			f.h.mu.Lock()
			assert.NotNil(t, f.h.pane(w.PaneID), "the worker's pane stays open")
			f.h.mu.Unlock()
			row := f.row(t, w.ID)
			assert.Equal(t, store.StatusActive, row.Status, "never gone, never retired")
			assert.True(t, row.RetiredAt.IsZero(), "still live")
			if t.Failed() {
				return
			}

			// The poll re-keys it; the retry retires it on its pane.
			require.NoError(t, f.st.RekeyWorker(t.Context(), row.Worker, w.PaneID, "term_renewed"))
			f.h.mu.Lock()
			f.h.onCall = nil
			f.h.mu.Unlock()
			res, err = f.svc.Retire(t.Context(), workforce.RetireRequest{Target: target})
			require.NoError(t, err)
			assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Changed: true, Outcome: workforce.OutcomeRetired}, res)
			assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
		})
	}
}

// TestRetireEndsOnlyTheRowItRead (NERD-5534): a re-key the team poll writes
// under no lock, once retire has re-read the row under its own and before
// it ends the row, makes the row another binding's: retire's ending is a
// compare-and-set on the terminal it read, as the poll's writes are, so the
// row stays live and retire answers ErrPaneChanged.
func TestRetireEndsOnlyTheRowItRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	row := f.row(t, w.ID).Worker
	var rekeyed error
	f.h.onCall = func(method string) { // under the fake's lock
		if method != "pane.get" {
			return
		}
		// A live handoff, and the poll's re-key, land between retire's
		// read under the lock and its write.
		f.h.dropPane(w.PaneID)
		p := f.h.newPane(f.ws, "")
		p.terminal = "term_renewed"
		rekeyed = f.st.RekeyWorker(context.Background(), row, p.id, p.terminal)
	}

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	assert.ErrorIs(t, err, workforce.ErrPaneChanged, "answered %+v", res)
	f.h.mu.Lock()
	require.NoError(t, rekeyed)
	f.h.mu.Unlock()
	now := f.row(t, w.ID)
	assert.True(t, now.RetiredAt.IsZero(), "still live")
	assert.Equal(t, store.StatusActive, now.Status)
	assert.Equal(t, "term_renewed", now.TerminalID)
}

// TestRetireAWorkerWhosePaneReallyWent: the neighbours of NERD-5534's fix.
// A worker whose pane closed, or whose pane id now runs another shell (a
// herdr restart), is gone, whatever shell its row recorded: retire ends it
// as before, gone at the close or while it waits, unbound when it reads the
// row with no pane, and closes nothing of the pane at its old id.
func TestRetireAWorkerWhosePaneReallyWent(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		at           string
		nth          int
		change       func(*fakeHerdr, string)
		afterHandoff bool
		want         string
	}{
		"closed while it waits":        {at: "pane.list", nth: 2, change: (*fakeHerdr).dropPane, afterHandoff: true, want: workforce.OutcomeGone},
		"closed at the close":          {at: "pane.get", nth: 1, change: (*fakeHerdr).dropPane, want: workforce.OutcomeGone},
		"closed before retire":         {change: (*fakeHerdr).dropPane, want: workforce.OutcomeUnbound},
		"restarted while it waits":     {at: "pane.list", nth: 2, change: restart, afterHandoff: true, want: workforce.OutcomeGone},
		"restarted at the close":       {at: "pane.get", nth: 1, change: restart, want: workforce.OutcomeGone},
		"restarted before retire":      {change: restart, want: workforce.OutcomeUnbound},
		"restarted, after its handoff": {change: restart, afterHandoff: true, want: workforce.OutcomeUnbound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := shellWorker(t, f, "ha-coder-1", "coder")
			if tc.at == "" {
				f.h.mu.Lock()
				tc.change(f.h, w.PaneID)
				f.h.mu.Unlock()
			} else {
				n := 0
				f.h.onCall = func(method string) { // under the fake's lock
					if method == tc.at {
						if n++; n == tc.nth {
							tc.change(f.h, w.PaneID)
						}
					}
				}
			}

			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: tc.afterHandoff})
			require.NoError(t, err)
			want := workforce.RetireResult{ID: w.ID, Changed: true, Outcome: tc.want}
			wantStatus := store.StatusRetired
			if tc.want == workforce.OutcomeGone {
				want.PaneID, wantStatus = w.PaneID, store.StatusGone
			}
			assert.Equal(t, want, res)
			assert.Equal(t, wantStatus, f.row(t, w.ID).Status)
			assert.Empty(t, f.h.params("pane.close"), "nothing of the old id is closed")
		})
	}
}

// TestRetireAWorkerAfterItsHandoffWithItsShell: a worker whose row has its
// shell retires after its handoff as one without does: its pane closes once
// it carries the token.
func TestRetireAWorkerAfterItsHandoffWithItsShell(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	f.h.setPane(w.PaneID, func(p *fakePane) { p.tokens = map[string]string{team.HandoffKey: "⟳ READY"} })

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Changed: true, Outcome: workforce.OutcomeRetired}, res)
	assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
}

// TestRetireRefusesAPendingHandoffAcrossALiveHandoff: a worker whose
// handoff is pending is refused (ErrHandoffPending) by every path, its pane
// renewed by a live handoff or not: A33 part 2a's rule comes first.
func TestRetireRefusesAPendingHandoffAcrossALiveHandoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	next, h := replacing(t, f, w)
	f.h.mu.Lock()
	liveHandoff(f.h, w.PaneID)
	f.h.mu.Unlock()

	for name, req := range map[string]workforce.RetireRequest{
		"by logical id": {Target: w.ID},
		"by pane id":    {Target: w.PaneID},
		"after handoff": {Target: w.ID, AfterHandoff: true},
	} {
		_, err := f.svc.Retire(t.Context(), req)
		require.ErrorIs(t, err, workforce.ErrHandoffPending, name)
		assert.Contains(t, err.Error(), next, name)
	}
	assert.Empty(t, f.h.params("pane.close"))
	assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
	assert.Equal(t, "pending", handoffState(t, f, h))
}

// TestRetireCannotTellAPaneItCannotRead: a pane that may hold a worker whose
// terminal herdr lists nowhere, which herdr cannot describe or knows no
// shell of yet, is no proof either way: retire ends nothing and answers
// ErrHerdr, for a retry.
func TestRetireCannotTellAPaneItCannotRead(t *testing.T) {
	t.Parallel()
	t.Run("herdr fails", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := shellWorker(t, f, "ha-coder-1", "coder")
		f.h.mu.Lock()
		liveHandoff(f.h, w.PaneID)
		f.h.fail["pane.process_info"] = "internal_error"
		f.h.mu.Unlock()

		_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
		require.ErrorIs(t, err, workforce.ErrHerdr)
		assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
		assert.Empty(t, f.h.params("pane.close"))
	})
	t.Run("no shell pid yet", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := shellWorker(t, f, "ha-coder-1", "coder")
		f.h.mu.Lock()
		liveHandoff(f.h, w.PaneID)
		f.h.mu.Unlock()
		wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
			reply := next()
			if r.Method == "pane.process_info" && reply.Error == nil {
				reply.Result.(map[string]any)["process_info"].(map[string]any)["shell_pid"] = 0
			}
			return reply
		})

		_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
		require.ErrorIs(t, err, workforce.ErrHerdr)
		assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
		assert.Empty(t, f.h.params("pane.close"))
	})
}

// TestRetireTellsAShellByItsStart: a pane at the worker's id that runs a
// shell with the row's pid but another start time, or one whose start
// cannot be read (it has exited), is not the worker's, as the team poll
// judges it (A35): the worker is retired unbound, and that pane stays.
func TestRetireTellsAShellByItsStart(t *testing.T) {
	t.Parallel()
	for name, start := range map[string]func(int) (uint64, error){
		"another start":    func(int) (uint64, error) { return 2, nil },
		"start unreadable": func(int) (uint64, error) { return 0, errors.New("no such process") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := shellWorker(t, f, "ha-coder-1", "coder")
			f.h.mu.Lock()
			liveHandoff(f.h, w.PaneID)
			f.h.mu.Unlock()
			f.svc.StartTime = start

			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
			require.NoError(t, err)
			assert.Equal(t, workforce.RetireResult{ID: w.ID, Changed: true, Outcome: workforce.OutcomeUnbound}, res)
			assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
			assert.Empty(t, f.h.params("pane.close"))
		})
	}
}

// TestRetireALostRowWithAShell: a lost row binds no pane and the poll never
// re-keys it (A35), so retire ends it unbound, closing nothing, even while
// the pane at its id still runs its shell: A35's rule, which NERD-5534 left
// as it was.
func TestRetireALostRowWithAShell(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	f.h.mu.Lock()
	liveHandoff(f.h, w.PaneID)
	f.h.mu.Unlock()
	f.markLost(t, w.ID)

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, Changed: true, Outcome: workforce.OutcomeUnbound}, res)
	assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
	assert.Empty(t, f.h.params("pane.close"))
}

// TestRetireAbandonsARotationAcrossALiveHandoff: a live handoff that renewed
// both the worker's pane and its pending handoff's replacement leaves the
// replacement the pane at its recorded id (A33 part 2a). Retiring it fails
// the handoff and closes it, though its shell has the worker's pid: the
// replacement is matched before any shell is.
func TestRetireAbandonsARotationAcrossALiveHandoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	next, h := replacing(t, f, w)
	f.h.mu.Lock()
	liveHandoff(f.h, w.PaneID)
	f.h.pane(next).terminal = "term_next_renewed"
	f.h.mu.Unlock()

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: next})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{PaneID: next, Changed: true, Outcome: workforce.OutcomeRetired, HandoffFailed: w.ID}, res)
	assert.Equal(t, "ended", handoffState(t, f, h))
	f.h.mu.Lock()
	assert.Nil(t, f.h.pane(next), "the replacement closed")
	assert.NotNil(t, f.h.pane(w.PaneID), "the worker's own pane stays")
	f.h.mu.Unlock()
	assert.True(t, f.row(t, w.ID).RetiredAt.IsZero(), "the worker stays live")
}

// TestRetireAPaneThatGoesWhileChecked: a pane that may hold the worker but
// closes between the layout retire reads and the read of its shell holds
// nothing: the worker is retired unbound, as when it had gone before.
func TestRetireAPaneThatGoesWhileChecked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	f.h.mu.Lock()
	liveHandoff(f.h, w.PaneID)
	f.h.onCall = func(method string) { // under the fake's lock
		if method == "pane.process_info" {
			f.h.dropPane(w.PaneID)
		}
	}
	f.h.mu.Unlock()

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, Changed: true, Outcome: workforce.OutcomeUnbound}, res)
	assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
}

// TestRetireEndsNothingWhenItsContextEndsMidCheck: the op's budget ending,
// or the daemon stopping, while retire reads a pane's shell ends nothing and
// closes nothing: the check fails before any write.
func TestRetireEndsNothingWhenItsContextEndsMidCheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	f.h.mu.Lock()
	liveHandoff(f.h, w.PaneID)
	f.h.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
		if r.Method == "pane.process_info" {
			cancel()
			time.Sleep(50 * time.Millisecond) // the caller sees its context end first
		}
		return next()
	})

	_, err := f.svc.Retire(ctx, workforce.RetireRequest{Target: w.ID})
	require.Error(t, err)
	assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
	assert.True(t, f.row(t, w.ID).RetiredAt.IsZero(), "still live")
	assert.Empty(t, f.h.params("pane.close"))
}

// TestRetireWithoutAStartTimeHookProvesByPid: a service given no StartTime
// hook proves a worker's shell on a pane by its pid alone, so a candidate
// with the row's shell pid is still the worker (ErrPaneChanged). The daemon
// always gives the poll's hook (TestTheWorkforceProvesShellsWithThePollsHook).
func TestRetireWithoutAStartTimeHookProvesByPid(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.svc.StartTime = nil
	w := shellWorker(t, f, "ha-coder-1", "coder")
	f.h.mu.Lock()
	liveHandoff(f.h, w.PaneID)
	f.h.mu.Unlock()

	_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.ErrorIs(t, err, workforce.ErrPaneChanged)
	assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
}

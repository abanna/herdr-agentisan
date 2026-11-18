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

// TestRetireTreatsAPaneItCannotReadAsUnproven: only a proof that a pane runs
// the worker's shell refuses retire. A pane herdr answers for without
// describing its shell (an error body, or no shell pid yet) proves nothing,
// so retire keeps A35's rule, as for a row with no shell, and a pane herdr
// never describes cannot block the worker's retirement. A call that never
// got herdr's answer, out of time, is ErrHerdr with nothing ended, for a
// retry.
func TestRetireTreatsAPaneItCannotReadAsUnproven(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reply   func(herdrtest.Reply) herdrtest.Reply
		timeout bool
	}{
		"herdr answers an error": {reply: func(herdrtest.Reply) herdrtest.Reply {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "internal_error", Message: "internal_error"}}
		}},
		"no shell pid yet": {reply: func(r herdrtest.Reply) herdrtest.Reply {
			r.Result.(map[string]any)["process_info"].(map[string]any)["shell_pid"] = 0
			return r
		}},
		"a call that times out": {timeout: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := shellWorker(t, f, "ha-coder-1", "coder")
			f.h.mu.Lock()
			liveHandoff(f.h, w.PaneID)
			f.h.mu.Unlock()
			wrapHerdr(t, f, func(r herdrtest.Request, next func() herdrtest.Reply) herdrtest.Reply {
				if r.Method != "pane.process_info" {
					return next()
				}
				if tc.timeout {
					time.Sleep(400 * time.Millisecond)
					return next()
				}
				return tc.reply(next())
			})
			if tc.timeout {
				f.svc.Herdr.Timeout = 100 * time.Millisecond
			}

			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
			if tc.timeout {
				require.ErrorIs(t, err, workforce.ErrHerdr)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status, "nothing ended")
				assert.True(t, f.row(t, w.ID).RetiredAt.IsZero(), "still live")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, workforce.RetireResult{ID: w.ID, Changed: true, Outcome: workforce.OutcomeUnbound}, res)
			assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
			assert.Empty(t, f.h.params("pane.close"), "nothing closed, as A35 has it")
		})
	}
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
	require.ErrorIs(t, err, workforce.ErrHerdr)
	require.ErrorIs(t, err, context.Canceled, "the check ended on the cancel, not on a proof")
	assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)
	assert.True(t, f.row(t, w.ID).RetiredAt.IsZero(), "still live")
	assert.Empty(t, f.h.params("pane.close"))
}

// TestRetireWithoutAStartTimeHookProvesNothing: a service given no
// StartTime hook proves no shell, as the team poll re-keys nothing without
// one (A35): a candidate with the row's shell pid is unproven, and retire
// keeps A35's rule. The daemon always gives the poll's hook
// (TestTheWorkforceProvesShellsWithThePollsHook).
func TestRetireWithoutAStartTimeHookProvesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.svc.StartTime = nil
	w := shellWorker(t, f, "ha-coder-1", "coder")
	f.h.mu.Lock()
	liveHandoff(f.h, w.PaneID)
	f.h.mu.Unlock()

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.RetireResult{ID: w.ID, Changed: true, Outcome: workforce.OutcomeUnbound}, res)
	assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
}

// TestRetireByPaneIDRefusesAPaneRekeyedMeanwhile (NERD-5534, pre-check M1):
// a pane named by its id that no row bound when retire read it, because a
// herdr live handoff had renewed its terminal, can be the worker's again by
// the time retire takes the workforce lock: the team poll re-keys the row
// onto it while retire waits for its handoff. Under the lock retire reads
// the rows again and refuses the pane as that worker's: ErrHandoffPending
// while its handoff is pending, else ErrPaneChanged. Nothing is closed, and
// the row stays live and bound.
func TestRetireByPaneIDRefusesAPaneRekeyedMeanwhile(t *testing.T) {
	t.Parallel()
	for name, pending := range map[string]bool{"no handoff pending": false, "its handoff pending": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := shellWorker(t, f, "ha-coder-1", "coder")
			var next string
			if pending {
				next, _ = replacing(t, f, w)
			}
			row := f.row(t, w.ID).Worker
			f.h.mu.Lock()
			liveHandoff(f.h, w.PaneID)
			lists := 0
			var rekeyed error
			f.h.onCall = func(method string) { // under the fake's lock
				if method == "pane.list" {
					if lists++; lists == 2 { // the wait's first poll
						f.h.pane(w.PaneID).tokens = map[string]string{team.HandoffKey: "⟳ READY"}
						rekeyed = f.st.RekeyWorker(context.Background(), row, w.PaneID, "term_renewed")
					}
				}
			}
			f.h.mu.Unlock()

			res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.PaneID, AfterHandoff: true})
			f.h.mu.Lock()
			assert.NoError(t, rekeyed)
			f.h.mu.Unlock()
			if pending {
				assert.ErrorIs(t, err, workforce.ErrHandoffPending, "answered %+v", res)
				if err != nil {
					assert.Contains(t, err.Error(), next, "it names the replacement")
				}
			} else {
				assert.ErrorIs(t, err, workforce.ErrPaneChanged, "answered %+v", res)
			}
			assert.Empty(t, f.h.params("pane.close"), "the worker's pane is not closed")
			now := f.row(t, w.ID)
			assert.Equal(t, store.StatusActive, now.Status)
			assert.True(t, now.RetiredAt.IsZero(), "still live")
			assert.Equal(t, "term_renewed", now.TerminalID, "bound to the pane retire was asked to close")
		})
	}
}

// TestRetireEndsTheRowOfAPaneItClosed (pre-check N1): once retire has closed
// the pane pane.get proved showed the worker's terminal, the worker went
// with it. A re-key landing between the proof and the row's write still
// retires the row, rather than leave it bound to a closed pane for the poll
// to mark lost.
func TestRetireEndsTheRowOfAPaneItClosed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := shellWorker(t, f, "ha-coder-1", "coder")
	row := f.row(t, w.ID).Worker
	var rekeyed error
	f.h.onCall = func(method string) { // under the fake's lock
		if method == "pane.close" {
			rekeyed = f.st.RekeyWorker(context.Background(), row, w.PaneID, "term_renewed")
		}
	}

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	f.h.mu.Lock()
	require.NoError(t, rekeyed)
	f.h.mu.Unlock()
	assert.Equal(t, workforce.RetireResult{ID: w.ID, PaneID: w.PaneID, Changed: true, Outcome: workforce.OutcomeRetired}, res)
	assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
}

// TestRetireALiveHandoffOfARowWithNoShell (pre-check N4): what NERD-5534
// leaves to A35's rule. A row bound less than a poll before a herdr live
// handoff has no shell, so nothing proves which pane runs its worker: retire
// by its logical id ends it unbound with nothing closed, and by the renewed
// pane's id closes that pane as one no row binds, the row left live for the
// poll to mark lost. Covering it needs a rule A35 does not have.
func TestRetireALiveHandoffOfARowWithNoShell(t *testing.T) {
	t.Parallel()
	t.Run("by its logical id", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		f.h.mu.Lock()
		liveHandoff(f.h, w.PaneID)
		f.h.mu.Unlock()

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{ID: w.ID, Changed: true, Outcome: workforce.OutcomeUnbound}, res)
		assert.Equal(t, store.StatusRetired, f.row(t, w.ID).Status)
		f.h.mu.Lock()
		assert.NotNil(t, f.h.pane(w.PaneID), "the worker's pane is still open")
		f.h.mu.Unlock()
	})
	t.Run("by the renewed pane's id", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		f.h.mu.Lock()
		liveHandoff(f.h, w.PaneID)
		f.h.mu.Unlock()

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.PaneID})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{PaneID: w.PaneID, Changed: true, Outcome: workforce.OutcomeRetired}, res)
		assert.Equal(t, []map[string]any{{"pane_id": w.PaneID}}, f.h.params("pane.close"))
		assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status, "left live, for the poll to mark lost")
	})
}

// TestRetireJudgesOnlyItsOwnTarget (pre-check S4): another worker's live
// handoff the poll has not re-keyed refuses no other retire. A worker whose
// pane really went is still retired unbound, and a pane no row binds still
// closes, while a second worker's pane runs that worker's shell on a renewed
// terminal: unkeyed judges only the target's row by logical id, and only the
// named pane by pane id.
func TestRetireJudgesOnlyItsOwnTarget(t *testing.T) {
	t.Parallel()
	t.Run("another worker, its pane gone", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		unkeyed := shellWorker(t, f, "ha-coder-1", "coder")
		gone := shellWorker(t, f, "ha-coder-2", "coder")
		f.h.mu.Lock()
		liveHandoff(f.h, unkeyed.PaneID)
		f.h.dropPane(gone.PaneID)
		f.h.mu.Unlock()

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: gone.ID})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{ID: gone.ID, Changed: true, Outcome: workforce.OutcomeUnbound}, res)
		assert.Equal(t, store.StatusRetired, f.row(t, gone.ID).Status)
		assert.Equal(t, store.StatusActive, f.row(t, unkeyed.ID).Status, "the other worker is untouched")
	})
	t.Run("a pane no row binds", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, teamConfig)
		unkeyed := shellWorker(t, f, "ha-coder-1", "coder")
		stranger := f.h.addTab(f.ws, "scratch", "claude", "idle")
		f.h.mu.Lock()
		liveHandoff(f.h, unkeyed.PaneID)
		f.h.mu.Unlock()

		res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: stranger})
		require.NoError(t, err)
		assert.Equal(t, workforce.RetireResult{PaneID: stranger, Changed: true, Outcome: workforce.OutcomeRetired}, res)
		assert.Equal(t, []map[string]any{{"pane_id": stranger}}, f.h.params("pane.close"), "only the stranger closes")
		assert.Equal(t, store.StatusActive, f.row(t, unkeyed.ID).Status, "the other worker is untouched")
	})
}

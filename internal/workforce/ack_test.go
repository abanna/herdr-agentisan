package workforce_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// rotating is w mid-rotation, as spawn --replaces (2b-2) will leave it: a
// replacement pane in w's tab running claude under the herdr name
// <id>-next, and w's handoff onto it pending. It returns the pane and the
// handoff.
func rotating(t *testing.T, f *fixture, w worker) (string, int64) {
	t.Helper()
	next, h := replacing(t, f, w)
	f.h.setPane(next, func(p *fakePane) { p.name, p.ready = w.ID+"-next", true })
	return next, h
}

// stated gives pane the item and stage tokens a turn's hooks write; ""
// leaves a key out.
func (f *fixture) stated(pane, item, stage string) {
	f.h.setPane(pane, func(p *fakePane) {
		p.tokens = map[string]string{}
		if item != "" {
			p.tokens["item"] = item
		}
		if stage != "" {
			p.tokens["stage"], p.tokens["stage_at"] = stage, "2026-10-23T05:00:00Z"
		}
	})
}

// nameOf is the herdr agent name pane's agent has now.
func (f *fixture) nameOf(pane string) string {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	return f.h.pane(pane).name
}

// steps records the handoff steps a service reports.
type steps struct {
	mu  sync.Mutex
	got []workforce.HandoffStep
}

func (s *steps) record(_ context.Context, h workforce.HandoffStep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, h)
}

func (s *steps) all() []workforce.HandoffStep {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]workforce.HandoffStep(nil), s.got...)
}

// TestHandoffAckCompletesTheRotation is D7 step 3 with I1 and I2: once the
// replacement re-reports the old pane's item and stage, its ack moves the
// names (the old agent's cleared, then the new one's to the id) before it
// completes the handoff, which binds the same worker, at the same
// generation, to the new pane and releases its inbox there. The daemon
// writes no item or stage of its own (A6), and reports the step.
func TestHandoffAckCompletesTheRotation(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	rec := &steps{}
	f.svc.HandoffSteps = rec.record
	w := f.worker(t, "ha-coder-1", "coder")
	next, h := rotating(t, f, w)
	before := f.row(t, w.ID)
	held, err := f.st.Send(t.Context(), store.Address{Kind: store.KindWorker, ID: w.ID}, store.Address{Kind: store.KindBoss, ID: "ha-boss"}, "during the handoff", time.Now())
	require.NoError(t, err)
	f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
	f.stated(next, "NERD-5261", "BOSS: handoff written")

	res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
	require.NoError(t, err)
	assert.Equal(t, workforce.HandoffAckResult{
		ID: w.ID, Project: "herdr-agentisan", Group: "coder", OldPane: w.PaneID, NewPane: next, HandoffID: h,
		Item: "NERD-5261", Stage: "BOSS: handoff written", Old: workforce.OldMatched, Changed: true,
	}, res)

	after := f.row(t, w.ID)
	assert.Equal(t, before.Generation, after.Generation, "the same worker (I1)")
	assert.Equal(t, next, after.PaneID)
	f.h.mu.Lock()
	terminal := f.h.pane(next).terminal
	f.h.mu.Unlock()
	assert.Equal(t, terminal, after.TerminalID)
	assert.Equal(t, w.ID, f.nameOf(next), "the id's herdr name moved to the replacement")
	assert.Empty(t, f.nameOf(w.PaneID), "and off the old agent")
	assert.Equal(t, []map[string]any{{"target": w.PaneID}, {"target": next, "name": w.ID}}, f.h.params("agent.rename"), "clear first, then rename")
	assert.Empty(t, f.h.params("pane.report_metadata"), "the daemon writes no item or stage (A6)")
	assert.Equal(t, "ended", handoffState(t, f, h))
	m, ok, err := f.st.Claim(t.Context(), store.Address{Kind: store.KindWorker, ID: w.ID}, time.Now())
	require.NoError(t, err)
	require.True(t, ok, "the inbox is released")
	assert.Equal(t, held.ID, m.ID)
	assert.Equal(t, []workforce.HandoffStep{{
		ID: w.ID, Project: "herdr-agentisan", Group: "coder", PaneID: next, OldPane: w.PaneID, Step: workforce.StepCompleted, Old: workforce.OldMatched,
	}}, rec.all())
}

// TestHandoffAckGate is I2's gate (Q2 a): the replacement must report an item
// and a stage, and equal the old pane's while the old pane reports both;
// any other ack is refused (ErrStageMismatch, naming both pairs) with
// nothing renamed and the handoff pending. An old pane whose pair cannot be
// read, gone or missing a key, is no gate: the ack completes, its answer
// saying so.
func TestHandoffAckGate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		oldItem, oldStage, newItem, newStage string
	}{
		"another stage":    {"NERD-5261", "BOSS: handoff written", "NERD-5261", "STATUS: reading"},
		"another item":     {"NERD-5261", "BOSS: handoff written", "NERD-5262", "BOSS: handoff written"},
		"no new item":      {"NERD-5261", "BOSS: handoff written", "", "BOSS: handoff written"},
		"no new stage":     {"NERD-5261", "BOSS: handoff written", "NERD-5261", ""},
		"old unread, none": {"", "", "", ""},
		"old unread, item": {"NERD-5261", "", "NERD-5261", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			next, h := rotating(t, f, w)
			f.stated(w.PaneID, tc.oldItem, tc.oldStage)
			f.stated(next, tc.newItem, tc.newStage)

			_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
			require.ErrorIs(t, err, workforce.ErrStageMismatch)
			assert.Contains(t, err.Error(), `new item "`+tc.newItem+`", stage "`+tc.newStage+`"`)
			if tc.oldItem != "" && tc.oldStage != "" {
				assert.Contains(t, err.Error(), `old item "`+tc.oldItem+`", stage "`+tc.oldStage+`"`)
			}
			assert.Empty(t, f.h.params("agent.rename"), "nothing renamed")
			assert.Equal(t, "pending", handoffState(t, f, h))
			assert.Equal(t, w.PaneID, f.row(t, w.ID).PaneID)
		})
	}

	t.Run("old unread", func(t *testing.T) {
		t.Parallel()
		for name, old := range map[string]func(*fixture, worker){
			"a key missing": func(f *fixture, w worker) { f.stated(w.PaneID, "NERD-5261", "") },
			"its pane gone": func(f *fixture, w worker) {
				f.h.mu.Lock()
				f.h.dropPane(w.PaneID)
				f.h.mu.Unlock()
			},
		} {
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			next, _ := rotating(t, f, w)
			old(f, w)
			f.stated(next, "NERD-5262", "STATUS: re-reported")

			res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
			require.NoError(t, err, name)
			assert.Equal(t, workforce.OldUnread, res.Old, name)
			assert.Equal(t, [2]string{"NERD-5262", "STATUS: re-reported"}, [2]string{res.Item, res.Stage}, name)
			assert.Equal(t, next, f.row(t, w.ID).PaneID, name)
			assert.Equal(t, w.ID, f.nameOf(next), name)
		}
	})
}

// TestHandoffAckFindsTheReplacement: with no target the ack is the caller's
// confirmed pane's; with none confirmed it is refused, never guessed from
// HERDR_AGENTISAN_WORKER, which the old pane and the new one share. The old
// pane acking by mistake is no replacement and is told which pane is; a pane
// no pending handoff names is ErrNoHandoff, one herdr lists nowhere
// ErrUnknownWorker, and a replacement herdr lists nowhere any more
// ErrHandoffChanged.
func TestHandoffAckFindsTheReplacement(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	other := f.worker(t, "ha-coder-2", "coder")
	next, h := rotating(t, f, w)
	f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
	f.stated(next, "NERD-5261", "BOSS: handoff written")

	_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{})
	require.ErrorIs(t, err, workforce.ErrBadRequest)
	_, err = f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{CallerPane: w.PaneID})
	require.ErrorIs(t, err, workforce.ErrNoHandoff)
	assert.Contains(t, err.Error(), next, "names the replacement")
	_, err = f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: other.PaneID})
	require.ErrorIs(t, err, workforce.ErrNoHandoff, "a worker no handoff completed onto, its agent named its id")
	_, err = f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: "w10:p99"})
	require.ErrorIs(t, err, workforce.ErrUnknownWorker)
	assert.Empty(t, f.h.params("agent.rename"))
	assert.Equal(t, "pending", handoffState(t, f, h))

	res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{CallerPane: next})
	require.NoError(t, err)
	assert.True(t, res.Changed)

	g := newFixture(t, teamConfig)
	v := g.worker(t, "ha-coder-1", "coder")
	gone, h2 := rotating(t, g, v)
	g.h.mu.Lock()
	g.h.dropPane(gone)
	g.h.mu.Unlock()
	_, err = g.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: gone})
	require.ErrorIs(t, err, workforce.ErrHandoffChanged)
	assert.Equal(t, "pending", handoffState(t, g, h2), "the boss abandons it with worker retire")
}

// TestHandoffAckAgain (Q4): an ack repeated once the handoff completed, as a
// crash after the complete leaves it, changes nothing and says so: the pane
// is the worker's bound pane and its agent has the id's name. The old pane
// is no worker's any more: ErrNoHandoff.
func TestHandoffAckAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	next, _ := rotating(t, f, w)
	f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
	f.stated(next, "NERD-5261", "BOSS: handoff written")
	_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
	require.NoError(t, err)
	renames := len(f.h.params("agent.rename"))

	res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
	require.NoError(t, err)
	assert.Equal(t, workforce.HandoffAckResult{ID: w.ID, Project: "herdr-agentisan", Group: "coder", NewPane: next}, res)
	assert.Len(t, f.h.params("agent.rename"), renames, "nothing renamed again")
	_, err = f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: w.PaneID})
	require.ErrorIs(t, err, workforce.ErrNoHandoff)

	f.h.setPane(next, func(p *fakePane) { p.name = "" })
	_, err = f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
	require.ErrorIs(t, err, workforce.ErrNoHandoff, "a bound pane whose agent lacks the id's name is no completed ack")
}

// TestHandoffAckAfterALiveHandoff is UQ3 (a): a herdr live handoff renews
// the replacement's terminal and keeps its pane id. The ack completes onto
// the renewed terminal only while the pane's agent still has the
// replacement's herdr name, <id>-next, or the id's after a crash past the
// rename; any other is ErrHandoffChanged. A pane id whose recorded terminal
// herdr lists elsewhere was reissued, and is no replacement.
func TestHandoffAckAfterALiveHandoff(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		agentName string
		want      error
	}{
		"still <id>-next":  {"ha-coder-1-next", nil},
		"the id's already": {"ha-coder-1", nil},
		"another name":     {"ha-qa-1", workforce.ErrHandoffChanged},
		"its name dropped": {"", workforce.ErrHandoffChanged},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			next, h := rotating(t, f, w)
			if tc.agentName == w.ID {
				f.h.setPane(w.PaneID, func(p *fakePane) { p.name = "" })
			}
			f.h.setPane(next, func(p *fakePane) { p.terminal, p.name = "term_renewed", tc.agentName })
			f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
			f.stated(next, "NERD-5261", "BOSS: handoff written")

			_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Equal(t, "pending", handoffState(t, f, h))
				assert.Empty(t, f.h.params("agent.rename"))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "term_renewed", f.row(t, w.ID).TerminalID, "the terminal the pane shows now")
			assert.Equal(t, w.ID, f.nameOf(next))
		})
	}

	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	next, h := rotating(t, f, w)
	f.h.mu.Lock()
	recorded := f.h.pane(next).terminal
	f.h.pane(next).terminal = "term_reissued"
	f.h.mu.Unlock()
	moved := f.h.addPane(w.PaneID, "claude", "idle")
	f.h.setPane(moved, func(p *fakePane) { p.terminal = recorded })
	_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
	require.ErrorIs(t, err, workforce.ErrNoHandoff)
	assert.Equal(t, "pending", handoffState(t, f, h))
}

// TestHandoffAckProvesThePane: the ack reads the replacement again with
// pane.get, whose tokens the gate reads; a pane that shows another terminal
// by then changed under the ack: ErrPaneChanged, nothing renamed. A pane
// with no agent is no replacement to rename: ErrNotAWorker.
func TestHandoffAckProvesThePane(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	next, h := rotating(t, f, w)
	f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
	f.stated(next, "NERD-5261", "BOSS: handoff written")
	f.h.onCall = func(method string) {
		if method == "pane.get" {
			f.h.pane(next).terminal = "term_since"
		}
	}
	_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
	require.ErrorIs(t, err, workforce.ErrPaneChanged)
	assert.Empty(t, f.h.params("agent.rename"))
	assert.Equal(t, "pending", handoffState(t, f, h))

	g := newFixture(t, teamConfig)
	v := g.worker(t, "ha-coder-1", "coder")
	shell, _ := replacing(t, g, v)
	g.h.setPane(shell, func(p *fakePane) { p.agent, p.name = "", "" })
	g.stated(shell, "NERD-5261", "BOSS: handoff written")
	_, err = g.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: shell})
	require.ErrorIs(t, err, workforce.ErrNotAWorker)
}

// TestHandoffAckGatesOnTheFreshRead (QA #140 F1): I2's gate reads the
// replacement's item and stage from the pane.get that proves its terminal,
// never from the pane.list before it. A stage that changes between the two
// reads is refused if the fresh one differs, and passes if the fresh one
// matches.
func TestHandoffAckGatesOnTheFreshRead(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		listed, fresh string
		want          error
	}{
		"the fresh stage differs": {"BOSS: handoff written", "STATUS: reading another task", workforce.ErrStageMismatch},
		"the fresh stage matches": {"STATUS: reading the handoff", "BOSS: handoff written", nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			next, h := rotating(t, f, w)
			f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
			f.stated(next, "NERD-5261", tc.listed)
			f.h.onCall = func(method string) {
				if method == "pane.get" {
					f.h.pane(next).tokens["stage"] = tc.fresh
				}
			}

			res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Contains(t, err.Error(), `new item "NERD-5261", stage "`+tc.fresh+`"`)
				assert.Empty(t, f.h.params("agent.rename"), "nothing renamed")
				assert.Equal(t, "pending", handoffState(t, f, h))
				assert.Equal(t, w.PaneID, f.row(t, w.ID).PaneID)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.fresh, res.Stage)
			assert.Equal(t, workforce.OldMatched, res.Old)
			assert.Equal(t, "ended", handoffState(t, f, h))
		})
	}
}

// TestHandoffAckSerializesRepeatedAcks (QA #140 F2): the whole ack runs
// under the workforce lock, so two acks of one replacement at once complete
// the handoff once: one changes it, the other finds it completed and changes
// nothing, and the names move once.
func TestHandoffAckSerializesRepeatedAcks(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	rec := &steps{}
	f.svc.HandoffSteps = rec.record
	w := f.worker(t, "ha-coder-1", "coder")
	next, h := rotating(t, f, w)
	f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
	f.stated(next, "NERD-5261", "BOSS: handoff written")

	type answer struct {
		res workforce.HandoffAckResult
		err error
	}
	answers, start := make(chan answer, 2), make(chan struct{})
	for range 2 {
		go func() {
			<-start
			res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
			answers <- answer{res, err}
		}()
	}
	close(start)
	changed := 0
	for range 2 {
		a := <-answers
		require.NoError(t, a.err)
		assert.Equal(t, w.ID, a.res.ID)
		assert.Equal(t, next, a.res.NewPane)
		if a.res.Changed {
			changed++
		}
	}
	assert.Equal(t, 1, changed, "one ack completes the handoff, the other finds it completed")
	assert.Len(t, rec.all(), 1, "one completion reported")
	assert.Len(t, f.h.params("agent.rename"), 2, "the names moved once: the clear, then the rename")
	assert.Equal(t, "ended", handoffState(t, f, h))
}

// TestHandoffAckMovesTheNamesSafely: a replacement's terminal some live row
// binds, or the id's herdr name held by a third pane, is refused before any
// rename (ErrPaneChanged), so a retry is never stuck past it; a rename that fails
// after the clear puts the old name back and leaves the handoff pending
// (ErrHerdr); an ack run again after a crash past the rename completes
// without renaming; and a clear whose agent went meanwhile carries on.
func TestHandoffAckMovesTheNamesSafely(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*fixture, worker, string, int64) {
		t.Helper()
		f := newFixture(t, teamConfig)
		w := f.worker(t, "ha-coder-1", "coder")
		next, h := rotating(t, f, w)
		f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
		f.stated(next, "NERD-5261", "BOSS: handoff written")
		return f, w, next, h
	}

	t.Run("a live row binds the replacement's terminal", func(t *testing.T) {
		t.Parallel()
		f, _, next, h := setup(t)
		f.bindRow(t, "ha-coder-2", "coder", next)
		_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
		require.ErrorIs(t, err, workforce.ErrPaneChanged, "refused before the names move, not by the store after")
		assert.Contains(t, err.Error(), "ha-coder-2")
		assert.Empty(t, f.h.params("agent.rename"))
		assert.Equal(t, "pending", handoffState(t, f, h))
	})

	t.Run("a third pane has the name", func(t *testing.T) {
		t.Parallel()
		f, w, next, h := setup(t)
		third := f.h.addTab(f.ws, "scratch", "claude", "idle")
		f.h.setPane(w.PaneID, func(p *fakePane) { p.name = "" })
		f.h.setPane(third, func(p *fakePane) { p.name = w.ID })
		_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
		require.ErrorIs(t, err, workforce.ErrPaneChanged)
		assert.Contains(t, err.Error(), third)
		assert.Empty(t, f.h.params("agent.rename"))
		assert.Equal(t, "pending", handoffState(t, f, h))
	})

	t.Run("the rename fails after the clear", func(t *testing.T) {
		t.Parallel()
		f, w, next, h := setup(t)
		// The clear succeeds, the rename fails, the restore succeeds; onCall
		// sees the count before the call it runs for.
		f.h.fail["agent.rename"], f.h.failAfter["agent.rename"] = "agent_launch_pending", 1
		f.h.onCall = func(method string) {
			if method == "agent.rename" && f.h.calls["agent.rename"] == 2 {
				delete(f.h.fail, "agent.rename")
			}
		}
		_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
		require.ErrorIs(t, err, workforce.ErrHerdr)
		assert.Equal(t, w.ID, f.nameOf(w.PaneID), "the old name put back")
		assert.Equal(t, w.ID+"-next", f.nameOf(next))
		assert.Equal(t, []map[string]any{{"target": w.PaneID}, {"target": next, "name": w.ID}, {"target": w.PaneID, "name": w.ID}}, f.h.params("agent.rename"))
		assert.Equal(t, "pending", handoffState(t, f, h))
		assert.Equal(t, w.PaneID, f.row(t, w.ID).PaneID)
	})

	t.Run("again after a crash past the rename", func(t *testing.T) {
		t.Parallel()
		f, w, next, h := setup(t)
		f.h.setPane(w.PaneID, func(p *fakePane) { p.name = "" })
		f.h.setPane(next, func(p *fakePane) { p.name = w.ID })
		res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
		require.NoError(t, err)
		assert.True(t, res.Changed)
		assert.Empty(t, f.h.params("agent.rename"), "the names moved already")
		assert.Equal(t, "ended", handoffState(t, f, h))
	})

	t.Run("the old agent goes during the clear", func(t *testing.T) {
		t.Parallel()
		f, w, next, h := setup(t)
		f.h.onCall = func(method string) {
			if method == "agent.rename" && f.h.calls["agent.rename"] == 0 {
				f.h.dropPane(w.PaneID)
			}
		}
		_, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
		require.NoError(t, err)
		assert.Equal(t, w.ID, f.nameOf(next))
		assert.Equal(t, "ended", handoffState(t, f, h))
	})
}

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// bound is the pane and generation the live workers row binds id to.
func bound(t *testing.T, s *store.Store, id string) (string, int64) {
	t.Helper()
	ws, err := s.Workers(t.Context())
	require.NoError(t, err)
	for _, w := range ws {
		if w.LogicalID == id {
			return w.PaneID, w.Generation
		}
	}
	t.Fatalf("%s is not bound", id)
	return "", 0
}

func handoffState(t *testing.T, s *store.Store, id int64) string {
	t.Helper()
	var state string
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT state FROM handoffs WHERE id = ?", id).Scan(&state))
	return state
}

// TestThreeMessagesDuringAHandoffArriveOnce is D8's test, with I3: given
// three messages queued during a handoff, when the replacement session
// binds, each is delivered exactly once. While the handoff is pending the
// worker's inbox is held: nothing is claimed for the old session. Completing
// it rebinds the logical id to the new pane and releases the inbox in one
// transaction, keeping the generation, so the claims that follow take every
// message once, in order, and none goes back to the boss.
func TestThreeMessagesDuringAHandoffArriveOnce(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "ha-coder-2", "w14:p2", 4)

	h, err := s.BeginHandoff(ctx, "ha-coder-2", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	var sent []string
	for _, body := range []string{"one", "two", "three"} {
		sent = append(sent, send(t, s, worker("ha-coder-2"), boss, body).ID)
	}
	noClaim(t, s, worker("ha-coder-2"), "the inbox is held while the handoff is pending")
	pane, _ := bound(t, s, "ha-coder-2")
	assert.Equal(t, "w14:p2", pane, "still the old session until the handoff completes")

	require.NoError(t, s.CompleteHandoff(ctx, h, t1.Add(time.Second)))
	pane, gen := bound(t, s, "ha-coder-2")
	assert.Equal(t, "w14:p7", pane, "bound to the replacement")
	assert.Equal(t, int64(4), gen, "a handoff keeps the generation (I1)")
	assert.Equal(t, "acknowledged", handoffState(t, s, h))

	delivered := map[string]int{}
	var order []string
	for {
		m, ok, err := s.Claim(ctx, worker("ha-coder-2"), t1)
		require.NoError(t, err)
		if !ok {
			break
		}
		delivered[m.ID]++
		order = append(order, m.ID)
		require.NoError(t, s.Delivered(ctx, m.ID, t1))
	}
	assert.Equal(t, sent, order, "each once, in the order sent")
	for _, id := range sent {
		assert.Equal(t, 1, delivered[id], "%s delivered exactly once", id)
	}
	returned, err := s.ReturnToBoss(ctx, "ha-coder-2", "agentisan", boss, t1)
	require.NoError(t, err)
	assert.Empty(t, returned, "nothing queued for the binding goes to the boss")
}

// TestHandoffWithAMessageInFlight: a message already dispatching when the
// handoff begins finishes into the old session, which is still running
// then; nothing more is claimed until the handoff ends.
func TestHandoffWithAMessageInFlight(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 0)
	first := send(t, s, worker("w"), boss, "first")
	second := send(t, s, worker("w"), boss, "second")
	claim(t, s, worker("w"))

	h, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	require.NoError(t, s.Delivered(ctx, first.ID, t1), "the dispatch in flight lands")
	noClaim(t, s, worker("w"), "held until the handoff ends")
	require.NoError(t, s.CompleteHandoff(ctx, h, t1))
	assert.Equal(t, second.ID, claim(t, s, worker("w")).ID)
}

// TestFailHandoffKeepsTheOldSession: a failed handoff leaves the worker
// bound where it was and releases its inbox there.
func TestFailHandoffKeepsTheOldSession(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 1)
	h, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	m := send(t, s, worker("w"), boss, "hi")
	noClaim(t, s, worker("w"), "held")

	require.NoError(t, s.FailHandoff(ctx, h, t1))
	pane, gen := bound(t, s, "w")
	assert.Equal(t, "w14:p2", pane)
	assert.Equal(t, int64(1), gen)
	assert.Equal(t, "failed", handoffState(t, s, h))
	assert.Equal(t, m.ID, claim(t, s, worker("w")).ID, "released to the old session")

	h2, err := s.BeginHandoff(ctx, "w", "w14:p8", terminal("w14:p8"), t1)
	require.NoError(t, err, "a new handoff may begin once the last one ended")
	assert.NotEqual(t, h, h2)
}

// TestCompleteHandoffIsOneTransaction: a handoff that cannot be ended leaves
// the worker on its old pane with its inbox held (A2, I3).
func TestCompleteHandoffIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "w", "w14:p2", 0)
	h, err := s.BeginHandoff(t.Context(), "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	send(t, s, worker("w"), boss, "held")
	_, err = store.DB(s).ExecContext(t.Context(),
		"CREATE TEMP TRIGGER refuse_ack BEFORE UPDATE OF state ON handoffs WHEN NEW.state = 'acknowledged' BEGIN SELECT RAISE(ABORT, 'ack refused'); END")
	require.NoError(t, err)
	require.ErrorIs(t, s.CompleteHandoff(t.Context(), h, t1), store.ErrStore)
	pane, _ := bound(t, s, "w")
	assert.Equal(t, "w14:p2", pane, "not rebound")
	assert.Equal(t, "pending", handoffState(t, s, h))
	noClaim(t, s, worker("w"), "still held")
}

// TestHandoffRefusals: a handoff needs a live worker and a new pane and
// terminal, one at a time; completing or failing needs a pending handoff, and completing needs
// the worker still on the old pane. A refused complete leaves the handoff
// pending, for the caller to fail.
func TestHandoffRefusals(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 0)
	seedWorker(t, s, "gone", "w14:p3", 0)
	retire(t, s, "gone")

	for name, tc := range map[string]struct {
		id, pane string
		at       time.Time
		want     error
	}{
		"no such worker":            {id: "nobody", pane: "w14:p9", want: store.ErrNotBound},
		"a retired worker":          {id: "gone", pane: "w14:p9", want: store.ErrNotBound},
		"the pane it is on":         {id: "w", pane: "w14:p2", want: store.ErrInvalid},
		"no new pane":               {id: "w", pane: "", want: store.ErrInvalid},
		"a control byte":            {id: "w", pane: "w14:\x1bp9", want: store.ErrInvalid},
		"a NUL in the pane":         {id: "w", pane: "w14:\x00p9", want: store.ErrInvalid},
		"invalid UTF-8 in the pane": {id: "w", pane: "w14:\xffp9", want: store.ErrInvalid},
		"the zero time":             {id: "w", pane: "w14:p9", at: time.Time{}, want: store.ErrInvalid},
		"an empty logical id":       {id: "", pane: "w14:p9", want: store.ErrInvalid},
	} {
		at := tc.at
		if name != "the zero time" {
			at = t1
		}
		_, err := s.BeginHandoff(ctx, tc.id, tc.pane, terminal(tc.pane), at)
		require.ErrorIs(t, err, tc.want, name)
	}

	_, err := s.BeginHandoff(ctx, "w", "w14:p9", terminal("w14:p2"), t1)
	require.ErrorIs(t, err, store.ErrInvalid, "the terminal it is on")
	_, err = s.BeginHandoff(ctx, "w", "w14:p9", "", t1)
	require.ErrorIs(t, err, store.ErrInvalid, "no new terminal")

	h, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	_, err = s.BeginHandoff(ctx, "w", "w14:p8", terminal("w14:p8"), t1)
	require.ErrorIs(t, err, store.ErrHandoffPending, "one at a time")

	require.ErrorIs(t, s.CompleteHandoff(ctx, h+100, t1), store.ErrNoHandoff)
	require.ErrorIs(t, s.CompleteHandoff(ctx, -1, t1), store.ErrNoHandoff)
	require.ErrorIs(t, s.FailHandoff(ctx, h+100, t1), store.ErrNoHandoff)
	require.ErrorIs(t, s.CompleteHandoff(ctx, h, time.Time{}), store.ErrInvalid)

	retire(t, s, "w")
	require.ErrorIs(t, s.CompleteHandoff(ctx, h, t1), store.ErrNotBound, "the worker was retired meanwhile")
	assert.Equal(t, "pending", handoffState(t, s, h), "left for the caller to fail")
	require.NoError(t, s.FailHandoff(ctx, h, t1))
	require.ErrorIs(t, s.FailHandoff(ctx, h, t1), store.ErrNoHandoff, "ended once")
	require.ErrorIs(t, s.CompleteHandoff(ctx, h, t1), store.ErrNoHandoff)
}

// TestCancelledContext: a call whose context has ended (the daemon
// stopping) fails, and changes nothing: a transaction cut short is rolled
// back whole.
func TestCancelledContext(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "w", "w14:p2", 0)
	h, err := s.BeginHandoff(t.Context(), "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	m := send(t, s, boss, pane, "hi")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = s.Send(ctx, boss, pane, "lost", t1)
	require.ErrorIs(t, err, store.ErrStore)
	_, _, err = s.Claim(ctx, boss, t1)
	require.ErrorIs(t, err, store.ErrStore)
	require.ErrorIs(t, s.CompleteHandoff(ctx, h, t1), store.ErrStore)
	_, err = s.RecoverDispatching(ctx, "x", t1)
	require.ErrorIs(t, err, store.ErrStore)

	assert.Equal(t, "pending", handoffState(t, s, h))
	pane, _ := bound(t, s, "w")
	assert.Equal(t, "w14:p2", pane)
	assert.Equal(t, m.ID, claim(t, s, boss).ID, "only the message sent before")
	require.NoError(t, s.Delivered(t.Context(), m.ID, t1))
	noClaim(t, s, boss, "nothing else was queued")
}

// TestPendingHandoffs: the read lists every pending handoff, oldest first,
// with the binding each completes onto, and leaves out the ones that
// completed or failed. A handoff begun before migration 0003 has no
// generation and no terminal, which it reads as -1 and "".
func TestPendingHandoffs(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	got, err := s.PendingHandoffs(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got, "none is an empty list, not null")

	for _, w := range []struct {
		id, pane string
		gen      int64
	}{{"w", "w14:p2", 4}, {"v", "w14:p3", 0}, {"u", "w14:p4", 1}, {"x", "w14:p5", 2}} {
		seedWorker(t, s, w.id, w.pane, w.gen)
	}
	acked, err := s.BeginHandoff(ctx, "u", "w14:p8", terminal("w14:p8"), t1)
	require.NoError(t, err)
	require.NoError(t, s.CompleteHandoff(ctx, acked, t1))
	failed, err := s.BeginHandoff(ctx, "v", "w14:p9", terminal("w14:p9"), t1)
	require.NoError(t, err)
	require.NoError(t, s.FailHandoff(ctx, failed, t1))
	first, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	second, err := s.BeginHandoff(ctx, "x", "w14:pA", "term_next", t1.Add(time.Minute))
	require.NoError(t, err)
	_, err = store.DB(s).ExecContext(ctx,
		"INSERT INTO handoffs (logical_id, old_pane, new_pane, created_at, updated_at) VALUES ('v', 'w14:p3', 'w14:pB', 5, 5)")
	require.NoError(t, err, "a handoff as migration 0003 found it")

	got, err = s.PendingHandoffs(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, store.Handoff{ID: first, LogicalID: "w", Generation: 4, OldPane: "w14:p2", NewPane: "w14:p7", NewTerminal: "term_w14:p7", CreatedAt: t1, Project: "agentisan", Group: "coders"}, got[0])
	assert.Equal(t, store.Handoff{ID: second, LogicalID: "x", Generation: 2, OldPane: "w14:p5", NewPane: "w14:pA", NewTerminal: "term_next", CreatedAt: t1.Add(time.Minute), Project: "agentisan", Group: "coders"}, got[1])
	assert.Equal(t, store.Handoff{ID: second + 1, LogicalID: "v", Generation: -1, OldPane: "w14:p3", NewPane: "w14:pB", CreatedAt: time.Unix(0, 5).UTC()}, got[2])

	require.NoError(t, s.Close())
	_, err = s.PendingHandoffs(context.Background())
	require.ErrorIs(t, err, store.ErrStore)
}

// TestBeginHandoffRefusesAStartingWorker: a worker whose agent is not up yet
// starts no handoff (ErrWorkerStarting), and nothing is recorded, so spawn's
// failed write never meets a pending handoff; once active it can hand off.
func TestBeginHandoffRefusesAStartingWorker(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	w := store.Worker{LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w14:p2", TerminalID: terminal("w14:p2")}
	gen, err := s.BindWorker(ctx, w, t1)
	require.NoError(t, err)

	_, err = s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.ErrorIs(t, err, store.ErrWorkerStarting)
	pending, err := s.PendingHandoffs(ctx)
	require.NoError(t, err)
	assert.Empty(t, pending, "nothing recorded")

	w.Generation = gen
	require.NoError(t, s.SetWorkerStatus(ctx, w, store.StatusActive, t1))
	_, err = s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
}

// TestCompleteHandoffOnto: the handoff ack completes onto the terminal it
// proved the replacement shows (A33 part 2b-1), which a herdr live handoff
// can have renewed since the handoff began. The worker keeps its id and
// generation, binds the new pane at that terminal, and its inbox is
// released. A terminal a live row binds already, the worker's own included,
// is refused with nothing changed; one a retired row held is free (QA #140
// F5).
func TestCompleteHandoffOnto(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 4)
	seedWorker(t, s, "v", "w14:p3", 0)
	h, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	sent := send(t, s, worker("w"), boss, "held").ID

	for name, term := range map[string]string{
		"another live row's terminal": terminal("w14:p3"),
		"the worker's own terminal":   terminal("w14:p2"),
		"no terminal":                 "",
		"a control byte":              "term_\x1b",
	} {
		require.ErrorIs(t, s.CompleteHandoffOnto(ctx, h, term, t1), store.ErrInvalid, name)
		assert.Equal(t, "pending", handoffState(t, s, h), name)
		noClaim(t, s, worker("w"), name)
	}

	require.NoError(t, s.CompleteHandoffOnto(ctx, h, "term_renewed", t1.Add(time.Second)))
	row, err := s.WorkerRow(ctx, "w")
	require.NoError(t, err)
	assert.Equal(t, "w14:p7", row.PaneID)
	assert.Equal(t, "term_renewed", row.TerminalID, "the terminal the ack proved, not the one recorded")
	assert.Equal(t, int64(4), row.Generation, "the same worker (I1)")
	assert.Equal(t, "acknowledged", handoffState(t, s, h))
	claimed, ok, err := s.Claim(ctx, worker("w"), t1.Add(2*time.Second))
	require.NoError(t, err)
	require.True(t, ok, "released")
	assert.Equal(t, sent, claimed.ID)
	require.ErrorIs(t, s.CompleteHandoffOnto(ctx, h, "term_other", t1), store.ErrNoHandoff, "completed once")

	again, err := s.BeginHandoff(ctx, "w", "w14:p8", terminal("w14:p8"), t1)
	require.NoError(t, err)
	retire(t, s, "v")
	require.NoError(t, s.CompleteHandoffOnto(ctx, again, terminal("w14:p3"), t1), "a retired row binds no terminal")
	row, err = s.WorkerRow(ctx, "w")
	require.NoError(t, err)
	assert.Equal(t, terminal("w14:p3"), row.TerminalID)
}

// TestPendingHandoffsNameTheWorkersPlace: a pending handoff carries the
// project and group of its worker's row at the handoff's generation, so the
// handoff's events name them with no other read. One whose id was reused
// since, or begun before migration 0003 with no generation, carries none.
func TestPendingHandoffsNameTheWorkersPlace(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 4)
	seedWorker(t, s, "v", "w14:p3", 0)
	_, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	_, err = s.BeginHandoff(ctx, "v", "w14:p8", terminal("w14:p8"), t1)
	require.NoError(t, err)
	reuse(t, s, "v")
	seedWorker(t, s, "u", "w14:p4", 0)
	_, err = store.DB(s).ExecContext(ctx,
		"INSERT INTO handoffs (logical_id, old_pane, new_pane, created_at, updated_at) VALUES ('u', 'w14:p4', 'w14:pB', 5, 5)")
	require.NoError(t, err, "a handoff as migration 0003 found it")

	got, err := s.PendingHandoffs(ctx)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, [2]string{"agentisan", "coders"}, [2]string{got[0].Project, got[0].Group})
	assert.Equal(t, [2]string{"", ""}, [2]string{got[1].Project, got[1].Group}, "the id's reuse is another worker")
	assert.Equal(t, [2]string{"", ""}, [2]string{got[2].Project, got[2].Group}, "no generation to find a row by")
}

// TestHandoffAcknowledgedOnto: a handoff that completed onto a pane is told
// from one pending, failed, at another generation or onto another pane, so
// an ack repeated after a crash past the complete can be told from an ack
// on any worker's pane.
func TestHandoffAcknowledgedOnto(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 4)
	seedWorker(t, s, "v", "w14:p3", 0)
	h, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	f, err := s.BeginHandoff(ctx, "v", "w14:p8", terminal("w14:p8"), t1)
	require.NoError(t, err)
	require.NoError(t, s.FailHandoff(ctx, f, t1))

	for _, tc := range []struct {
		id, pane string
		gen      int64
	}{{"w", "w14:p7", 4}, {"v", "w14:p8", 0}} {
		ok, err := s.HandoffAcknowledgedOnto(ctx, tc.id, tc.gen, tc.pane)
		require.NoError(t, err)
		assert.False(t, ok, "%s: pending or failed", tc.id)
	}
	require.NoError(t, s.CompleteHandoff(ctx, h, t1))
	for _, tc := range []struct {
		id, pane string
		gen      int64
		want     bool
	}{{"w", "w14:p7", 4, true}, {"w", "w14:p2", 4, false}, {"w", "w14:p7", 5, false}, {"v", "w14:p7", 4, false}} {
		ok, err := s.HandoffAcknowledgedOnto(ctx, tc.id, tc.gen, tc.pane)
		require.NoError(t, err)
		assert.Equal(t, tc.want, ok, "%+v", tc)
	}
	require.NoError(t, s.Close())
	_, err = s.HandoffAcknowledgedOnto(context.Background(), "w", 4, "w14:p7")
	require.ErrorIs(t, err, store.ErrStore)
}

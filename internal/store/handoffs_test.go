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

	h, err := s.BeginHandoff(ctx, "ha-coder-2", "w14:p7", t1)
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
	n, err := s.ReturnToBoss(ctx, "ha-coder-2", boss, t1)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing queued for the binding goes to the boss")
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

	h, err := s.BeginHandoff(ctx, "w", "w14:p7", t1)
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
	h, err := s.BeginHandoff(ctx, "w", "w14:p7", t1)
	require.NoError(t, err)
	m := send(t, s, worker("w"), boss, "hi")
	noClaim(t, s, worker("w"), "held")

	require.NoError(t, s.FailHandoff(ctx, h, t1))
	pane, gen := bound(t, s, "w")
	assert.Equal(t, "w14:p2", pane)
	assert.Equal(t, int64(1), gen)
	assert.Equal(t, "failed", handoffState(t, s, h))
	assert.Equal(t, m.ID, claim(t, s, worker("w")).ID, "released to the old session")

	h2, err := s.BeginHandoff(ctx, "w", "w14:p8", t1)
	require.NoError(t, err, "a new handoff may begin once the last one ended")
	assert.NotEqual(t, h, h2)
}

// TestHandoffRefusals: a handoff needs a live worker and a new pane, one at a
// time; completing or failing needs a pending handoff, and completing needs
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
		"no such worker":      {id: "nobody", pane: "w14:p9", want: store.ErrNotBound},
		"a retired worker":    {id: "gone", pane: "w14:p9", want: store.ErrNotBound},
		"the pane it is on":   {id: "w", pane: "w14:p2", want: store.ErrInvalid},
		"no new pane":         {id: "w", pane: "", want: store.ErrInvalid},
		"a control byte":      {id: "w", pane: "w14:\x1bp9", want: store.ErrInvalid},
		"the zero time":       {id: "w", pane: "w14:p9", at: time.Time{}, want: store.ErrInvalid},
		"an empty logical id": {id: "", pane: "w14:p9", want: store.ErrInvalid},
	} {
		at := tc.at
		if name != "the zero time" {
			at = t1
		}
		_, err := s.BeginHandoff(ctx, tc.id, tc.pane, at)
		require.ErrorIs(t, err, tc.want, name)
	}

	h, err := s.BeginHandoff(ctx, "w", "w14:p7", t1)
	require.NoError(t, err)
	_, err = s.BeginHandoff(ctx, "w", "w14:p8", t1)
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
	h, err := s.BeginHandoff(t.Context(), "w", "w14:p7", t1)
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

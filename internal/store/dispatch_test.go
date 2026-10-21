package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// setStatus moves id's live row to status, as the workforce commands and
// the team poll do.
func setStatus(t *testing.T, s *store.Store, id, status string) {
	t.Helper()
	_, err := store.DB(s).ExecContext(t.Context(), "UPDATE workers SET status = ? WHERE logical_id = ? AND retired_at IS NULL", status, id)
	require.NoError(t, err)
}

// TestReusedIDInheritsNothing (#29's QA N1): before ReturnToBoss runs, a
// reused id's new binding claims only what was sent to it, never what was
// sent to the binding before.
func TestReusedIDInheritsNothing(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "w", "w14:p2", 0)
	send(t, s, worker("w"), boss, "for generation 0")
	retire(t, s, "w")
	reuse(t, s, "w")
	setStatus(t, s, "w", "active")
	fresh := send(t, s, worker("w"), boss, "for generation 1")
	assert.Equal(t, fresh.ID, claim(t, s, worker("w")).ID)
	noClaim(t, s, worker("w"), "generation 0's message is not generation 1's")
}

// TestClaimHoldsALostOrStartingWorker: a lost row binds no pane proven to
// type into (A35), and a starting one has no agent ready yet, so neither's
// inbox is claimed until the row is active again; the messages stay queued
// and keep their order.
func TestClaimHoldsALostOrStartingWorker(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"lost", "starting"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			seedWorker(t, s, "w", "w14:p2", 0)
			first := send(t, s, worker("w"), boss, "one")
			send(t, s, worker("w"), boss, "two")
			setStatus(t, s, "w", status)
			noClaim(t, s, worker("w"), "a "+status+" worker's inbox is held")
			assert.Equal(t, store.MessageQueued, message(t, s, first.ID).State)

			setStatus(t, s, "w", "active")
			assert.Equal(t, first.ID, claim(t, s, worker("w")).ID, "released in order")
		})
	}
}

// TestQueuedRecipients: every address with a message still queued, once
// each, the one whose oldest queued message is oldest first; a recipient
// with only messages in flight or settled is not listed.
func TestQueuedRecipients(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "w1", "w14:p2", 0)
	seedWorker(t, s, "w2", "w14:p3", 0)
	seedWorker(t, s, "w3", "w14:p4", 0)

	send(t, s, worker("w2"), boss, "w2's first")
	send(t, s, boss, worker("w1"), "the boss's")
	send(t, s, worker("w1"), boss, "w1's")
	send(t, s, worker("w2"), boss, "w2's second")
	send(t, s, pane, boss, "a pane's")
	inFlight := send(t, s, worker("w3"), boss, "w3's, claimed")
	require.Equal(t, inFlight.ID, claim(t, s, worker("w3")).ID)

	got, err := s.QueuedRecipients(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []store.Address{worker("w2"), boss, worker("w1"), pane}, got)

	empty := open(t, dbPath(t))
	got, err = empty.QueuedRecipients(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestEndedQueues: the workers with messages queued to a binding that has
// ended, each with the project its messages belong to, so the dispatcher
// can return them to that project's boss. A live binding's queue, a lost
// one's (it has not ended) and a boss's are not listed.
func TestEndedQueues(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "retired", "w14:p2", 0)
	seedWorker(t, s, "reused", "w14:p3", 0)
	seedWorker(t, s, "live", "w14:p4", 0)
	seedWorker(t, s, "lost", "w14:p5", 0)
	for _, id := range []string{"retired", "reused", "live", "lost"} {
		send(t, s, worker(id), boss, "to "+id)
	}
	send(t, s, boss, worker("live"), "to the boss")
	retire(t, s, "retired")
	retire(t, s, "reused")
	reuse(t, s, "reused")
	setStatus(t, s, "lost", "lost")

	got, err := s.EndedQueues(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, []store.EndedQueue{{Worker: "retired", Project: "agentisan"}, {Worker: "reused", Project: "agentisan"}}, got)

	n, err := s.ReturnToBoss(t.Context(), "retired", "agentisan", boss, t1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	got, err = s.EndedQueues(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []store.EndedQueue{{Worker: "reused", Project: "agentisan"}}, got, "a returned queue is no longer listed")
}

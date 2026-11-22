package store_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// Each queue write has sampled its clock before the acknowledgement but
// commits after it. Its stored time must put it past the held cutoff.
func TestHeldAckCountsAWriteThatCommitsAfterIt(t *testing.T) {
	t.Parallel()
	sampled, ackAt := t1.Add(time.Minute), t1.Add(2*time.Minute)
	want := ackAt.Add(time.Nanosecond)
	ack := func(t *testing.T, s *store.Store) {
		t.Helper()
		_, err := s.AckUpto(t.Context(), "ha", boss.ID, 0, "", ackAt)
		require.NoError(t, err)
	}

	t.Run("append a notice", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		old, _, err := s.Notify(t.Context(), boss, t1, func(string) (string, bool) { return "old line", true })
		require.NoError(t, err)
		ack(t, s)
		m, appended, err := s.Notify(t.Context(), boss, sampled, func(q string) (string, bool) { return q + "\nnew line", true })
		require.NoError(t, err)
		require.True(t, appended)
		require.Equal(t, old.ID, m.ID)
		assert.Equal(t, want, m.UpdatedAt)
	})

	t.Run("send a row reusing a pruned seq", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		old := send(t, s, boss, pane, "the newest row")
		oldSeq := messageSeq(t, s, old.ID)
		ack(t, s)
		require.NoError(t, s.Delivered(t.Context(), claim(t, s, boss).ID, ackAt))
		n, err := s.PruneMessages(t.Context(), ackAt.Add(store.MessageRetention+time.Hour))
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		m, err := s.Send(t.Context(), boss, pane, "sampled before A", sampled)
		require.NoError(t, err)
		require.Equal(t, oldSeq, messageSeq(t, s, m.ID), "SQLite reused the pruned seq")
		assert.Equal(t, sampled, m.CreatedAt)
		assert.Equal(t, want, m.UpdatedAt)
	})

	t.Run("put a row back", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		send(t, s, boss, pane, "refused before typing")
		m := claim(t, s, boss)
		send(t, s, boss, pane, "newer and covered")
		ack(t, s)
		require.NoError(t, s.Requeue(t.Context(), m.ID, "agent_not_ready", sampled))
		got, err := s.Message(t.Context(), m.ID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageQueued, got.State)
		assert.Equal(t, want, got.UpdatedAt)
	})

	t.Run("return a worker's row to the boss", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		seedWorker(t, s, "w", "w14:p2", 1)
		old := send(t, s, worker("w"), boss, "for w")
		send(t, s, boss, pane, "newer and covered")
		ack(t, s)
		retire(t, s, "w")
		returned, err := s.ReturnToBoss(t.Context(), "w", "agentisan", boss, sampled)
		require.NoError(t, err)
		require.Len(t, returned, 1)
		assert.Equal(t, old.ID, returned[0].ID)
		assert.Equal(t, want, returned[0].UpdatedAt)
	})

	t.Run("notify the boss after delivery fails", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		seedWorker(t, s, "w", "w14:p2", 1)
		failed := send(t, s, worker("w"), boss, "to w, from the boss")
		claim(t, s, worker("w"))
		ack(t, s)
		require.NoError(t, s.Fail(t.Context(), failed.ID, "timed out", sampled))
		var updated int64
		require.NoError(t, store.DB(s).QueryRowContext(t.Context(),
			"SELECT updated_at FROM messages WHERE notice_of = ? AND recipient = ?", failed.ID, boss.ID).Scan(&updated))
		assert.Equal(t, want.UnixNano(), updated)
	})
}

func TestHeldAckCoversARowStampedAheadOfItsClock(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	late := t1.Add(time.Hour)
	_, err := s.AckUpto(t.Context(), "ha", boss.ID, 0, "", late)
	require.NoError(t, err)
	m := send(t, s, boss, pane, "written with the clock behind the first ack")
	assert.Equal(t, late.Add(time.Nanosecond), m.UpdatedAt)

	got, err := s.AckUpto(t.Context(), "ha", boss.ID, 0, "", t1.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, messageSeq(t, s, m.ID), got.HeldThrough)
	assert.Equal(t, m.UpdatedAt, got.HeldAt, "the second ack covers the row despite its earlier clock")
}

func TestHeldAckStampUsesOnlyTheRecipientsRecords(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	latest := t1.Add(2 * time.Minute)
	_, err := s.AckUpto(t.Context(), "ha", boss.ID, 0, "", t1.Add(time.Minute))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "web", boss.ID, 0, "", latest)
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "ha", "other-boss", 0, "", latest.Add(time.Hour))
	require.NoError(t, err)

	assert.Equal(t, latest.Add(time.Nanosecond), send(t, s, boss, pane, "to the boss").UpdatedAt)
	sameID := store.Address{Kind: store.KindPane, ID: boss.ID}
	assert.Equal(t, t1, send(t, s, sameID, pane, "to a pane").UpdatedAt)
}

func TestMessageWritesKeepAnUnstampedPreEpochTime(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	m, _, err := s.Notify(t.Context(), boss, t1, func(string) (string, bool) { return "old line", true })
	require.NoError(t, err)
	// SQL can hold a pre-1970 time even though a ULID cannot be made then.
	_, err = store.DB(s).ExecContext(t.Context(), "UPDATE messages SET created_at = -2, updated_at = -2 WHERE id = ?", m.ID)
	require.NoError(t, err)
	at := time.Unix(0, -1).UTC()
	got, appended, err := s.Notify(t.Context(), boss, at, func(q string) (string, bool) { return q + "\nnew line", true })
	require.NoError(t, err)
	require.True(t, appended)
	assert.Equal(t, at, got.UpdatedAt)
}

func TestHeldAckStampAtLatestStoreTime(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	limit := time.Unix(0, math.MaxInt64).UTC()
	_, err := s.Send(t.Context(), boss, pane, "a row at the timestamp limit", limit)
	require.NoError(t, err)
	held, err := s.AckUpto(t.Context(), "ha", boss.ID, 0, "", limit)
	require.NoError(t, err)
	require.Equal(t, limit, held.HeldAt)

	m := send(t, s, boss, pane, "written after the ack")
	assert.Equal(t, limit, m.UpdatedAt)
}

package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// minute is t1 plus n minutes.
func minute(n int) time.Time { return t1.Add(time.Duration(n) * time.Minute) }

// TestBoardPendingSkipsAcknowledgedPosts (A74): a post the user
// acknowledged is no longer pending, and the oldest pending is the oldest
// left unacknowledged.
func TestBoardPendingSkipsAcknowledgedPosts(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var seqs []int64
	for i := range 3 {
		p := teamPost("ha", "ha-coder-1", "post")
		p.At = boardAt.Add(time.Duration(i) * time.Minute)
		seqs = append(seqs, post(t, s, p).Seq)
	}
	_, err := s.AckPost(t.Context(), "ha", seqs[0], boardAt.Add(time.Hour))
	require.NoError(t, err)
	got := pending(t, s)
	assert.Equal(t, 2, got.Pending)
	assert.Equal(t, boardAt.Add(time.Minute), got.Oldest, "the oldest unacknowledged")
}

func TestBoardPendingNewestCeilingPostSkipsAcknowledgedCeilings(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	old := post(t, s, daemonPost("ha", "ctx_ceiling", "ha-boss is at 90% context", minute(1)))
	newest := post(t, s, daemonPost("ha", "ctx_ceiling", "ha-boss is at 95% context", minute(2)))
	require.Equal(t, minute(2), pending(t, s).NewestCeilingPost)
	ackPost(t, s, "ha", newest.Seq)
	assert.Equal(t, minute(1), pending(t, s).NewestCeilingPost, "the newest ceiling post still pending")
	ackPost(t, s, "ha", old.Seq)
	assert.Zero(t, pending(t, s).NewestCeilingPost)
}

// TestBoardPendingSkipsAcknowledgedHeldMessages (A74): an `A` acknowledges
// every message then queued for the boss; one queued after it counts.
func TestBoardPendingSkipsAcknowledgedHeldMessages(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	_, err := s.Send(t.Context(), boss, pane, "first", minute(1))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "ha", "ha-boss", 0, "", minute(2))
	require.NoError(t, err)
	_, err = s.Send(t.Context(), boss, pane, "second", minute(3))
	require.NoError(t, err)
	got := pending(t, s)
	assert.Equal(t, 1, got.Held)
	assert.Equal(t, minute(3), got.OldestHeld)
}

// TestBoardPendingReportsTheLastAck (A74): LastAck is the user's latest
// acknowledgement on the project, of a post or of the waiting messages, an
// `A` that acknowledged no post included; another project's is not.
func TestBoardPendingReportsTheLastAck(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ha := post(t, s, teamPost("ha", "ha-coder-1", "post"))
	web := post(t, s, teamPost("web", "web-coder-1", "post"))
	assert.Zero(t, pending(t, s).LastAck, "none yet")
	_, err := s.AckPost(t.Context(), "ha", ha.Seq, minute(1))
	require.NoError(t, err)
	assert.Equal(t, minute(1), pending(t, s).LastAck, "the latest post acknowledgement alone")
	_, err = s.AckUpto(t.Context(), "ha", "ha-boss", 0, "", minute(2))
	require.NoError(t, err)
	_, err = s.AckPost(t.Context(), "web", web.Seq, minute(3))
	require.NoError(t, err)
	assert.Equal(t, minute(2), pending(t, s).LastAck)
}

// TestBoardPendingCountsChangedHeldRowsAgain (A74, M1): an `A` acknowledges
// the waiting messages as they stood; a notice appended to one, and a
// message returned to the boss with an older seq, count again.
func TestBoardPendingCountsChangedHeldRowsAgain(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "w", "w14:p2", 0)
	_, err := s.Send(t.Context(), worker("w"), pane, "for the worker", minute(0))
	require.NoError(t, err)
	_, _, err = s.Notify(t.Context(), boss, minute(1), func(q string) (string, bool) { return "a turn ended", q == "" })
	require.NoError(t, err)
	_, err = s.Send(t.Context(), boss, pane, "a pane's", minute(2))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "ha", "ha-boss", 0, "", minute(3))
	require.NoError(t, err)
	require.Zero(t, pending(t, s).Held, "acknowledged")
	_, appended, err := s.Notify(t.Context(), boss, minute(4), func(q string) (string, bool) { return q + "\nanother turn ended", q != "" })
	require.NoError(t, err)
	require.True(t, appended)
	retire(t, s, "w")
	returned, err := s.ReturnToBoss(t.Context(), "w", "agentisan", boss, minute(5))
	require.NoError(t, err)
	require.Len(t, returned, 1)
	assert.Equal(t, 2, pending(t, s).Held, "the appended notice and the returned message")
}

// TestBoardPendingHeldAckIsPerBoss (A74): a boss's held acknowledgement
// hides no other boss's messages, a lower seq queued before it included.
func TestBoardPendingHeldAckIsPerBoss(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	other := store.Address{Kind: store.KindBoss, ID: "ha-boss2"}
	_, err := s.Send(t.Context(), other, pane, "the other boss's", minute(0))
	require.NoError(t, err)
	for i := 1; i <= 5; i++ {
		_, err = s.Send(t.Context(), boss, pane, "held", minute(i))
		require.NoError(t, err)
	}
	_, err = s.AckUpto(t.Context(), "ha", "ha-boss", 0, "", minute(6))
	require.NoError(t, err)
	p, err := s.BoardPending(t.Context(), "ha", "ha-boss2", notice.CeilingMark)
	require.NoError(t, err)
	assert.Equal(t, 1, p.Held)
	assert.Zero(t, pending(t, s).Held, "ha-boss's acknowledged")
}

// TestBoardPendingHeldAckCountsANewerSeq (A74, M1): a message queued after
// the A counts by its seq even when its clock reads the record's time or
// earlier, a clock stepped back.
func TestBoardPendingHeldAckCountsANewerSeq(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	_, err := s.Send(t.Context(), boss, pane, "first", minute(5))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "ha", "ha-boss", 0, "", minute(6))
	require.NoError(t, err)
	_, err = s.Send(t.Context(), boss, pane, "queued later, at an earlier clock", minute(4))
	require.NoError(t, err)
	assert.Equal(t, 1, pending(t, s).Held)
}

func TestHeldAckCountsAnUnstampedNewerRowBySeq(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	_, err := s.Send(t.Context(), boss, pane, "covered", minute(5))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "ha", boss.ID, 0, "", minute(6))
	require.NoError(t, err)

	// Bypass Send's stamp so the new row can count only by its seq.
	at := minute(4).UnixNano()
	_, err = store.DB(s).ExecContext(t.Context(), `
INSERT INTO messages (id, recipient_kind, recipient, project, sender_kind, sender, body, created_at, updated_at)
VALUES ('00000000000000000000000001', 'boss', ?, '', 'pane', ?, 'queued after the ack', ?, ?)`, boss.ID, pane.ID, at, at)
	require.NoError(t, err)

	// The other boss's record has a higher seq and sorts first by key.
	other := store.Address{Kind: store.KindBoss, ID: "a-boss"}
	_, err = s.Send(t.Context(), other, pane, "the other boss's", minute(7))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "ha", other.ID, 0, "", minute(8))
	require.NoError(t, err)

	assert.Equal(t, 1, pending(t, s).Held, "the newer seq counts against this boss's acknowledgement")
}

// TestBoardPendingHeldAckIsPerProject (A74): the waiting messages' record is
// one per project and boss. The boss's record on another project hides none
// of its messages here, and neither it nor another boss's record here is
// this boss's LastAck.
func TestBoardPendingHeldAckIsPerProject(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	_, err := s.Send(t.Context(), boss, pane, "held", minute(0))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "web", "ha-boss", 0, "", minute(1))
	require.NoError(t, err)
	_, err = s.AckUpto(t.Context(), "ha", "ha-boss2", 0, "", minute(2))
	require.NoError(t, err)
	got := pending(t, s)
	assert.Equal(t, 1, got.Held, "web's record hides nothing on ha")
	assert.Zero(t, got.LastAck, "neither record is ha-boss's on ha")
}

// TestBoardPendingHeldAckLegsAreEachScoped (A74): each of the held filter's
// two legs reads the record of this project and this boss alone, so
// another boss's record, or another project's, never hides a row by seq or
// by its change.
func TestBoardPendingHeldAckLegsAreEachScoped(t *testing.T) {
	t.Parallel()
	t.Run("the seq leg is this boss's", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		other := store.Address{Kind: store.KindBoss, ID: "a-boss"}
		_, err := s.Send(t.Context(), boss, pane, "old", minute(5))
		require.NoError(t, err)
		_, err = s.AckUpto(t.Context(), "ha", boss.ID, 0, "", minute(6))
		require.NoError(t, err)
		// Send stamps the older clock. The unstamped case above isolates the seq leg.
		_, err = s.Send(t.Context(), boss, pane, "new seq, older time", minute(4))
		require.NoError(t, err)
		_, err = s.Send(t.Context(), other, pane, "the other boss's", minute(7))
		require.NoError(t, err)
		_, err = s.AckUpto(t.Context(), "ha", other.ID, 0, "", minute(8))
		require.NoError(t, err)
		assert.Equal(t, 1, pending(t, s).Held, "a-boss's record, past the new seq, hides nothing of ha-boss's")
	})
	t.Run("the time leg is this project's", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		// aa's record comes first by key and by rowid, so a time leg that
		// read any project's record would read aa's.
		_, err := s.AckUpto(t.Context(), "aa", boss.ID, 0, "", minute(0))
		require.NoError(t, err)
		_, _, err = s.Notify(t.Context(), boss, minute(1), func(string) (string, bool) { return "one", true })
		require.NoError(t, err)
		_, err = s.AckUpto(t.Context(), "ha", boss.ID, 0, "", minute(2))
		require.NoError(t, err)
		// An append to the same row: only the time leg counts it.
		_, _, err = s.Notify(t.Context(), boss, minute(3), func(q string) (string, bool) { return q + "\ntwo", true })
		require.NoError(t, err)
		_, err = s.AckUpto(t.Context(), "aa", boss.ID, 0, "", minute(4))
		require.NoError(t, err)
		assert.Equal(t, 1, pending(t, s).Held, "aa's later record hides nothing on ha")
	})
}

func TestBoardPendingAppendedNoticeKeepsItsCreationAge(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	_, _, err := s.Notify(ctx, boss, minute(0), func(q string) (string, bool) {
		return "a turn ended", q == ""
	})
	require.NoError(t, err)
	_, err = s.AckUpto(ctx, "ha", boss.ID, 0, "", minute(30))
	require.NoError(t, err)
	require.Zero(t, pending(t, s).Held)

	_, appended, err := s.Notify(ctx, boss, minute(31), func(q string) (string, bool) {
		return q + "\nanother turn ended", q != ""
	})
	require.NoError(t, err)
	require.True(t, appended)
	got := pending(t, s)
	require.Equal(t, 1, got.Held)
	assert.Equal(t, minute(0), got.OldestHeld)
}

func TestBoardPendingDrainedAcknowledgedMessageCountsAsAPost(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	_, err := s.Send(ctx, boss, pane, "first", minute(0))
	require.NoError(t, err)
	_, err = s.AckUpto(ctx, "ha", boss.ID, 0, "", minute(30))
	require.NoError(t, err)
	before := pending(t, s)
	require.Zero(t, before.Held+before.Pending)

	out, err := s.PostQueued(ctx, boss, "ha", minute(31))
	require.NoError(t, err)
	require.Len(t, out.Posts, 1)
	got := pending(t, s)
	assert.Equal(t, 1, got.Pending)
	assert.Zero(t, got.Held)
	assert.Equal(t, minute(0), got.Oldest)
	assert.Equal(t, minute(30), got.LastAck)
}

func TestBoardPendingHeldAckHidesAnEqualStamp(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	_, err := s.AckUpto(ctx, "ha", boss.ID, 0, "", minute(10))
	require.NoError(t, err)
	_, err = s.Send(ctx, boss, pane, "stamped ahead of its clock", minute(5))
	require.NoError(t, err)
	require.Equal(t, 1, pending(t, s).Held)
	_, err = s.AckUpto(ctx, "ha", boss.ID, 0, "", minute(6))
	require.NoError(t, err)
	assert.Zero(t, pending(t, s).Held)
}

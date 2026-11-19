package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// ackAt is when the tests' acknowledgements are made.
var ackAt = boardAt.Add(time.Minute)

// daemonPost is a notice on project's board.
func daemonPost(project, body string) store.BoardPost {
	return store.BoardPost{Project: project, Channel: store.ChannelDaemon, Author: store.Daemon, Kind: "turn_ended", Body: body, At: boardAt}
}

func ackPost(t *testing.T, s *store.Store, project string, seq int64) store.AckResult {
	t.Helper()
	r, err := s.AckPost(t.Context(), project, seq, ackAt)
	require.NoError(t, err)
	return r
}

func ackUpto(t *testing.T, s *store.Store, project string, upto int64, channel string) store.AckResult {
	t.Helper()
	r, err := s.AckUpto(t.Context(), project, boss.ID, upto, channel, ackAt)
	require.NoError(t, err)
	return r
}

// ackRows is every acknowledgement in s: its seq, project and acked_at.
func ackRows(t *testing.T, s *store.Store) [][]any {
	t.Helper()
	return queryRows(t, store.DB(s), "SELECT seq, project, acked_at FROM board_acks ORDER BY seq")
}

// ackedSeqs is the seq of every acknowledged post in s.
func ackedSeqs(t *testing.T, s *store.Store) []int64 {
	t.Helper()
	out := []int64{}
	for _, row := range ackRows(t, s) {
		seq, ok := row[0].(int64)
		require.True(t, ok, "a seq: %v", row[0])
		out = append(out, seq)
	}
	return out
}

// markBoss moves the boss's mark on project's board to upto.
func markBoss(t *testing.T, s *store.Store, project string, upto int64) {
	t.Helper()
	_, err := s.MarkSeen(t.Context(), project, store.ReaderBoss, upto)
	require.NoError(t, err)
}

// messageSeq is the inbox seq of the message id.
func messageSeq(t *testing.T, s *store.Store, id string) int64 {
	t.Helper()
	var seq int64
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT seq FROM messages WHERE id = ?", id).Scan(&seq))
	return seq
}

// TestAckPostAcksAnUnreadPostOnce (A74): the user's acknowledgement of a post
// the boss has not read is one record of that post alone, not the older or
// newer one beside it, written once: a second press writes nothing and keeps
// the first one's time.
func TestAckPostAcksAnUnreadPostOnce(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	post(t, s, teamPost("ha", "ha-coder-1", "older"))
	p := post(t, s, teamPost("ha", "ha-coder-1", "one"))
	post(t, s, teamPost("ha", "ha-coder-1", "newer"))

	assert.Equal(t, store.AckResult{Acked: 1}, ackPost(t, s, "ha", p.Seq))
	again, err := s.AckPost(t.Context(), "ha", p.Seq, ackAt.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, store.AckResult{}, again, "an acknowledged post is not acknowledged again")
	assert.Equal(t, [][]any{{p.Seq, "ha", ackAt.UnixNano()}}, ackRows(t, s), "one record, with the first acknowledgement's time")
}

// TestAckPostNeedsTheBossNotToHaveReadIt (A74): a post the boss has read is
// not the alert's to count, so it takes no acknowledgement. The bound is this
// project's boss's mark, never the user's or another project's.
func TestAckPostNeedsTheBossNotToHaveReadIt(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	read := post(t, s, teamPost("ha", "ha-coder-1", "read by the boss"))
	unread := post(t, s, teamPost("ha", "ha-coder-1", "not read by the boss"))
	markBoss(t, s, "ha", read.Seq)
	markBoss(t, s, "web", post(t, s, teamPost("web", "web-coder-1", "read by web's boss")).Seq)
	_, err := s.MarkSeen(t.Context(), "ha", store.ReaderUser, unread.Seq)
	require.NoError(t, err)

	assert.Equal(t, store.AckResult{}, ackPost(t, s, "ha", read.Seq), "the boss read it")
	assert.Equal(t, store.AckResult{Acked: 1}, ackPost(t, s, "ha", unread.Seq), "the user's mark does not bound it")
	assert.Equal(t, []int64{unread.Seq}, ackedSeqs(t, s))
}

// TestAckPostOnlyInItsProject (A74): a seq from another project's board
// acknowledges nothing on this one. The store takes the projects Post takes,
// the unnamed one ("") included.
func TestAckPostOnlyInItsProject(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	web := post(t, s, teamPost("web", "web-coder-1", "web's"))
	unnamed := post(t, s, teamPost("", "ha-coder-1", "the unnamed project's"))

	assert.Equal(t, store.AckResult{}, ackPost(t, s, "ha", web.Seq), "not on ha's board")
	assert.Equal(t, store.AckResult{}, ackPost(t, s, "", web.Seq), "not on the unnamed project's board")
	assert.Empty(t, ackedSeqs(t, s))
	assert.Equal(t, store.AckResult{Acked: 1}, ackPost(t, s, "web", web.Seq), "on its own board")
	assert.Equal(t, store.AckResult{Acked: 1}, ackPost(t, s, "", unnamed.Seq), "on its own board")
	assert.Equal(t, [][]any{{web.Seq, "web", ackAt.UnixNano()}, {unnamed.Seq, "", ackAt.UnixNano()}}, ackRows(t, s))
}

// TestAckUptoBoundsByMarkLimitAndChannel (A74, the scratch case): an
// acknowledge-all takes the posts after the boss's mark, up to upto, on the
// panel's channel, and "" takes both channels.
func TestAckUptoBoundsByMarkLimitAndChannel(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var seqs []int64
	for i, p := range []store.BoardPost{
		daemonPost("ha", "1"), teamPost("ha", "ha-coder-1", "2"), daemonPost("ha", "3"),
		teamPost("ha", "ha-coder-1", "4"), daemonPost("ha", "5"),
	} {
		got := post(t, s, p)
		require.Equal(t, strconv.Itoa(i+1), got.Body)
		seqs = append(seqs, got.Seq)
	}
	markBoss(t, s, "ha", seqs[0])

	assert.Equal(t, 1, ackUpto(t, s, "ha", seqs[2], store.ChannelDaemon).Acked)
	assert.Equal(t, []int64{seqs[2]}, ackedSeqs(t, s), "after the boss's mark, up to upto, on the daemon channel")
	assert.Equal(t, 2, ackUpto(t, s, "ha", seqs[3], store.ChannelTeam).Acked)
	assert.Equal(t, seqs[1:4], ackedSeqs(t, s), "the team channel's posts 2 and 4")
	assert.Equal(t, 1, ackUpto(t, s, "ha", seqs[4], "").Acked)
	assert.Equal(t, seqs[1:], ackedSeqs(t, s), "both channels, and never the post the boss read")
}

// TestAckUptoReturnsTheNewestHeldMessage (A74, the cold check's M1):
// AckUpto reports the newest seq queued for the boss and the latest
// updated_at among those rows. A notice appended to a waiting one keeps its
// seq, so only HeldAt shows it; so does a worker's message returned to the
// boss, which is older still. A message delivered, or queued for another
// boss or for a pane of the same name, is not held for this one.
func TestAckUptoReturnsTheNewestHeldMessage(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	assert.Equal(t, store.AckResult{}, ackUpto(t, s, "ha", 0, ""), "with nothing queued, 0 and the zero time")

	seedWorker(t, s, "w", "w14:p2", 1)
	forW := send(t, s, worker("w"), boss, "for w, older than the rest")
	_, err := s.Send(ctx, boss, pane, "a pane's message", t1)
	require.NoError(t, err)
	appendTo := func(queued string) (string, bool) {
		if queued == "" {
			return "first notice", true
		}
		return queued + "\nanother notice", true
	}
	notice, appended, err := s.Notify(ctx, boss, t1.Add(time.Minute), appendTo)
	require.NoError(t, err)
	require.False(t, appended)
	// The newest of ha-boss's messages is delivered. Claims take the oldest
	// first, so its state is set here.
	delivered, err := s.Send(ctx, boss, pane, "delivered", t1.Add(2*time.Minute))
	require.NoError(t, err)
	_, err = store.DB(s).ExecContext(ctx, "UPDATE messages SET state = 'delivered', updated_at = ? WHERE id = ?", t1.Add(3*time.Minute).UnixNano(), delivered.ID)
	require.NoError(t, err)
	_, err = s.Send(ctx, store.Address{Kind: store.KindBoss, ID: "web-boss"}, pane, "another boss's", t1.Add(4*time.Minute))
	require.NoError(t, err)
	_, err = s.Send(ctx, store.Address{Kind: store.KindPane, ID: boss.ID}, pane, "a pane named as the boss", t1.Add(5*time.Minute))
	require.NoError(t, err)

	first := ackUpto(t, s, "ha", 0, "")
	assert.Equal(t, messageSeq(t, s, notice.ID), first.HeldThrough, "the notice, the newest still queued for ha-boss")
	assert.Equal(t, t1.Add(time.Minute), first.HeldAt)
	assert.Equal(t, time.UTC, first.HeldAt.Location())
	assert.Equal(t, 0, first.Acked)

	_, appended, err = s.Notify(ctx, boss, t1.Add(6*time.Minute), appendTo)
	require.NoError(t, err)
	require.True(t, appended, "Notify appends to the acknowledged notice")
	second := ackUpto(t, s, "ha", 0, "")
	assert.Equal(t, first.HeldThrough, second.HeldThrough, "the appended notice keeps its seq")
	assert.Equal(t, t1.Add(6*time.Minute), second.HeldAt, "HeldAt moves when Notify appends")

	retire(t, s, "w")
	returned, err := s.ReturnToBoss(ctx, "w", "agentisan", boss, t1.Add(7*time.Minute))
	require.NoError(t, err)
	require.Len(t, returned, 1)
	require.Equal(t, forW.ID, returned[0].ID)
	third := ackUpto(t, s, "ha", 0, "")
	assert.Equal(t, first.HeldThrough, third.HeldThrough, "a returned message keeps its older seq")
	assert.Equal(t, t1.Add(7*time.Minute), third.HeldAt, "HeldAt moves when a message is returned to the boss")
}

// heldAcks is every held acknowledgement in s, by project and boss.
func heldAcks(t *testing.T, s *store.Store) [][]any {
	t.Helper()
	return queryRows(t, store.DB(s), "SELECT project, boss, through_seq, through_at, acked_at FROM board_held_acks ORDER BY project, boss")
}

// TestHeldAckIsSaved (A74, the cold check's minor 12): AckUpto saves its
// acknowledgement of the messages waiting for the boss in board_held_acks,
// so a restart forgets nothing.
func TestHeldAckIsSaved(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	s := open(t, path)
	m, err := s.Send(t.Context(), boss, pane, "held", t1)
	require.NoError(t, err)
	seq := messageSeq(t, s, m.ID)
	require.Equal(t, seq, ackUpto(t, s, "ha", 0, "").HeldThrough)
	require.NoError(t, s.Close())

	assert.Equal(t, [][]any{{"ha", boss.ID, seq, t1.UnixNano(), ackAt.UnixNano()}}, heldAcks(t, open(t, path)), "one record, after a restart")
}

// TestHeldAckIsPerProjectAndBoss (A74): a held acknowledgement is one record
// per project and boss. A later one replaces its own record, whole, and no
// other.
func TestHeldAckIsPerProjectAndBoss(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	first, err := s.Send(ctx, boss, pane, "one", t1)
	require.NoError(t, err)
	for _, key := range [][2]string{{"ha", boss.ID}, {"other", boss.ID}, {"ha", "ha-boss2"}} {
		_, err := s.AckUpto(ctx, key[0], key[1], 0, "", ackAt)
		require.NoError(t, err)
	}
	newer, err := s.Send(ctx, boss, pane, "two", t1.Add(time.Minute))
	require.NoError(t, err)
	_, err = s.AckUpto(ctx, "ha", boss.ID, 0, "", ackAt.Add(time.Minute))
	require.NoError(t, err)

	assert.Equal(t, [][]any{
		{"ha", boss.ID, messageSeq(t, s, newer.ID), t1.Add(time.Minute).UnixNano(), ackAt.Add(time.Minute).UnixNano()},
		{"ha", "ha-boss2", int64(0), int64(0), ackAt.UnixNano()},
		{"other", boss.ID, messageSeq(t, s, first.ID), t1.UnixNano(), ackAt.UnixNano()},
	}, heldAcks(t, s))

	// Both messages delivered, a later acknowledgement records an empty queue:
	// it replaces the record, and never keeps the larger values.
	for range 2 {
		require.NoError(t, s.Delivered(ctx, claim(t, s, boss).ID, t1.Add(2*time.Minute)))
	}
	r, err := s.AckUpto(ctx, "ha", boss.ID, 0, "", ackAt.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, store.AckResult{}, r)
	assert.Equal(t, []any{"ha", boss.ID, int64(0), int64(0), ackAt.Add(2 * time.Minute).UnixNano()}, heldAcks(t, s)[0], "the record says nothing is queued")
}

// TestAcksAtOnceWriteEachPostOnce (A74): acknowledgements made at once, by
// one post and by all, write each post's record once between them, and
// their counts add up to the posts acknowledged.
func TestAcksAtOnceWriteEachPostOnce(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var seqs []int64
	for i := range 4 {
		seqs = append(seqs, post(t, s, teamPost("ha", "ha-coder-1", strconv.Itoa(i))).Seq)
	}
	counts := make(chan int, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			var r store.AckResult
			var err error
			if i%2 == 0 {
				r, err = s.AckUpto(t.Context(), "ha", boss.ID, seqs[3], "", ackAt)
			} else {
				r, err = s.AckPost(t.Context(), "ha", seqs[i%4], ackAt)
			}
			assert.NoError(t, err)
			counts <- r.Acked
		})
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	assert.Equal(t, 4, total, "each post acknowledged once between them")
	assert.Equal(t, seqs, ackedSeqs(t, s))
}

// TestAcksGoWithTheirPost (A74): the prune deletes a post with its
// acknowledgement, through the foreign key's cascade, and leaves a kept
// post's.
func TestAcksGoWithTheirPost(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	old := post(t, s, teamPost("ha", "ha-coder-1", "31 days old at the prune"))
	keptPost := teamPost("ha", "ha-coder-1", "2 days old at the prune")
	keptPost.At = boardAt.Add(29 * 24 * time.Hour)
	kept := post(t, s, keptPost)
	assert.Equal(t, 2, ackUpto(t, s, "ha", kept.Seq, "").Acked)

	n, err := s.PruneBoard(t.Context(), boardAt.Add(31*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Equal(t, []int64{kept.Seq}, ackedSeqs(t, s), "the pruned post's acknowledgement went with it")
	assert.NotContains(t, ackedSeqs(t, s), old.Seq)
	assert.Empty(t, queryRows(t, store.DB(s), "PRAGMA foreign_key_check"))
}

// TestAckMovesNoMark (A74): an acknowledgement is a record, not a mark: it
// moves neither the boss's mark nor the user's.
func TestAckMovesNoMark(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	var newest int64
	for i := range 3 {
		newest = post(t, s, teamPost("ha", "ha-coder-1", strconv.Itoa(i))).Seq
	}
	_, err := s.MarkSeen(ctx, "ha", store.ReaderUser, 1)
	require.NoError(t, err)
	status := func() [2]store.BoardState {
		b, err := s.BoardStatus(ctx, "ha", store.ReaderBoss)
		require.NoError(t, err)
		u, err := s.BoardStatus(ctx, "ha", store.ReaderUser)
		require.NoError(t, err)
		return [2]store.BoardState{b, u}
	}
	before, marks := status(), queryRows(t, store.DB(s), "SELECT * FROM board_marks ORDER BY rowid")

	assert.Equal(t, 3, ackUpto(t, s, "ha", newest, "").Acked)
	assert.Equal(t, before, status(), "both readers stand where they stood")
	assert.Equal(t, marks, queryRows(t, store.DB(s), "SELECT * FROM board_marks ORDER BY rowid"))
}

// TestPostsCarryTheirAck (A74): both reads of the board say which posts the
// user acknowledged.
func TestPostsCarryTheirAck(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	acked := post(t, s, teamPost("ha", "ha-coder-1", "acknowledged"))
	post(t, s, teamPost("ha", "ha-coder-1", "not acknowledged"))
	ackPost(t, s, "ha", acked.Seq)
	ackedOf := func(ps []store.BoardPost) map[string]bool {
		out := map[string]bool{}
		for _, p := range ps {
			out[p.Body] = p.Acked
		}
		return out
	}
	want := map[string]bool{"acknowledged": true, "not acknowledged": false}

	latest, err := s.LatestPosts(t.Context(), "ha", 5)
	require.NoError(t, err)
	assert.Equal(t, want, ackedOf(latest))
	read, err := s.ReadBoard(t.Context(), "ha", store.ReaderUser, 5)
	require.NoError(t, err)
	assert.Equal(t, want, ackedOf(read.Posts))
}

// TestAckChecksItsInput (A74): a bad project, seq, upto, channel, boss or
// time is ErrInvalid, and writes nothing, though a post is there to take
// it and AckUpto would otherwise record a held acknowledgement.
func TestAckChecksItsInput(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	p := post(t, s, daemonPost("ha", "one"))
	for name, ack := range map[string]func() (store.AckResult, error){
		"a channel x": func() (store.AckResult, error) { return s.AckUpto(ctx, "ha", boss.ID, p.Seq, "x", ackAt) },
		"upto -1":     func() (store.AckResult, error) { return s.AckUpto(ctx, "ha", boss.ID, -1, "", ackAt) },
		"seq -1":      func() (store.AckResult, error) { return s.AckPost(ctx, "ha", -1, ackAt) },
		"a project with a line break (AckPost)": func() (store.AckResult, error) {
			return s.AckPost(ctx, "ha\n", p.Seq, ackAt)
		},
		"a project with a line break (AckUpto)": func() (store.AckResult, error) {
			return s.AckUpto(ctx, "ha\n", boss.ID, p.Seq, "", ackAt)
		},
		"no boss": func() (store.AckResult, error) { return s.AckUpto(ctx, "ha", "", p.Seq, "", ackAt) },
		"a boss with a line break": func() (store.AckResult, error) {
			return s.AckUpto(ctx, "ha", "ha\nboss", p.Seq, "", ackAt)
		},
		"the zero time (AckPost)": func() (store.AckResult, error) {
			return s.AckPost(ctx, "ha", p.Seq, time.Time{})
		},
		"the zero time (AckUpto)": func() (store.AckResult, error) {
			return s.AckUpto(ctx, "ha", boss.ID, p.Seq, "", time.Time{})
		},
	} {
		r, err := ack()
		require.ErrorIs(t, err, store.ErrInvalid, name)
		assert.Equal(t, store.AckResult{}, r, name)
		assert.Empty(t, ackedSeqs(t, s), "%s writes nothing", name)
		assert.Empty(t, heldAcks(t, s), "%s writes no held acknowledgement", name)
	}
}

// TestAckTablesRefuseBadRows (A74): the tables refuse, on their own, the
// rows the Go checks never let through. Each bad row differs from a good one
// in one column, refused by that column's rule, and the good one is taken.
func TestAckTablesRefuseBadRows(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	const (
		insertAck  = "INSERT INTO board_acks (seq, project, acked_at) VALUES (?, ?, ?)"
		insertHeld = "INSERT INTO board_held_acks (project, boss, through_seq, through_at, acked_at) VALUES (?, ?, ?, ?, ?)"
	)
	for name, c := range map[string]struct {
		held bool
		bad  map[string]any
		rule string
	}{
		"an acknowledgement with no project":    {false, map[string]any{"project": nil}, "NOT NULL constraint failed: board_acks.project"},
		"an acknowledgement's time as text":     {false, map[string]any{"acked_at": "noon"}, "cannot store TEXT value in INTEGER column board_acks.acked_at"},
		"a held acknowledgement with no boss":   {true, map[string]any{"boss": ""}, "CHECK constraint failed: boss <> ''"},
		"a held acknowledgement through seq -1": {true, map[string]any{"through_seq": -1}, "CHECK constraint failed: through_seq >= 0"},
		"a held acknowledgement through -1 ns":  {true, map[string]any{"through_at": -1}, "CHECK constraint failed: through_at >= 0"},
		"a held acknowledgement's time as text": {true, map[string]any{"acked_at": "noon"}, "cannot store TEXT value in INTEGER column board_held_acks.acked_at"},
	} {
		insert, columns := insertHeld, []string{"project", "boss", "through_seq", "through_at", "acked_at"}
		good := map[string]any{"project": name, "boss": boss.ID, "through_seq": 1, "through_at": 1, "acked_at": ackAt.UnixNano()}
		if !c.held {
			insert, columns = insertAck, []string{"seq", "project", "acked_at"}
			good = map[string]any{"seq": post(t, s, teamPost("ha", "ha-coder-1", name)).Seq, "project": "ha", "acked_at": ackAt.UnixNano()}
		}
		bad := maps.Clone(good)
		maps.Copy(bad, c.bad)
		err := insertRow(t, db, insert, columns, bad)
		constraintFailed(t, err, name)
		assert.ErrorContains(t, err, c.rule, name)
		assert.NoError(t, insertRow(t, db, insert, columns, good), "%s, made good, is taken", name)
	}
}

// TestAckWakesNoReader (A74): an acknowledgement is not a post, so neither
// AckPost nor AckUpto wakes a board wait. The next post does.
func TestAckWakesNoReader(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	one := post(t, s, teamPost("ha", "ha-coder-1", "one"))
	two := post(t, s, teamPost("ha", "ha-coder-1", "two"))
	_, err := s.Send(t.Context(), boss, pane, "held", t1)
	require.NoError(t, err)
	changed := s.BoardChanged()
	waiting := func() bool {
		select {
		case <-changed:
			return false
		default:
			return true
		}
	}

	assert.Equal(t, 1, ackPost(t, s, "ha", one.Seq).Acked)
	assert.True(t, waiting(), "AckPost wakes no waiter")
	r := ackUpto(t, s, "ha", two.Seq, "")
	assert.Equal(t, 1, r.Acked)
	assert.NotZero(t, r.HeldThrough, "AckUpto recorded the held message")
	assert.True(t, waiting(), "AckUpto wakes no waiter")
	post(t, s, teamPost("ha", "ha-coder-1", "three"))
	assert.False(t, waiting(), "a post wakes the waiter")
}

// TestAckUptoIsOneTransaction (A74): AckUpto writes the posts' records and
// the held record in one transaction. When the held record fails, the posts'
// records written before it go back with it.
func TestAckUptoIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	p := post(t, s, teamPost("ha", "ha-coder-1", "one"))
	_, err := store.DB(s).ExecContext(t.Context(), `CREATE TRIGGER refuse_held BEFORE INSERT ON board_held_acks
BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	require.NoError(t, err)

	r, err := s.AckUpto(t.Context(), "ha", boss.ID, p.Seq, "", ackAt)
	require.ErrorIs(t, err, store.ErrStore)
	require.ErrorContains(t, err, "injected")
	assert.Equal(t, store.AckResult{}, r)
	assert.Empty(t, ackRows(t, s), "the post's record went back with the held one")
	assert.Empty(t, heldAcks(t, s))

	_, err = store.DB(s).ExecContext(t.Context(), "DROP TRIGGER refuse_held")
	require.NoError(t, err)
	assert.Equal(t, 1, ackUpto(t, s, "ha", p.Seq, "").Acked, "without the failure, the post is acknowledged")
}

// TestAcksHaveNoProductionCaller: the acknowledgements are dormant until the
// board's PR 3c. No non-test file in internal/ or cmd/ but boardack.go uses
// AckPost, AckUpto or AckResult, or the statements and helper behind them;
// none but boardack.go names board_held_acks in a string; and none but
// boardack.go and board.go names board_acks, board.go only to read it, for
// ReadBoard and LatestPosts. A write split across joined strings is past
// what a scan of single strings can see.
func TestAcksHaveNoProductionCaller(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	const ackFile, boardFile = "internal/store/boardack.go", "internal/store/board.go"
	names := []string{"AckPost", "AckUpto", "AckResult", "ackPosts", "ackOnePost", "ackUptoPost", "ackHeld", "ackRows"}
	write := regexp.MustCompile(`\b(insert|update|delete|replace)\b`)
	uses := map[string][]string{}
	found := map[string]bool{}
	parsed := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			parsed++
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					switch {
					case !slices.Contains(names, n.Name):
					case rel == ackFile:
						found[n.Name] = true
					default:
						uses[rel] = append(uses[rel], fset.Position(n.Pos()).String()+" "+n.Name)
					}
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						break
					}
					lit := strings.ToLower(n.Value)
					for table, allowed := range map[string][]string{
						"board_acks":      {ackFile, boardFile},
						"board_held_acks": {ackFile},
					} {
						switch {
						case !strings.Contains(lit, table):
						case rel == boardFile && write.MatchString(lit):
							uses[rel] = append(uses[rel], fset.Position(n.Pos()).String()+" a write to "+table)
						case slices.Contains(allowed, rel):
							found[table+" in "+rel] = true
						default:
							uses[rel] = append(uses[rel], fset.Position(n.Pos()).String()+" "+table)
						}
					}
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	assert.Greater(t, parsed, 100, "the scan reached the module's packages")
	// The scan finds the names and the table where they are: it cannot pass
	// by seeing nothing.
	for _, name := range append(names, "board_acks in "+ackFile, "board_acks in "+boardFile, "board_held_acks in "+ackFile) {
		assert.True(t, found[name], "the scan finds %s", name)
	}
	assert.Empty(t, uses, "the acknowledgements are dormant until PR 3c")
}

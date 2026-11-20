package store_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// queueProject is the board PostQueued posts on in these tests, and
// postedAt when it posts.
const queueProject = "agentisan"

var postedAt = t1.Add(time.Hour)

// failBoardInserts makes the board refuse, as a statement error, every post
// that matches when; a messages insert still works. A TEMP trigger lives on
// one connection, and the store holds exactly one.
func failBoardInserts(t *testing.T, s *store.Store, when string) {
	t.Helper()
	_, err := store.DB(s).ExecContext(t.Context(), "CREATE TEMP TRIGGER fail_board_inserts BEFORE INSERT ON board_posts WHEN "+when+
		" BEGIN SELECT RAISE(ABORT, 'test: the board refuses this post'); END")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = store.DB(s).ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_board_inserts")
	})
}

// insertMessage writes m raw, queued at t1 with a fresh id, for a row no API
// writes, such as one too long for Send or from a 300-byte sender. A worker
// sender's generation is 0. It returns m as written.
func insertMessage(t *testing.T, s *store.Store, m store.Message) store.Message {
	t.Helper()
	id, err := store.NewULID(t1, rand.Reader)
	require.NoError(t, err)
	m.ID, m.State, m.CreatedAt, m.UpdatedAt = id, store.MessageQueued, t1, t1
	_, err = store.DB(s).ExecContext(t.Context(), `INSERT INTO messages
	(id, recipient_kind, recipient, project, generation, sender_kind, sender, sender_generation, body, notice_of, returned_from, created_at, updated_at)
	VALUES (?1, ?2, ?3, ?4, CASE WHEN ?2 = 'worker' THEN ?5 END, ?6, ?7, CASE WHEN ?6 = 'worker' THEN 0 END, ?8, ?9, ?10, ?11, ?11)`,
		m.ID, m.To.Kind, m.To.ID, m.Project, m.Generation, m.From.Kind, m.From.ID, m.Body, m.NoticeOf, m.ReturnedFrom, t1.UnixNano())
	require.NoError(t, err)
	return m
}

func postQueued(t *testing.T, s *store.Store) store.PostedQueue {
	t.Helper()
	got, err := s.PostQueued(t.Context(), boss, queueProject, postedAt)
	require.NoError(t, err)
	return got
}

// postCount is how many posts every board holds.
func postCount(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT count(*) FROM board_posts").Scan(&n))
	return n
}

func ids(ms []store.Message) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// assertState asserts every message in ms is in state.
func assertState(t *testing.T, s *store.Store, state string, ms ...store.Message) {
	t.Helper()
	for _, m := range ms {
		assert.Equal(t, state, message(t, s, m.ID).State, "message %q", m.Body)
	}
}

// ulidTime is the time an id made at at carries: a ULID's first ten
// characters, its 48-bit millisecond time.
func ulidTime(t *testing.T, at time.Time) string {
	t.Helper()
	id, err := store.NewULID(at, rand.Reader)
	require.NoError(t, err)
	return id[:10]
}

// TestPostQueuedSplitsAnOversizedFailureNotice (B1): a failure notice
// carries the whole failed body, so it can pass 8 KiB. It is posted in
// parts, each within MaxBody, the first under the notice's id, and the
// notice is delivered, never refused and left to wedge the boss's queue.
func TestPostQueuedSplitsAnOversizedFailureNotice(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "ha-coder-1", "w14:p2", 0)
	m := send(t, s, worker("ha-coder-1"), boss, strings.Repeat("b", store.MaxBody))
	claim(t, s, worker("ha-coder-1"))
	require.NoError(t, s.Fail(t.Context(), m.ID, strings.Repeat("r", 512), t1))
	var nid string
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT id FROM messages WHERE notice_of = ?", m.ID).Scan(&nid))
	notice := message(t, s, nid)
	require.Greater(t, len(notice.Body), store.MaxBody, "the notice is longer than a post may be")

	got := postQueued(t, s)
	require.Len(t, got.Posts, 2)
	var joined string
	for i, p := range got.Posts {
		prefix := fmt.Sprintf("(part %d of 2) ", i+1)
		assert.LessOrEqual(t, len(p.Body), store.MaxBody)
		require.True(t, strings.HasPrefix(p.Body, prefix), "post %d begins %q", i, prefix)
		joined += strings.TrimPrefix(p.Body, prefix)
		assert.Equal(t, "undelivered", p.Kind)
		assert.Equal(t, m.ID, p.Subject)
	}
	assert.Equal(t, notice.Body, joined, "the parts join to the notice")
	assert.Equal(t, nid, got.Posts[0].ID, "the first part keeps the notice's id")
	assert.Len(t, got.Posts[1].ID, 26)
	assert.NotEqual(t, nid, got.Posts[1].ID, "a later part has an id of its own")
	assert.Equal(t, ulidTime(t, notice.CreatedAt), got.Posts[1].ID[:10], "a later part's id carries the notice's time")
	assert.Equal(t, []string{nid}, ids(got.Delivered))
	assert.Empty(t, got.Failed)
	assertState(t, s, store.MessageDelivered, notice)
}

// TestPostQueuedSplitsOnARuneBoundary: a cut never falls inside a
// character, so each part is UTF-8 that CheckBody takes, and the row is
// delivered, not failed. The cut moves back to the character's start, never
// forward past MaxBody - 32.
func TestPostQueuedSplitsOnARuneBoundary(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	body := "x" + strings.Repeat("😀", 2100)
	require.Len(t, body, 8401)
	m := insertMessage(t, s, store.Message{To: boss, From: store.Daemon, Body: body})

	got := postQueued(t, s)
	require.Len(t, got.Posts, 2)
	var joined string
	for i, p := range got.Posts {
		assert.True(t, utf8.ValidString(p.Body), "post %d is UTF-8", i)
		joined += strings.TrimPrefix(p.Body, fmt.Sprintf("(part %d of 2) ", i+1))
	}
	assert.Equal(t, body, joined, "the parts join to the body")
	assert.Len(t, strings.TrimPrefix(got.Posts[0].Body, "(part 1 of 2) "), store.MaxBody-32-3,
		"the cut at MaxBody - 32 falls 3 bytes into an emoji and moves back to its start")
	assert.Empty(t, got.Failed)
	assertState(t, s, store.MessageDelivered, m)
}

// TestPostQueuedCutsPartsAtMaxBodyLessThePrefix: each part but the last
// holds MaxBody - 32 bytes of the body, the 32 its "(part i of n) " prefix
// may take, and the last part the rest.
func TestPostQueuedCutsPartsAtMaxBodyLessThePrefix(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	insertMessage(t, s, store.Message{To: boss, From: store.Daemon, Body: strings.Repeat("c", 20000)})

	got := postQueued(t, s)
	var sizes []int
	for i, p := range got.Posts {
		chunk, ok := strings.CutPrefix(p.Body, fmt.Sprintf("(part %d of %d) ", i+1, len(got.Posts)))
		assert.True(t, ok, "post %d begins with its prefix", i)
		sizes = append(sizes, len(chunk))
	}
	room := store.MaxBody - 32
	assert.Equal(t, []int{room, room, 20000 - 2*room}, sizes, "the bytes of the body each part holds")
}

// TestPostQueuedRefusesARowWholeForALaterPart: every part is checked before
// any is inserted, so a row whose second part the board refuses (a NUL just
// past the first part) leaves none of its parts on the board, only the
// undelivered post in its place.
func TestPostQueuedRefusesARowWholeForALaterPart(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	m := insertMessage(t, s, store.Message{To: boss, From: store.Daemon, Body: strings.Repeat("a", store.MaxBody-32) + "\x00" + strings.Repeat("b", 100)})

	got := postQueued(t, s)
	require.Len(t, got.Posts, 1)
	assert.Equal(t, "undelivered", got.Posts[0].Kind)
	assert.Equal(t, m.ID, got.Posts[0].Subject)
	assert.Equal(t, 1, postCount(t, s), "no part of the row is on the board")
	assert.Empty(t, got.Delivered)
	assert.Equal(t, []string{m.ID}, ids(got.Failed))
}

// TestPostQueuedIsolatesARefusedRow (B1, minor 3): a row the board refuses
// is failed on its own, with an undelivered post in its place under an id of
// its own, while the rows around it are posted, oldest first. Its sender is
// sent no notice: no messages row is written.
func TestPostQueuedIsolatesARefusedRow(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	a := send(t, s, boss, pane, "from A")
	long := store.Address{Kind: store.KindPane, ID: strings.Repeat("p", 300)}
	b := insertMessage(t, s, store.Message{To: boss, From: long, Body: "from B"})
	c := send(t, s, boss, pane, "from C")
	before := count(t, s)

	got := postQueued(t, s)
	assert.Equal(t, before, count(t, s), "no notice, and no other message, is queued")
	assertState(t, s, store.MessageDelivered, a, c)
	failed := message(t, s, b.ID)
	assert.Equal(t, store.MessageFailed, failed.State)
	assert.True(t, strings.HasPrefix(failed.Reason, "the board refused it"), "reason %q", failed.Reason)
	assert.Equal(t, postedAt, failed.UpdatedAt, "B is failed at the call's time")
	assert.Equal(t, []string{a.ID, c.ID}, ids(got.Delivered))
	assert.Equal(t, []store.Message{failed}, got.Failed)

	// Posts holds three posts with rising seqs: A's, then the undelivered
	// post for B, then C's.
	require.Len(t, got.Posts, 3)
	assert.Less(t, got.Posts[0].Seq, got.Posts[1].Seq)
	assert.Less(t, got.Posts[1].Seq, got.Posts[2].Seq)
	assert.Equal(t, a.ID, got.Posts[0].ID, "A's post first")
	assert.Equal(t, c.ID, got.Posts[2].ID, "C's post last")
	undelivered := got.Posts[1]
	assert.Equal(t, b.ID, undelivered.Subject, "the undelivered post for B second")
	assert.Equal(t, store.ChannelDaemon, undelivered.Channel)
	assert.Equal(t, store.Daemon, undelivered.Author)
	assert.Equal(t, "undelivered", undelivered.Kind)
	assert.Equal(t, t1, undelivered.At)
	assert.Equal(t, queueProject, undelivered.Project)
	assert.Len(t, undelivered.ID, 26)
	assert.NotEqual(t, b.ID, undelivered.ID, "an id of its own")
	assert.Equal(t, ulidTime(t, b.CreatedAt), undelivered.ID[:10], "its id carries B's time")
	assert.True(t, strings.HasPrefix(undelivered.Body, fmt.Sprintf("[agentisan] message %s from \"", b.ID)), "body %q", undelivered.Body)
	assert.Equal(t, fmt.Sprintf("[agentisan] message %s from %s could not be posted on the board (%s); it is kept, failed, in state.db",
		b.ID, strconv.Quote(long.ID), failed.Reason), undelivered.Body)
}

// TestPostQueuedWakesForAnUndeliveredPostAlone: an undelivered post counts
// as a post, so a call whose only post is one wakes the board's waiters.
func TestPostQueuedWakesForAnUndeliveredPostAlone(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	insertMessage(t, s, store.Message{To: boss, From: store.Address{Kind: store.KindPane, ID: strings.Repeat("p", 300)}, Body: "refused"})
	changed := s.BoardChanged()

	got := postQueued(t, s)
	require.Len(t, got.Failed, 1)
	require.Len(t, got.Posts, 1)
	select {
	case <-changed:
	default:
		t.Fatal("the undelivered post woke no waiter")
	}
}

// TestUndeliveredPostSkipsTheCheck: the undelivered post is not checked, so
// one whose body passes 8 KiB, quoting a 9,000-byte sender, is still
// posted, and the call does not roll back on every pass. The failed row's
// reason, which quotes the sender too, is CleanReason's, cut to 512 bytes.
func TestUndeliveredPostSkipsTheCheck(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	m := insertMessage(t, s, store.Message{To: boss, From: store.Address{Kind: store.KindPane, ID: strings.Repeat("p", 9000)}, Body: "refused"})

	got, err := s.PostQueued(t.Context(), boss, queueProject, postedAt)
	require.NoError(t, err)
	require.Len(t, got.Posts, 1)
	assert.Equal(t, "undelivered", got.Posts[0].Kind)
	assert.Greater(t, len(got.Posts[0].Body), store.MaxBody, "the post is longer than checkPost takes")
	assertState(t, s, store.MessageFailed, m)
	reason := message(t, s, m.ID).Reason
	assert.Len(t, reason, 512, "the reason is cut to 512 bytes")
	assert.Equal(t, store.CleanReason(reason), reason, "the reason is CleanReason's")
}

// TestPostQueuedTakesAFreshIDWhenTheMessageIDIsTaken (minor 1): a post that
// already holds the message's id does not abort the call on the board's
// UNIQUE id, pass after pass: the post takes a fresh id.
func TestPostQueuedTakesAFreshIDWhenTheMessageIDIsTaken(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "ha-coder-1", "w14:p2", 0)
	m := send(t, s, boss, worker("ha-coder-1"), "hello")
	_, err := store.DB(s).ExecContext(t.Context(), `INSERT INTO board_posts (id, project, channel, author_kind, author, kind, body, created_at)
	VALUES (?, 'other', 'team', 'pane', 'w14:p9', 'post', 'taken', 0)`, m.ID)
	require.NoError(t, err)

	got := postQueued(t, s)
	require.Len(t, got.Posts, 1)
	assert.Len(t, got.Posts[0].ID, 26)
	assert.NotEqual(t, m.ID, got.Posts[0].ID)
	assert.Equal(t, "hello", got.Posts[0].Body)
	assertState(t, s, store.MessageDelivered, m)
}

// TestPostQueuedMapsEveryRowKind: each row becomes the post its table row
// says, the first rule that matches winning, so a daemon row with both
// ReturnedFrom and NoticeOf is returned, and a pane's or a boss's message is
// a team post, as a worker's is; every post is read back as made, and every
// row is delivered.
func TestPostQueuedMapsEveryRowKind(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	for i, id := range []string{"ha-coder-1", "ha-coder-2", "ha-coder-3"} {
		seedWorker(t, s, id, fmt.Sprintf("w14:p%d", i+2), 0)
	}
	r1 := send(t, s, worker("ha-coder-2"), worker("ha-coder-1"), "a worker's, returned")
	r2 := send(t, s, boss, worker("ha-coder-1"), "a worker's post")
	r3 := send(t, s, worker("ha-coder-2"), store.Daemon, "the daemon's, returned")
	failedToWorker := send(t, s, worker("ha-coder-1"), boss, "the boss's message")
	claim(t, s, worker("ha-coder-1"))
	require.NoError(t, s.Fail(t.Context(), failedToWorker.ID, "timeout", t1))
	r5 := send(t, s, boss, store.Daemon, "[agentisan] notices:\nha-coder-2 is blocked")
	failedFromWorker := send(t, s, pane, worker("ha-coder-3"), "a worker's message")
	claim(t, s, pane)
	require.NoError(t, s.Fail(t.Context(), failedFromWorker.ID, "timeout", t1))
	for _, id := range []string{"ha-coder-2", "ha-coder-3"} {
		retire(t, s, id)
		_, err := s.ReturnToBoss(t.Context(), id, "agentisan", boss, t1)
		require.NoError(t, err)
	}
	otherBoss := store.Address{Kind: store.KindBoss, ID: "hb-boss"}
	r7 := send(t, s, boss, pane, "a pane's post")
	r8 := send(t, s, boss, otherBoss, "a boss's post")
	var noticeIDs [2]string
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(),
		"SELECT min(id) FILTER (WHERE notice_of = ?1), min(id) FILTER (WHERE notice_of = ?2) FROM messages",
		failedToWorker.ID, failedFromWorker.ID).Scan(&noticeIDs[0], &noticeIDs[1]))
	r4, r6 := message(t, s, noticeIDs[0]), message(t, s, noticeIDs[1])
	require.Equal(t, failedToWorker.ID, r4.NoticeOf)
	require.Equal(t, []string{failedFromWorker.ID, "ha-coder-3"}, []string{r6.NoticeOf, r6.ReturnedFrom})

	daemonPost := func(m store.Message, kind, subject, body string) store.BoardPost {
		return store.BoardPost{ID: m.ID, Project: queueProject, Channel: store.ChannelDaemon, Author: store.Daemon, Kind: kind, Subject: subject, Body: body, At: t1}
	}
	want := []store.BoardPost{
		{ID: r1.ID, Project: queueProject, Channel: store.ChannelTeam, Author: worker("ha-coder-1"), Kind: "returned", Subject: "ha-coder-2", Body: r1.Body, At: t1},
		{ID: r2.ID, Project: queueProject, Channel: store.ChannelTeam, Author: worker("ha-coder-1"), Kind: "post", Body: r2.Body, At: t1},
		daemonPost(r3, "returned", "ha-coder-2", r3.Body),
		daemonPost(r4, "undelivered", failedToWorker.ID, r4.Body),
		daemonPost(r5, "notices", "", "ha-coder-2 is blocked"),
		daemonPost(r6, "returned", "ha-coder-3", r6.Body),
		{ID: r7.ID, Project: queueProject, Channel: store.ChannelTeam, Author: pane, Kind: "post", Body: r7.Body, At: t1},
		{ID: r8.ID, Project: queueProject, Channel: store.ChannelTeam, Author: otherBoss, Kind: "post", Body: r8.Body, At: t1},
	}

	got := postQueued(t, s)
	require.Len(t, got.Posts, len(want))
	for i := range want {
		want[i].Seq = got.Posts[i].Seq
		if i > 0 {
			assert.Less(t, got.Posts[i-1].Seq, got.Posts[i].Seq)
		}
	}
	assert.Equal(t, want, got.Posts)
	latest, err := s.LatestPosts(t.Context(), queueProject, store.MaxBoardRead)
	require.NoError(t, err)
	require.Len(t, latest, len(want))
	for i := range want {
		assert.Equal(t, want[i], latest[len(latest)-1-i], "post %d reads back as made", i)
	}
	assert.Equal(t, []string{r1.ID, r2.ID, r3.ID, r4.ID, r5.ID, r6.ID, r7.ID, r8.ID}, ids(got.Delivered))
	for _, m := range got.Delivered {
		assert.Equal(t, store.MessageDelivered, m.State)
		assert.Equal(t, postedAt, m.UpdatedAt)
	}
	assertState(t, s, store.MessageDelivered, r1, r2, r3, r4, r5, r6, r7, r8)
}

// TestPostQueuedIsAllOrNone: a statement that fails rolls the whole call
// back, so every row stays queued, no post is left, and nothing is woken.
func TestPostQueuedIsAllOrNone(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	a := send(t, s, boss, pane, "first")
	b := send(t, s, boss, pane, "second")
	c := send(t, s, boss, pane, "third")
	failBoardInserts(t, s, "NEW.body = 'second'")
	changed := s.BoardChanged()

	_, err := s.PostQueued(t.Context(), boss, queueProject, postedAt)
	require.ErrorIs(t, err, store.ErrStore)
	assertState(t, s, store.MessageQueued, a, b, c)
	assert.Zero(t, postCount(t, s), "no post is left")
	select {
	case <-changed:
		t.Fatal("a rolled-back call woke the board's waiters")
	default:
	}
}

// TestPostQueuedRollsBackARowItCannotMark: marking a row delivered or failed
// must return the row; when a trigger leaves it as it was, the whole call
// rolls back with ErrStore, the row still queued, no post left and no waiter
// woken, so the next pass cannot post it twice.
func TestPostQueuedRollsBackARowItCannotMark(t *testing.T) {
	t.Parallel()
	for state, from := range map[string]store.Address{
		store.MessageDelivered: pane,
		store.MessageFailed:    {Kind: store.KindPane, ID: strings.Repeat("p", 300)},
	} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			m := insertMessage(t, s, store.Message{To: boss, From: from, Body: "unmarked"})
			_, err := store.DB(s).ExecContext(t.Context(), "CREATE TEMP TRIGGER keep_queued BEFORE UPDATE ON messages WHEN NEW.state = '"+state+
				"' BEGIN SELECT RAISE(IGNORE); END")
			require.NoError(t, err)
			changed := s.BoardChanged()

			_, err = s.PostQueued(t.Context(), boss, queueProject, postedAt)
			require.ErrorIs(t, err, store.ErrStore)
			assertState(t, s, store.MessageQueued, m)
			assert.Zero(t, postCount(t, s), "no post is left")
			select {
			case <-changed:
				t.Fatal("a rolled-back call woke the board's waiters")
			default:
			}
		})
	}
}

// TestPostQueuedWithTheClockBehindTheRows: a call whose time is before the
// rows' (the clock stepped back) still posts and fails them: each row's
// updated_at is the later of the call's time and its created_at, never below
// created_at, which the messages table refuses, pass after pass.
func TestPostQueuedWithTheClockBehindTheRows(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	sent := send(t, s, boss, pane, "posted")
	refused := insertMessage(t, s, store.Message{To: boss, From: store.Address{Kind: store.KindPane, ID: strings.Repeat("p", 300)}, Body: "refused"})

	got, err := s.PostQueued(t.Context(), boss, queueProject, t1.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []string{sent.ID}, ids(got.Delivered))
	assert.Equal(t, []string{refused.ID}, ids(got.Failed))
	for _, m := range append(got.Delivered, got.Failed...) {
		assert.Equal(t, t1, m.UpdatedAt, "message %q keeps updated_at at its created_at", m.Body)
	}
}

// TestPostQueuedPostsInSendOrder: the queue is taken by seq, the order the
// rows were sent in, not by their times: a row sent after the clock stepped
// back is still posted after the one sent before it.
func TestPostQueuedPostsInSendOrder(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	_, err := s.Send(t.Context(), boss, pane, "first", t1.Add(time.Hour))
	require.NoError(t, err)
	send(t, s, boss, pane, "second")

	got := postQueued(t, s)
	assert.Equal(t, []string{"first", "second"}, bodies(got.Posts))
}

// TestPostQueuedTakesAnEmptyProject: an empty project is allowed, as it is
// for any post.
func TestPostQueuedTakesAnEmptyProject(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	m := send(t, s, boss, pane, "one")

	got, err := s.PostQueued(t.Context(), boss, "", postedAt)
	require.NoError(t, err)
	require.Len(t, got.Posts, 1)
	assert.Empty(t, got.Posts[0].Project)
	assertState(t, s, store.MessageDelivered, m)
}

// TestPostQueuedLeavesDispatchingAlone: a message in flight is the
// dispatcher's: it is neither posted nor moved.
func TestPostQueuedLeavesDispatchingAlone(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	inFlight := send(t, s, boss, pane, "in flight")
	claim(t, s, boss)
	send(t, s, boss, pane, "one")
	send(t, s, boss, pane, "two")

	got := postQueued(t, s)
	assert.Equal(t, []string{"one", "two"}, bodies(got.Posts))
	assertState(t, s, store.MessageDispatching, inFlight)
}

// TestPostQueuedOnlyThatBoss: only the boss's own queue is posted, not
// another boss's, nor a worker's or a pane's that bears its id.
func TestPostQueuedOnlyThatBoss(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, boss.ID, "w14:p2", 0)
	others := []store.Message{
		send(t, s, store.Address{Kind: store.KindBoss, ID: "hb-boss"}, pane, "for another boss"),
		send(t, s, worker(boss.ID), pane, "for a worker"),
		send(t, s, store.Address{Kind: store.KindPane, ID: boss.ID}, pane, "for a pane"),
	}
	mine := send(t, s, boss, pane, "for ha-boss")

	got := postQueued(t, s)
	require.Len(t, got.Posts, 1)
	assert.Equal(t, mine.ID, got.Posts[0].ID)
	assert.Equal(t, 1, postCount(t, s))
	assertState(t, s, store.MessageQueued, others...)
}

// TestPostQueuedOneWake: a call that posts wakes the board's waiters, its
// last post the board's newest; a call with nothing queued wakes none.
func TestPostQueuedOneWake(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	send(t, s, boss, pane, "one")
	send(t, s, boss, pane, "two")
	changed := s.BoardChanged()

	got := postQueued(t, s)
	require.Len(t, got.Posts, 2)
	select {
	case <-changed:
	default:
		t.Fatal("the posts woke no waiter")
	}
	st, err := s.BoardStatus(t.Context(), queueProject, store.ReaderBoss)
	require.NoError(t, err)
	assert.Equal(t, got.Posts[1].Seq, st.Newest)

	changed = s.BoardChanged()
	got = postQueued(t, s)
	assert.Empty(t, got.Posts)
	select {
	case <-changed:
		t.Fatal("a call that posted nothing woke the board's waiters")
	default:
	}
}

// TestPostQueuedKeepsTheMessageID: a message posted whole keeps its id.
func TestPostQueuedKeepsTheMessageID(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "ha-coder-1", "w14:p2", 0)
	m := send(t, s, boss, worker("ha-coder-1"), "hello")

	got := postQueued(t, s)
	require.Len(t, got.Posts, 1)
	assert.Equal(t, m.ID, got.Posts[0].ID)
}

// TestPostQueuedDropsTheBareNoticesHeader: an A44 notice's bare header is
// dropped; a header with a count is information and stays; a header with
// nothing after it stays, since an empty body would fail the row.
func TestPostQueuedDropsTheBareNoticesHeader(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ms := []store.Message{
		send(t, s, boss, store.Daemon, "[agentisan] notices:\na\nb"),
		send(t, s, boss, store.Daemon, "[agentisan] notices (3 earlier dropped):\na"),
		send(t, s, boss, store.Daemon, "[agentisan] notices:"),
		send(t, s, boss, store.Daemon, "[agentisan] notices:\n"),
	}

	got := postQueued(t, s)
	assert.Equal(t, []string{"a\nb", ms[1].Body, ms[2].Body, ms[3].Body}, bodies(got.Posts))
	assert.Empty(t, got.Failed)
	assertState(t, s, store.MessageDelivered, ms...)
}

// TestPostQueuedTakesTheLongestLegalRow: the longest body Send takes, from
// the longest sender id, fits one post: it is not split.
func TestPostQueuedTakesTheLongestLegalRow(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	from := store.Address{Kind: store.KindPane, ID: strings.Repeat("p", 256)}
	m := send(t, s, boss, from, strings.Repeat("z", store.MaxBody))

	got := postQueued(t, s)
	require.Len(t, got.Posts, 1)
	assert.Equal(t, m.Body, got.Posts[0].Body)
	assert.Equal(t, from, got.Posts[0].Author)
	assertState(t, s, store.MessageDelivered, m)
}

// TestPostQueuedRefusesANonBoss: posts are made from a boss's queue, under
// a boss id, project and time the store takes; anything else is ErrInvalid,
// with nothing posted.
func TestPostQueuedRefusesANonBoss(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "ha-coder-1", "w14:p2", 0)
	m := send(t, s, boss, pane, "queued")
	tests := map[string]struct {
		to      store.Address
		project string
		at      time.Time
	}{
		"a worker":                                  {to: worker("ha-coder-1"), project: queueProject, at: postedAt},
		"an empty boss id":                          {to: store.Address{Kind: store.KindBoss}, project: queueProject, at: postedAt},
		"a boss id with a control byte":             {to: store.Address{Kind: store.KindBoss, ID: "ha\x1bboss"}, project: queueProject, at: postedAt},
		"a project with a line break":               {to: boss, project: "agent\nisan", at: postedAt},
		"a time nanoseconds since 1970 cannot hold": {to: boss, project: queueProject, at: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for name, tc := range tests {
		_, err := s.PostQueued(t.Context(), tc.to, tc.project, tc.at)
		require.ErrorIs(t, err, store.ErrInvalid, name)
	}
	assert.Zero(t, postCount(t, s), "nothing is posted")
	assertState(t, s, store.MessageQueued, m)
}

// postQueuedFile is PostQueued's own file, relative to the module root.
const postQueuedFile = "internal/store/boardqueue.go"

// postQueuedFuncs is the name of every function and method postQueuedFile
// declares: PostQueued and its helpers.
func postQueuedFuncs(t *testing.T, root string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, postQueuedFile), nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			names[fn.Name.Name] = true
		}
	}
	return names
}

// TestPostQueuedHasNoProductionCaller: PostQueued is dormant until PR 1b's
// dispatcher calls it (NERD-5533, A74), which deletes this test. No non-test
// file in internal/ or cmd/ but its own uses the name, a selector, a method
// value or an interface's method alike; nor does any other non-test file of
// internal/store use a function its file declares, postQueued or a helper,
// which could post a queue without the name.
func TestPostQueuedHasNoProductionCaller(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	funcs := postQueuedFuncs(t, root)
	for _, name := range []string{"PostQueued", "postQueued", "postRow", "rowPosts", "postIDTaken", "splitBody", "refuseRow", "insertBoardPost"} {
		assert.True(t, funcs[name], "%s declares %s", postQueuedFile, name)
	}
	storeDir := filepath.Join(root, "internal", "store")
	uses := map[string][]string{}
	parsed := 0
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == "testdata":
				return filepath.SkipDir
			case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			parsed++
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			// An unexported name is the store's own function only in the
			// store's package; elsewhere it is another package's name.
			inStore := filepath.Dir(path) == storeDir
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && funcs[id.Name] && (id.IsExported() || inStore) {
					uses[filepath.ToSlash(rel)] = append(uses[filepath.ToSlash(rel)], id.Name+" at "+fset.Position(id.Pos()).String())
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	assert.Greater(t, parsed, 100, "the scan reached the module's packages")
	assert.NotEmpty(t, uses[postQueuedFile], "the scan finds PostQueued's functions in %s", postQueuedFile)
	delete(uses, postQueuedFile)
	assert.Empty(t, uses, "PostQueued and its helpers have no production caller until PR 1b")
}

package store_test

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// t1 is when the messaging tests' messages are sent.
var t1 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func worker(id string) store.Address { return store.Address{Kind: store.KindWorker, ID: id} }

var (
	boss = store.Address{Kind: store.KindBoss, ID: "ha-boss"}
	pane = store.Address{Kind: store.KindPane, ID: "w14:p9"}
)

// seedWorker binds id to pane, on its terminal, at generation gen, live, the
// way the workforce commands (NERD-5254) write the row.
func seedWorker(t *testing.T, s *store.Store, id, pane string, gen int64) {
	t.Helper()
	_, err := store.DB(s).ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, terminal_id) VALUES (?, 'agentisan', 'coders', ?, ?, 'active', 0, ?)",
		id, pane, gen, terminal(pane))
	require.NoError(t, err)
}

// terminal is the terminal the tests give pane.
func terminal(pane string) string { return "term_" + pane }

// retire ends id's binding, as `worker retire` does.
func retire(t *testing.T, s *store.Store, id string) {
	t.Helper()
	_, err := store.DB(s).ExecContext(t.Context(), "UPDATE workers SET status = 'retired', retired_at = 1 WHERE logical_id = ?", id)
	require.NoError(t, err)
}

// reuse binds a retired id again, at the next generation, as BindWorker does.
func reuse(t *testing.T, s *store.Store, id string) {
	t.Helper()
	_, err := store.DB(s).ExecContext(t.Context(),
		"UPDATE workers SET generation = generation + 1, status = 'starting', retired_at = NULL WHERE logical_id = ?", id)
	require.NoError(t, err)
}

func send(t *testing.T, s *store.Store, to, from store.Address, body string) store.Message {
	t.Helper()
	m, err := s.Send(t.Context(), to, from, body, t1)
	require.NoError(t, err)
	return m
}

// claim claims the next message to `to`, which must be there.
func claim(t *testing.T, s *store.Store, to store.Address) store.Message {
	t.Helper()
	m, ok, err := s.Claim(t.Context(), to, t1)
	require.NoError(t, err)
	require.True(t, ok, "a message to %v was claimable", to)
	return m
}

func noClaim(t *testing.T, s *store.Store, to store.Address, why string) {
	t.Helper()
	m, ok, err := s.Claim(t.Context(), to, t1)
	require.NoError(t, err)
	require.False(t, ok, "%s, but claimed %+v", why, m)
}

func message(t *testing.T, s *store.Store, id string) store.Message {
	t.Helper()
	m, err := s.Message(t.Context(), id)
	require.NoError(t, err)
	return m
}

// TestSendQueuesForTheBinding: a message to a worker takes the project and
// generation of its live row, so it belongs to that binding; one to the boss
// or a pane has neither. Every message starts queued, with no attempt, and
// reads back as sent.
func TestSendQueuesForTheBinding(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "ha-coder-1", "w14:p2", 3)
	seedWorker(t, s, "ha-coder-2", "w14:p3", 0)

	tests := map[string]struct {
		to, from store.Address
		project  string
		gen      int64
	}{
		"to a worker, from a worker":  {to: worker("ha-coder-1"), from: worker("ha-coder-2"), project: "agentisan", gen: 3},
		"to the boss":                 {to: boss, from: worker("ha-coder-1")},
		"to a pane, from the daemon":  {to: pane, from: store.Daemon},
		"a non-ASCII id with a space": {to: store.Address{Kind: store.KindPane, ID: "◆ pané 1"}, from: store.Daemon},
		"an id of exactly 256 bytes":  {to: store.Address{Kind: store.KindPane, ID: strings.Repeat("p", 256)}, from: store.Daemon},
		"from a pane":                 {to: worker("ha-coder-2"), from: pane, project: "agentisan"},
		"from the boss":               {to: worker("ha-coder-1"), from: boss, project: "agentisan", gen: 3},
	}
	for name, tc := range tests {
		// A byte-order mark and CRLF are text: the store keeps them byte for
		// byte. How a body is typed into a pane is the dispatcher's.
		got := send(t, s, tc.to, tc.from, "\uFEFFhello\r\nwörld")
		assert.Len(t, got.ID, 26, name)
		want := store.Message{
			ID: got.ID, To: tc.to, Project: tc.project, Generation: tc.gen, From: tc.from, Body: "\uFEFFhello\r\nwörld",
			State: store.MessageQueued, CreatedAt: t1, UpdatedAt: t1,
		}
		assert.Equal(t, want, got, name)
		assert.Equal(t, want, message(t, s, got.ID), name+": reads back as sent")
	}
}

// TestSendRefuses: what the inbox would hold wrong is refused before it is
// queued, and the error never repeats the body.
func TestSendRefuses(t *testing.T) {
	t.Parallel()
	const private = "a private body"
	tests := map[string]struct {
		to, from store.Address
		body     string
		at       time.Time
		want     error
	}{
		"a worker with no row":        {to: worker("nobody"), want: store.ErrNotBound},
		"a retired worker":            {to: worker("gone"), want: store.ErrNotBound},
		"from a worker with no row":   {from: worker("nobody"), want: store.ErrNotBound},
		"from a retired worker":       {from: worker("gone"), want: store.ErrNotBound},
		"to the daemon":               {to: store.Daemon, want: store.ErrInvalid},
		"to an unknown kind":          {to: store.Address{Kind: "team", ID: "x"}, want: store.ErrInvalid},
		"from an unknown kind":        {from: store.Address{Kind: "", ID: "x"}, want: store.ErrInvalid},
		"an empty recipient":          {to: worker(""), want: store.ErrInvalid},
		"a control byte in an id":     {to: worker("ha\x1b[2Jcoder"), want: store.ErrInvalid},
		"a NUL in an id":              {to: worker("ha\x00coder"), want: store.ErrInvalid},
		"a line break in an id":       {to: worker("ha-coder\r\n"), want: store.ErrInvalid},
		"a byte-order mark in an id":  {from: store.Address{Kind: store.KindPane, ID: "\uFEFFw14:p1"}, want: store.ErrInvalid},
		"invalid UTF-8 in an id":      {to: worker("ha\xffcoder"), want: store.ErrInvalid},
		"an id past 256 bytes":        {to: worker(strings.Repeat("a", 257)), want: store.ErrInvalid},
		"an empty body":               {body: "-", want: store.ErrInvalid},
		"a body one byte past 8 KiB":  {body: private + strings.Repeat("x", store.MaxBody-len(private)+1), want: store.ErrTooLarge},
		"a body of invalid UTF-8":     {body: private + "\xff", want: store.ErrInvalid},
		"a NUL in the body":           {body: private + "\x00", want: store.ErrInvalid},
		"the zero time":               {at: time.Time{}, want: store.ErrInvalid},
		"a time before 1970 (a ULID)": {at: time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC), want: store.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			seedWorker(t, s, "ha-coder-1", "w14:p2", 0)
			seedWorker(t, s, "gone", "w14:p3", 0)
			retire(t, s, "gone")
			to, from, body, at := tc.to, tc.from, tc.body, tc.at
			if to == (store.Address{}) {
				to = worker("ha-coder-1")
			}
			if from == (store.Address{}) {
				from = boss
			}
			switch body {
			case "":
				body = private
			case "-":
				body = ""
			}
			if at.Equal(time.Time{}) && name != "the zero time" {
				at = t1
			}

			_, err := s.Send(t.Context(), to, from, body, at)
			require.ErrorIs(t, err, tc.want)
			assert.NotContains(t, err.Error(), private, "the body is never echoed")
			var n int
			require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
			assert.Zero(t, n, "nothing was queued")
		})
	}

	t.Run("exactly 8 KiB is taken", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		m := send(t, s, boss, pane, strings.Repeat("é", store.MaxBody/2))
		assert.Len(t, m.Body, store.MaxBody)
		assert.Equal(t, 8<<10, store.MaxBody)
	})
}

// TestClaimIsFIFOAndOneAtATime: a recipient's messages are claimed oldest
// first, one at a time: while one is dispatching no other is claimable, so
// at most one prompt is ever in flight to it. Each claim counts an attempt.
// Recipients do not hold each other up.
func TestClaimIsFIFOAndOneAtATime(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "w", "w14:p2", 0)
	var sent []string
	for _, body := range []string{"one", "two", "three"} {
		sent = append(sent, send(t, s, worker("w"), boss, body).ID)
	}
	other := send(t, s, boss, worker("w"), "to the boss")

	var got []string
	for range sent {
		m := claim(t, s, worker("w"))
		assert.Equal(t, store.MessageDispatching, m.State)
		assert.Equal(t, 1, m.Attempts)
		noClaim(t, s, worker("w"), "one message is already in flight")
		assert.Equal(t, other.ID, claim(t, s, boss).ID, "another recipient is not held up")
		require.NoError(t, s.Requeue(t.Context(), other.ID, "agent_blocked", t1))
		require.NoError(t, s.Delivered(t.Context(), m.ID, t1))
		got = append(got, m.ID)
	}
	assert.Equal(t, sent, got, "oldest first")
	noClaim(t, s, worker("w"), "every message was delivered")
	assert.Equal(t, store.MessageDelivered, message(t, s, sent[0]).State)
}

// TestClaimFollowsTheSeqNotTheClock: FIFO is the order messages were queued
// in, even when the clock steps back between two sends and their ULIDs sort
// the other way.
func TestClaimFollowsTheSeqNotTheClock(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	first, err := s.Send(t.Context(), boss, pane, "first", t1)
	require.NoError(t, err)
	second, err := s.Send(t.Context(), boss, pane, "second", t1.Add(-time.Hour))
	require.NoError(t, err)
	require.Less(t, second.ID, first.ID, "the ULIDs sort the other way")
	assert.Equal(t, first.ID, claim(t, s, boss).ID)
}

// TestClaimIsAtomicUnderConcurrentClaimers: goroutines sending to one
// recipient while others claim and deliver its messages: every message is
// queued with an id of its own and claimed exactly once.
func TestClaimIsAtomicUnderConcurrentClaimers(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	const n = 40
	var mu sync.Mutex
	claims := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range n / 4 {
				_, err := s.Send(t.Context(), boss, pane, "m", t1)
				assert.NoError(t, err)
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for {
				mu.Lock()
				done := len(claims) == n
				mu.Unlock()
				if done {
					return
				}
				m, ok, err := s.Claim(t.Context(), boss, t1)
				if !assert.NoError(t, err) {
					return
				}
				if !ok {
					continue
				}
				mu.Lock()
				claims[m.ID]++
				mu.Unlock()
				assert.NoError(t, s.Delivered(t.Context(), m.ID, t1))
			}
		})
	}
	wg.Wait()
	require.Len(t, claims, n)
	for id, c := range claims {
		assert.Equal(t, 1, c, "%s claimed once", id)
	}
}

// TestTransitions: from dispatching, a message is delivered, or requeued
// when it was certainly not sent (attempts keep counting), or failed when it
// may have been. Each is refused from any other state, so a late answer can
// never move a message twice.
func TestTransitions(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	m := send(t, s, boss, pane, "hi")
	ctx := t.Context()

	require.ErrorIs(t, s.Delivered(ctx, m.ID, t1), store.ErrNoMessage, "a queued message is not delivered")
	require.ErrorIs(t, s.Fail(ctx, m.ID, "timeout", t1), store.ErrNoMessage)
	require.ErrorIs(t, s.Requeue(ctx, m.ID, "x", t1), store.ErrNoMessage)

	claim(t, s, boss)
	later := t1.Add(time.Minute)
	require.NoError(t, s.Requeue(ctx, m.ID, "agent_not_ready", later))
	got := message(t, s, m.ID)
	assert.Equal(t, store.MessageQueued, got.State)
	assert.Equal(t, "agent_not_ready", got.Reason)
	assert.True(t, later.Equal(got.UpdatedAt))
	assert.Equal(t, 2, claim(t, s, boss).Attempts, "the next claim is the second attempt")

	require.NoError(t, s.Delivered(ctx, m.ID, t1.Add(-time.Hour)), "a clock behind the message's own time")
	got = message(t, s, m.ID)
	assert.Equal(t, store.MessageDelivered, got.State)
	assert.True(t, got.UpdatedAt.Equal(got.CreatedAt), "updated_at never precedes created_at")
	require.ErrorIs(t, s.Delivered(ctx, m.ID, t1), store.ErrNoMessage, "delivered once")
	require.ErrorIs(t, s.Fail(ctx, m.ID, "timeout", t1), store.ErrNoMessage)
	require.ErrorIs(t, s.Delivered(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV", t1), store.ErrNoMessage, "no such message")
	_, err := s.Message(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	require.ErrorIs(t, err, store.ErrNoMessage)
	require.ErrorIs(t, s.Requeue(ctx, m.ID, "x", time.Time{}), store.ErrInvalid)
}

// TestFailNotifiesTheSender is the crash rule's other half (A2): a message
// that may not have arrived is failed, never retried, and its sender is sent
// a notice carrying its id, recipient, the reason and the body, in the same
// transaction. A notice that fails is not itself reported (no loop), and
// nothing is sent back to the daemon.
func TestFailNotifiesTheSender(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "a", "w14:p2", 2)
	seedWorker(t, s, "b", "w14:p3", 0)
	ctx := t.Context()

	for name, from := range map[string]store.Address{"a worker": worker("a"), "the boss": boss, "a pane": pane} {
		m := send(t, s, worker("b"), from, "the body")
		claim(t, s, worker("b"))
		require.NoError(t, s.Fail(ctx, m.ID, "timeout", t1), name)
		failed := message(t, s, m.ID)
		assert.Equal(t, store.MessageFailed, failed.State, name)
		assert.Equal(t, "timeout", failed.Reason, name)
		noClaim(t, s, worker("b"), name+": a failed message is never redelivered")

		notice := claim(t, s, from)
		assert.Equal(t, store.Daemon, notice.From, name)
		assert.Equal(t, m.ID, notice.NoticeOf, name)
		assert.Equal(t, "[agentisan] message "+m.ID+" to b may not have been delivered (timeout); resend if needed: the body", notice.Body, name)
		if from.Kind == store.KindWorker {
			assert.Equal(t, int64(2), notice.Generation, "the notice goes to the binding that sent it")
			assert.Equal(t, "agentisan", notice.Project)
		}
		require.NoError(t, s.Fail(ctx, notice.ID, "timeout", t1), name)
		noClaim(t, s, from, name+": a failed notice is not reported again")
	}

	m := send(t, s, worker("b"), store.Daemon, "from the daemon")
	claim(t, s, worker("b"))
	require.NoError(t, s.Fail(ctx, m.ID, "timeout", t1))
	var n int
	require.NoError(t, store.DB(s).QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE notice_of = ?", m.ID).Scan(&n))
	assert.Zero(t, n, "nothing goes back to the daemon")

	long := send(t, s, worker("b"), boss, "x")
	claim(t, s, worker("b"))
	require.NoError(t, s.Fail(ctx, long.ID, "x\x1b[2J"+strings.Repeat("é", 600)+"\xff", t1))
	reason := message(t, s, long.ID).Reason
	assert.LessOrEqual(t, len(reason), 512, "a long reason is cut, never refused")
	assert.True(t, utf8.ValidString(reason), "cut on a character boundary, invalid bytes replaced")
	assert.True(t, strings.HasPrefix(reason, "x [2Jé"), "control characters become spaces: %q", reason[:8])
}

// TestRecoverDispatchingAfterACrash is the crash rule (A2) on the store: a
// store closed while a message is dispatching leaves the row so, as a crash
// does. The next start fails it and notifies its sender; it is never claimed
// again, and the messages behind it go on.
func TestRecoverDispatchingAfterACrash(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	s, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	seedWorker(t, s, "w", "w14:p2", 0)
	inFlight := send(t, s, worker("w"), boss, "in flight")
	next := send(t, s, worker("w"), boss, "next")
	claim(t, s, worker("w"))
	require.NoError(t, s.Close()) // the daemon dies here

	s = open(t, path)
	n, err := s.RecoverDispatching(t.Context(), "the daemon stopped mid-dispatch", t1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	got := message(t, s, inFlight.ID)
	assert.Equal(t, store.MessageFailed, got.State)
	assert.Equal(t, "the daemon stopped mid-dispatch", got.Reason)
	assert.Equal(t, next.ID, claim(t, s, worker("w")).ID, "never redelivered: the next message is")
	notice := claim(t, s, boss)
	assert.Equal(t, inFlight.ID, notice.NoticeOf)

	require.NoError(t, s.Delivered(t.Context(), next.ID, t1))
	n, err = s.RecoverDispatching(t.Context(), "again", t1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "the notice in flight is failed too, with no notice of its own")
	n, err = s.RecoverDispatching(t.Context(), "again", t1)
	require.NoError(t, err)
	assert.Zero(t, n)
	noClaim(t, s, boss, "no notice of a notice")
}

// refuseNotices makes the schema refuse every notice, so the notice a failure
// queues fails after the failed row was written in the same call. A TEMP
// trigger lives on one connection, and the store holds exactly one.
func refuseNotices(t *testing.T, s *store.Store) {
	t.Helper()
	_, err := store.DB(s).ExecContext(t.Context(),
		"CREATE TEMP TRIGGER refuse_notices BEFORE INSERT ON messages WHEN NEW.notice_of <> '' BEGIN SELECT RAISE(ABORT, 'notice refused'); END")
	require.NoError(t, err)
}

// TestFailIsOneTransaction: a notice that cannot be queued leaves the message
// dispatching, not failed with no notice.
func TestFailIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	m := send(t, s, boss, pane, "the private body")
	claim(t, s, boss)
	refuseNotices(t, s)
	err := s.Fail(t.Context(), m.ID, "timeout", t1)
	require.ErrorIs(t, err, store.ErrStore)
	assert.NotContains(t, err.Error(), "the private body")
	assert.Equal(t, store.MessageDispatching, message(t, s, m.ID).State, "rolled back whole")
	noClaim(t, s, pane, "no notice")
}

// TestRecoverDispatchingIsOneTransaction: the same for the crash rule.
func TestRecoverDispatchingIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	p1 := store.Address{Kind: store.KindPane, ID: "w14:p1"}
	a := send(t, s, boss, pane, "the private body")
	b := send(t, s, p1, pane, "another")
	claim(t, s, boss)
	claim(t, s, p1)
	refuseNotices(t, s)
	_, err := s.RecoverDispatching(t.Context(), "crash", t1)
	require.ErrorIs(t, err, store.ErrStore)
	assert.Equal(t, store.MessageDispatching, message(t, s, a.ID).State, "rolled back whole")
	assert.Equal(t, store.MessageDispatching, message(t, s, b.ID).State, "rolled back whole")
	noClaim(t, s, pane, "no notice")
}

// TestReturnToBoss: a worker's queued messages go to the boss once the
// binding they were sent to has ended, when the row is retired or the id
// was reused at a later generation, and keep their ids and order. A reused
// id keeps only what was sent to it. A live binding, a message in flight,
// and a queue asked for under another project keep theirs.
func TestReturnToBoss(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 0)
	old1 := send(t, s, worker("w"), pane, "old 1")
	old2 := send(t, s, worker("w"), pane, "old 2")

	n, err := s.ReturnToBoss(ctx, "w", "agentisan", boss, t1)
	require.NoError(t, err)
	assert.Zero(t, n, "the binding is live")

	inFlight := send(t, s, worker("w"), pane, "in flight")
	_, err = store.DB(s).ExecContext(ctx, "UPDATE messages SET state = 'dispatching' WHERE id = ?", inFlight.ID)
	require.NoError(t, err)
	retire(t, s, "w")
	reuse(t, s, "w")
	fresh := send(t, s, worker("w"), pane, "fresh")
	assert.Equal(t, int64(1), fresh.Generation)

	n, err = s.ReturnToBoss(ctx, "w", "elsewhere", boss, t1)
	require.NoError(t, err)
	assert.Zero(t, n, "another project's ended queue goes to that project's boss")
	n, err = s.ReturnToBoss(ctx, "w", "agentisan", boss, t1)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	for _, id := range []string{old1.ID, old2.ID} {
		got := message(t, s, id)
		assert.Equal(t, boss, got.To)
		assert.Equal(t, "w", got.ReturnedFrom)
		assert.Zero(t, got.Generation)
		assert.Equal(t, "agentisan", got.Project, "the project of the worker it was sent to")
	}
	assert.Equal(t, old1.ID, claim(t, s, boss).ID, "in their order")
	assert.Equal(t, store.MessageDispatching, message(t, s, inFlight.ID).State, "in flight stays")
	assert.Equal(t, worker("w"), message(t, s, fresh.ID).To, "the new binding keeps what was sent to it")

	_, err = s.ReturnToBoss(ctx, "w", "agentisan", worker("x"), t1)
	require.ErrorIs(t, err, store.ErrInvalid, "only a boss takes them back")
}

// TestPruneMessages is A1's retention: a delivered or failed message is
// deleted once its last change is more than 14 days old; a queued or
// dispatching one never is.
func TestPruneMessages(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	tests := map[string]struct {
		state   string
		updated time.Duration // before now
		pruned  bool
	}{
		"delivered 15 days ago":            {state: "delivered", updated: 15 * day, pruned: true},
		"failed 15 days ago":               {state: "failed", updated: 15 * day, pruned: true},
		"delivered exactly 14 days ago":    {state: "delivered", updated: 14 * day},
		"delivered 14 days and 1 ns ago":   {state: "delivered", updated: 14*day + 1, pruned: true},
		"failed an hour ago":               {state: "failed", updated: time.Hour},
		"queued for 30 days":               {state: "queued", updated: 30 * day},
		"dispatching since 30 days ago":    {state: "dispatching", updated: 30 * day},
		"delivered a day ago, sent 20 ago": {state: "delivered", updated: day},
	}
	s := open(t, dbPath(t))
	want := 0
	for name, tc := range tests {
		m := send(t, s, store.Address{Kind: store.KindPane, ID: name}, boss, "x")
		at := now.Add(-tc.updated).UnixNano()
		_, err := store.DB(s).ExecContext(t.Context(),
			"UPDATE messages SET state = ?, created_at = ?, updated_at = ? WHERE id = ?", tc.state, at-int64(day), at, m.ID)
		require.NoError(t, err, name)
		if tc.pruned {
			want++
		}
	}

	n, err := s.PruneMessages(t.Context(), now)
	require.NoError(t, err)
	assert.EqualValues(t, want, n)
	for name, tc := range tests {
		var c int
		require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE recipient = ?", name).Scan(&c))
		assert.Equal(t, !tc.pruned, c == 1, name)
	}
	n, err = s.PruneMessages(t.Context(), now)
	require.NoError(t, err)
	assert.Zero(t, n, "pruning again finds nothing")
	_, err = s.PruneMessages(t.Context(), time.Time{})
	require.ErrorIs(t, err, store.ErrInvalid)
	assert.Equal(t, 14*day, store.MessageRetention)
}

// TestMessagesSchemaConstraints: the schema itself refuses a second message
// in flight to one recipient, a generation that does not match the kind of
// address, a message to the daemon, and an id that is no ULID.
func TestMessagesSchemaConstraints(t *testing.T) {
	t.Parallel()
	const insert = "INSERT INTO messages (id, recipient_kind, recipient, project, generation, sender_kind, sender, sender_generation, body, state, created_at, updated_at) VALUES "
	tests := map[string]struct {
		sql string
		ok  bool
	}{
		"a message":                   {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'worker', 'w', 'p', 0, 'boss', 'b', NULL, 'x', 'queued', 1, 1)", ok: true},
		"a second one in flight":      {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'worker', 'w', 'p', 0, 'boss', 'b', NULL, 'x', 'dispatching', 1, 1)"},
		"in flight to another":        {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'boss', 'w', '', NULL, 'boss', 'b', NULL, 'x', 'dispatching', 1, 1)", ok: true},
		"a worker with no generation": {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'worker', 'w', 'p', NULL, 'boss', 'b', NULL, 'x', 'queued', 1, 1)"},
		"a boss with a generation":    {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'boss', 'b', '', 0, 'boss', 'b', NULL, 'x', 'queued', 1, 1)"},
		"a worker sender unpinned":    {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'boss', 'b', '', NULL, 'worker', 'w', NULL, 'x', 'queued', 1, 1)"},
		"to the daemon":               {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'daemon', 'd', '', NULL, 'boss', 'b', NULL, 'x', 'queued', 1, 1)"},
		"an id that is no ULID":       {sql: insert + "('m1', 'boss', 'b', '', NULL, 'boss', 'b', NULL, 'x', 'queued', 1, 1)"},
		"a state of its own":          {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'boss', 'b', '', NULL, 'boss', 'b', NULL, 'x', 'sent', 1, 1)"},
		"an empty body":               {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'boss', 'b', '', NULL, 'boss', 'b', NULL, '', 'queued', 1, 1)"},
		"updated before it was sent":  {sql: insert + "('01ARZ3NDEKTSV4RRFFQ69G5FA2', 'boss', 'b', '', NULL, 'boss', 'b', NULL, 'x', 'queued', 5, 4)"},
		"a second pending handoff":    {sql: "INSERT INTO handoffs (logical_id, old_pane, new_pane, created_at, updated_at) VALUES ('w', 'w1:p1', 'w1:p3', 1, 1)"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			db := store.DB(s)
			seedWorker(t, s, "w", "w1:p1", 0)
			_, err := db.ExecContext(t.Context(), insert+"('01ARZ3NDEKTSV4RRFFQ69G5FA1', 'worker', 'w', 'p', 0, 'boss', 'b', NULL, 'x', 'dispatching', 1, 1)")
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), "INSERT INTO handoffs (logical_id, old_pane, new_pane, created_at, updated_at) VALUES ('w', 'w1:p1', 'w1:p2', 1, 1)")
			require.NoError(t, err)

			_, err = db.ExecContext(t.Context(), tc.sql)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

// TestULID: 26 Crockford base32 characters, the first ten the millisecond,
// so ids sort by the time they were made; the rest from the entropy given.
func TestULID(t *testing.T) {
	t.Parallel()
	at := time.UnixMilli(1469918176385).UTC()
	id, err := store.NewULID(at, strings.NewReader(strings.Repeat("\xff", 10)))
	require.NoError(t, err)
	assert.Equal(t, "01ARYZ6S41ZZZZZZZZZZZZZZZZ", id)
	zero, err := store.NewULID(time.UnixMilli(0), strings.NewReader(strings.Repeat("\x00", 10)))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("0", 26), zero)

	a, err := store.NewULID(at, strings.NewReader(strings.Repeat("\xff", 10)))
	require.NoError(t, err)
	b, err := store.NewULID(at.Add(time.Millisecond), strings.NewReader(strings.Repeat("\x00", 10)))
	require.NoError(t, err)
	assert.Less(t, a, b, "a later millisecond sorts after, whatever the entropy")

	_, err = store.NewULID(at, strings.NewReader("short"))
	require.ErrorIs(t, err, store.ErrStore, "entropy that runs out is an error")
	_, err = store.NewULID(time.UnixMilli(-1), strings.NewReader(strings.Repeat("\x00", 10)))
	require.ErrorIs(t, err, store.ErrInvalid)
	_, err = store.NewULID(time.UnixMilli(1<<48), strings.NewReader(strings.Repeat("\x00", 10)))
	require.ErrorIs(t, err, store.ErrInvalid, "past the 48-bit millisecond")
}

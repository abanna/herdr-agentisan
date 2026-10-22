package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// appendLine is a body func that adds line to the queued body, or starts one,
// and records every body it was given.
func appendLine(line string, given *[]string) func(string) (string, bool) {
	return func(queued string) (string, bool) {
		*given = append(*given, queued)
		if queued == "" {
			return "[agentisan] notices:\n" + line, true
		}
		return queued + "\n" + line, true
	}
}

func notify(t *testing.T, s *store.Store, to store.Address, at time.Time, body func(string) (string, bool)) (store.Message, bool) {
	t.Helper()
	m, appended, err := s.Notify(t.Context(), to, at, body)
	require.NoError(t, err)
	return m, appended
}

// count is how many messages state.db holds.
func count(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	return n
}

// TestNotifyQueuesThenAppends (A44): the first notice for a boss is a new
// message from the daemon; the next is appended to it while it is queued,
// keeping its id and its place in the inbox, so the boss gets one.
func TestNotifyQueuesThenAppends(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var given []string
	first, appended := notify(t, s, boss, t1, appendLine("a", &given))
	assert.False(t, appended)
	assert.Equal(t, []string{""}, given, "a new notice's body is body(\"\")")
	assert.Equal(t, boss, first.To)
	assert.Equal(t, store.Daemon, first.From)
	assert.Equal(t, store.MessageQueued, first.State)
	assert.Equal(t, "[agentisan] notices:\na", first.Body)
	assert.Empty(t, first.Project)
	assert.Zero(t, first.Generation)
	assert.Empty(t, first.NoticeOf)

	later := send(t, s, boss, pane, "a pane's message, queued after it")
	second, appended := notify(t, s, boss, t1.Add(time.Minute), appendLine("b", &given))
	assert.True(t, appended)
	assert.Equal(t, []string{"", "[agentisan] notices:\na"}, given)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, "[agentisan] notices:\na\nb", second.Body)
	assert.Equal(t, first.CreatedAt, second.CreatedAt)
	assert.Equal(t, t1.Add(time.Minute), second.UpdatedAt)
	assert.Equal(t, second, message(t, s, first.ID), "as stored")
	assert.Equal(t, 2, count(t, s))

	assert.Equal(t, first.ID, claim(t, s, boss).ID, "the notice keeps its place, before what was queued after it")
	require.NoError(t, s.Delivered(t.Context(), first.ID, t1))
	assert.Equal(t, later.ID, claim(t, s, boss).ID)
}

// TestNotifyWithTheClockBack: an append at a time before the notice was
// queued, a clock stepped back, keeps the notice's times in order.
func TestNotifyWithTheClockBack(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var given []string
	first, _ := notify(t, s, boss, t1, appendLine("a", &given))
	second, appended := notify(t, s, boss, t1.Add(-time.Hour), appendLine("b", &given))
	assert.True(t, appended)
	assert.Equal(t, first.CreatedAt, second.CreatedAt)
	assert.Equal(t, first.CreatedAt, second.UpdatedAt, "updated no earlier than created")
}

// TestNotifyAfterAClaim is the race with Claim: a notice the dispatcher
// took first is never appended to, so the next notice is a new message,
// and the one in flight is typed as it was claimed.
func TestNotifyAfterAClaim(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var given []string
	first, _ := notify(t, s, boss, t1, appendLine("a", &given))
	claimed := claim(t, s, boss)
	require.Equal(t, first.ID, claimed.ID)
	second, appended := notify(t, s, boss, t1, appendLine("b", &given))
	assert.False(t, appended)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, "[agentisan] notices:\nb", second.Body)
	assert.Equal(t, "[agentisan] notices:\na", message(t, s, first.ID).Body)
	assert.Equal(t, []string{"", ""}, given)

	require.NoError(t, s.Delivered(t.Context(), first.ID, t1))
	third, appended := notify(t, s, boss, t1, appendLine("c", &given))
	assert.True(t, appended, "the new one is queued, so it takes the next")
	assert.Equal(t, second.ID, third.ID)
}

// TestNotifyNeverAppendsToAnotherMessage: a message from anyone but the
// daemon, a notice of a failed delivery, a delivered notice and another
// boss's notice are each left as they are.
func TestNotifyNeverAppendsToAnotherMessage(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "a", "w14:p2", 0)
	ctx := t.Context()
	other := store.Address{Kind: store.KindBoss, ID: "sk-boss"}

	fromAWorker := send(t, s, boss, worker("a"), "from a worker")
	fromThePane := send(t, s, boss, pane, "from a pane")
	failed := send(t, s, worker("a"), boss, "to a")
	claim(t, s, worker("a"))
	require.NoError(t, s.Fail(ctx, failed.ID, "timeout", t1))
	var others []string
	othersNotice, _ := notify(t, s, other, t1, appendLine("for the other boss", &others))

	var given []string
	m, appended := notify(t, s, boss, t1, appendLine("a", &given))
	assert.False(t, appended)
	assert.Equal(t, []string{""}, given, "no queued message was offered")
	for _, kept := range []store.Message{fromAWorker, fromThePane, othersNotice} {
		assert.Equal(t, kept.Body, message(t, s, kept.ID).Body)
	}

	claimed := []string{}
	for {
		c, ok, err := s.Claim(ctx, boss, t1)
		require.NoError(t, err)
		if !ok {
			break
		}
		claimed = append(claimed, c.Body)
		require.NoError(t, s.Delivered(ctx, c.ID, t1))
	}
	require.Len(t, claimed, 4)
	assert.Equal(t, "from a worker", claimed[0])
	assert.Equal(t, "from a pane", claimed[1])
	assert.True(t, strings.HasPrefix(claimed[2], "[agentisan] message "+failed.ID), claimed[2])
	assert.Equal(t, "[agentisan] notices:\na", claimed[3])
	assert.Equal(t, store.MessageDelivered, message(t, s, m.ID).State)

	given = nil
	_, appended = notify(t, s, boss, t1, appendLine("b", &given))
	assert.False(t, appended, "a delivered notice is not appended to")
	assert.Equal(t, []string{""}, given)
}

// TestNotifyAppendsToTheNewest: when body refuses the queued notice, a new
// one is queued after it, and the next notice goes to that newest one.
func TestNotifyAppendsToTheNewest(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var given []string
	old, _ := notify(t, s, boss, t1, appendLine("a", &given))
	refuse := func(queued string) (string, bool) {
		given = append(given, queued)
		if queued != "" {
			return "", false
		}
		return "[agentisan] notices:\nb", true
	}
	newer, appended := notify(t, s, boss, t1, refuse)
	assert.False(t, appended)
	assert.NotEqual(t, old.ID, newer.ID)
	assert.Equal(t, []string{"", "[agentisan] notices:\na", ""}, given, "offered the queued one, then asked for a new body")

	last, appended := notify(t, s, boss, t1, appendLine("c", &given))
	assert.True(t, appended)
	assert.Equal(t, newer.ID, last.ID)
	assert.Equal(t, "[agentisan] notices:\nb\nc", last.Body)
	assert.Equal(t, "[agentisan] notices:\na", message(t, s, old.ID).Body)
}

// TestNotifyChecksTheBody: a body Send would refuse is refused here too,
// and changes nothing: the queued notice keeps its body, and no message is
// added.
func TestNotifyChecksTheBody(t *testing.T) {
	t.Parallel()
	tooLarge := strings.Repeat("x", store.MaxBody+1)
	tests := map[string]struct {
		body string
		want error
	}{
		"too large": {tooLarge, store.ErrTooLarge},
		"a NUL":     {"a\x00b", store.ErrInvalid},
		"not UTF-8": {"caf\xe9", store.ErrInvalid},
		"empty":     {"", store.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			bad := func(string) (string, bool) { return tc.body, true }
			_, _, err := s.Notify(t.Context(), boss, t1, bad)
			require.ErrorIs(t, err, tc.want, "a new notice")
			assert.Zero(t, count(t, s))

			var given []string
			queued, _ := notify(t, s, boss, t1, appendLine("a", &given))
			_, _, err = s.Notify(t.Context(), boss, t1, bad)
			require.ErrorIs(t, err, tc.want, "an appended one")
			assert.Equal(t, queued, message(t, s, queued.ID))
			assert.Equal(t, 1, count(t, s))
		})
	}
	s := open(t, dbPath(t))
	m, _ := notify(t, s, boss, t1, func(string) (string, bool) { return strings.Repeat("é", store.MaxBody/2), true })
	assert.Len(t, m.Body, store.MaxBody, "a body at the cap is taken")
}

// TestNotifyGoesToABoss: a notice goes to a boss by name, and only to one.
func TestNotifyGoesToABoss(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "a", "w14:p2", 0)
	body := func(string) (string, bool) { return "[agentisan] notices:\na", true }
	for name, to := range map[string]store.Address{
		"a worker":            worker("a"),
		"a pane":              pane,
		"the daemon":          store.Daemon,
		"a boss with no name": {Kind: store.KindBoss},
	} {
		_, _, err := s.Notify(t.Context(), to, t1, body)
		require.ErrorIs(t, err, store.ErrInvalid, name)
	}
	_, _, err := s.Notify(t.Context(), boss, time.Unix(0, 0).Add(-time.Hour), body)
	require.ErrorIs(t, err, store.ErrInvalid, "a time before 1970")
	assert.Zero(t, count(t, s))
}

// TestNotifyOnAClosedStore: a store that cannot be written is ErrStore, and
// a call already cancelled writes nothing.
func TestNotifyOnAClosedStore(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := s.Notify(ctx, boss, t1, func(string) (string, bool) { return "[agentisan] notices:\na", true })
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, count(t, s))
	require.NoError(t, s.Close())
	_, _, err = s.Notify(t.Context(), boss, t1, func(string) (string, bool) { return "[agentisan] notices:\na", true })
	require.ErrorIs(t, err, store.ErrStore)
}

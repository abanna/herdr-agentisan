package daemon_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// The tests of a flush's settlement (NERD-5533, ruling 4 of 2fd5a9f5): a
// flush settles exactly the notices the committed inbox body kept. The
// Batcher sizes a message without the inbox's header, which AppendTo adds,
// so in the 21 bytes below the cap, or 13 once the Batcher has cut, the
// header pushes the oldest line out of a message the sink accepts (A44).

// settleBoss is the boss these tests' project "app" sends to.
var settleBoss = store.Address{Kind: store.KindBoss, ID: "app-boss"}

// fillTo is notices for project, each with its own line and subject, whose
// lines, after have bytes of lines already, make the lines joined by line
// breaks exactly total bytes; acked records each one's acknowledgement by
// its line.
func fillTo(project string, have, total int, acked map[string]bool) []notice.Notice {
	var ns []notice.Notice
	for i := 0; have < total; i++ {
		l := min(500, total-have-1)
		if r := total - have - 1 - l; r > 0 && r < 8 {
			l -= 8
		}
		text := fmt.Sprintf("w%03d ", i) + strings.Repeat("x", l-5)
		ns = append(ns, notice.Notice{Project: project, Subject: text[:4], Kind: notice.Blocked, Text: text, Ack: func() { acked[text] = true }})
		have += 1 + l
	}
	return ns
}

// joined is the length of ns's lines joined by line breaks.
func joined(ns []notice.Notice) int {
	n := -1
	for _, x := range ns {
		n += 1 + len(x.Text)
	}
	return n
}

// queuedFor is the body of the message queued for to, claimed.
func queuedFor(t *testing.T, st *store.Store, to store.Address, now time.Time) string {
	t.Helper()
	m, ok, err := st.Claim(t.Context(), to, now)
	require.NoError(t, err)
	require.True(t, ok, "a message is queued for %s", to.ID)
	return m.Body
}

// TestAGitHubNoticeTheInboxHeaderCutsIsToldAgain (T1): an 8,191-byte batch
// whose oldest line is a genuine CI failure fits the Batcher, which sends it
// whole; the inbox's header pushes that line out. It was never sent, so it
// is not acknowledged (A44, A45): the running source tells it again once its
// hold has passed, and a fresh one, after a restart, tells it too; settled,
// it is seen once.
func TestAGitHubNoticeTheInboxHeaderCutsIsToldAgain(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	st := scratchStore(t)
	o := daemon.Options{Logger: zerolog.Nop(), Now: func() time.Time { return now }, Team: &daemon.TeamOptions{}}
	h := settings.Header{Project: "app", GitHub: "acme/app"}
	list := func(ci snapshot.CIState) github.Open {
		return github.Open{PRs: []github.OpenPR{{PR: snapshot.PR{Number: 5, CI: ci, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean}, Head: headA, Checks: 1, Failed: 1}}}
	}
	collect := func(src daemon.GitHubSource) []notice.Notice {
		src.Collected(t.Context(), github.Runner{}, h, list(snapshot.CIFailure))
		return src.Take()
	}
	src := daemon.NewGitHubSource(o, st)
	src.Collected(t.Context(), github.Runner{}, h, list(snapshot.CIPending))
	require.Empty(t, src.Take(), "the seed")
	ci := collect(src)
	require.Len(t, ci, 1)

	acked := map[string]bool{}
	fill := fillTo("app", len(ci[0].Text), notice.MaxBody-1, acked)
	batch := append(ci, fill...)
	require.Equal(t, notice.MaxBody-1, joined(batch), "8,191 bytes: the Batcher cuts nothing")
	b := notice.NewBatcher(notice.Configs{})
	b.Add(now, batch)
	daemon.FlushToInbox(o, st, map[string]string{"app": settleBoss.ID}, b)

	body := queuedFor(t, st, settleBoss, now)
	assert.True(t, strings.HasPrefix(body, "[agentisan] notices (1 earlier dropped):\n"), "the header pushed one line out")
	assert.NotContains(t, body, ci[0].Text, "the CI failure was the one")
	assert.Equal(t, len(fill), len(acked), "every line the body kept is settled")

	assert.Empty(t, collect(src), "held")
	hold := notice.DefaultGap + daemon.GitHubInterval
	now = now.Add(hold)
	again := collect(src)
	require.Len(t, again, 1, "never sent, never acknowledged: the running source tells it again")
	assert.Equal(t, ci[0].Text, again[0].Text)
	fresh := collect(daemon.NewGitHubSource(o, st))
	require.Len(t, fresh, 1, "nor recorded: a fresh source tells it again")

	again[0].Ack()
	now = now.Add(hold)
	assert.Empty(t, collect(src), "seen, once settled")
	assert.Empty(t, collect(daemon.NewGitHubSource(o, st)), "seen after a restart too")
}

// TestTheLineTheHeaderCutsAfterTheBatchCutIsNotSettled (T2): once the
// Batcher has cut, its dropped line is 13 bytes shorter than the inbox's
// header, so a batch in those 13 bytes below the cap loses one more line to
// the header. That line is not settled; the Batcher's own cut never was.
func TestTheLineTheHeaderCutsAfterTheBatchCutIsNotSettled(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	st := scratchStore(t)
	o := daemon.Options{Logger: zerolog.Nop(), Now: func() time.Time { return now }, Team: &daemon.TeamOptions{}}
	acked := map[string]bool{}
	ack := func(text string) func() { return func() { acked[text] = true } }
	first := "c0 " + strings.Repeat("y", notice.MaxLine-3)
	second := "c1 the line the header cuts"
	ns := []notice.Notice{
		{Project: "app", Subject: "c0", Kind: notice.Blocked, Text: first, Ack: ack(first)},
		{Project: "app", Subject: "c1", Kind: notice.Blocked, Text: second, Ack: ack(second)},
	}
	// The Batcher's body once it cut first: its dropped line, 27 bytes and
	// a break, then the rest, 5 bytes below the cap.
	rest := append(ns[1:], fillTo("app", len(second), notice.MaxBody-28-5, acked)...)
	require.Equal(t, notice.MaxBody-28-5, joined(rest))
	b := notice.NewBatcher(notice.Configs{})
	b.Add(now, append(ns[:1], rest...))
	daemon.FlushToInbox(o, st, map[string]string{"app": settleBoss.ID}, b)

	body := queuedFor(t, st, settleBoss, now)
	assert.True(t, strings.HasPrefix(body, "[agentisan] notices (2 earlier dropped):\n"), "the Batcher cut one, the header one more")
	assert.NotContains(t, body, second)
	assert.False(t, acked[first], "the Batcher's cut is not settled")
	assert.False(t, acked[second], "nor the header's")
	assert.Equal(t, len(rest)-1, len(acked), "every line the body kept is settled")
}

// TestAnAppendSettlesOnlyTheLinesItKept (T3): a flush appended to the
// boss's notice still queued keeps its lines after the queued ones; the cut
// takes the queued lines first, then the flush's oldest. Only the flush's
// lines the body kept are settled. The queued lines were settled by the
// flush that committed them and are lost to this cut, which the header
// counts: the known limit (the ack runs at commit, not when it is typed).
func TestAnAppendSettlesOnlyTheLinesItKept(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	st := scratchStore(t)
	o := daemon.Options{Logger: zerolog.Nop(), Now: func() time.Time { return now }, Team: &daemon.TeamOptions{}}
	bosses := map[string]string{"app": settleBoss.ID}
	b := notice.NewBatcher(notice.Configs{})

	earlier := map[string]bool{}
	queued := fillTo("app", -1, 2000, earlier)
	for i := range queued {
		queued[i].Text = "q" + queued[i].Text[1:]
		text := queued[i].Text
		queued[i].Ack = func() { earlier[text] = true }
	}
	b.Add(now, queued)
	daemon.FlushToInbox(o, st, bosses, b)
	require.Equal(t, len(queued), len(earlier), "the first flush is settled whole")

	acked := map[string]bool{}
	// 39 bytes and its break: what the header, already 40 bytes once the
	// queued lines are cut, needs past the 8,191 of the append.
	oldest := "n0 " + strings.Repeat("o", 36)
	ns := append([]notice.Notice{{Project: "app", Subject: "n0", Kind: notice.Blocked, Text: oldest, Ack: func() { acked[oldest] = true }}},
		fillTo("app", len(oldest), notice.MaxBody-1, acked)...)
	require.Equal(t, notice.MaxBody-1, joined(ns))
	b.Add(now, ns)
	daemon.FlushToInbox(o, st, bosses, b)

	body := queuedFor(t, st, settleBoss, now)
	assert.True(t, strings.HasPrefix(body, fmt.Sprintf("[agentisan] notices (%d earlier dropped):\n", len(queued)+1)), "every queued line, then the append's oldest")
	for _, q := range queued {
		assert.NotContains(t, body, q.Text)
	}
	assert.NotContains(t, body, oldest)
	assert.False(t, acked[oldest], "the append's own cut is not settled")
	assert.Equal(t, len(ns)-1, len(acked), "every line of the append the body kept is settled")
}

// TestTheInboxBodyAndLogAreUnchanged (T4): settling by what the body kept
// changes no byte the boss is typed or the log holds. The body is
// AppendTo's, which the dispatcher types as it stands
// (TestANoticeIsTypedInThePollThatQueuedIt), and the log line names every
// line of the message, the cut one included, with the same fields.
func TestTheInboxBodyAndLogAreUnchanged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	st := scratchStore(t)
	o := daemon.Options{Logger: logger, Now: func() time.Time { return now }, Team: &daemon.TeamOptions{}}
	acked := map[string]bool{}
	ns := fillTo("app", -1, notice.MaxBody-1, acked)
	b := notice.NewBatcher(notice.Configs{})
	b.Add(now, ns)
	daemon.FlushToInbox(o, st, map[string]string{"app": settleBoss.ID}, b)

	lines := make([]string, len(ns))
	for i, n := range ns {
		lines[i] = n.Text
	}
	assert.Equal(t, "[agentisan] notices (1 earlier dropped):\n"+strings.Join(lines[1:], "\n"), queuedFor(t, st, settleBoss, now))
	es := noticeLog(t, &logs)
	require.Len(t, es, 1)
	inbox, _ := es[0]["inbox"].(string)
	assert.Len(t, inbox, 26)
	delete(es[0], "time")
	want, err := json.Marshal(map[string]any{"level": "info", "project": "app", "notices": lines, "boss": settleBoss.ID, "inbox": inbox, "appended": false, "message": "notice"})
	require.NoError(t, err)
	got, err := json.Marshal(es[0])
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(got))
}

// TestANoticeForNoBossIsSettledWhole (T5): a message for a project with no
// boss is logged and nothing else, and settled whole, as before: told
// again it would still go nowhere, since the daemon reads its teams once.
// The sink keeps every line, so none is told again.
func TestANoticeForNoBossIsSettledWhole(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	st := scratchStore(t)
	o := daemon.Options{Logger: logger, Now: func() time.Time { return now }, Team: &daemon.TeamOptions{}}
	acked := map[string]bool{}
	ns := fillTo("nowhere", -1, notice.MaxBody-1, acked)
	b := notice.NewBatcher(notice.Configs{})
	b.Add(now, ns)
	daemon.FlushToInbox(o, st, map[string]string{"app": settleBoss.ID}, b)

	assert.Equal(t, len(ns), len(acked), "settled whole")
	queued, err := st.QueuedRecipients(t.Context())
	require.NoError(t, err)
	assert.Empty(t, queued, "nothing queued")
	es := noticeLog(t, &logs)
	require.Len(t, es, 1)
	assert.Equal(t, "", es[0]["boss"], "logged, with no boss")
	assert.Nil(t, es[0]["inbox"])
}

package daemon_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
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
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
)

// The tests of a flush's settlement (NERD-5533, ruling 4 of 2fd5a9f5): a
// flush settles exactly the notices the committed inbox body kept. The
// Batcher sizes a message without the inbox's header, which AppendTo adds,
// so in the 21 bytes below the cap, or 13 once the Batcher has cut, the
// header pushes the oldest line out of a message the sink accepts (A44).
// Each acknowledgement is counted, so one run twice is seen.

// settleBoss is the boss these tests' project "app" sends to.
var settleBoss = store.Address{Kind: store.KindBoss, ID: "app-boss"}

// fillTo is notices for project, each with its own line and subject, whose
// lines, after have bytes of lines already, make the lines joined by line
// breaks exactly total bytes; acked counts each one's acknowledgements by
// its line.
func fillTo(project string, have, total int, acked map[string]int) []notice.Notice {
	var ns []notice.Notice
	for i := 0; have < total; i++ {
		l := min(500, total-have-1)
		if r := total - have - 1 - l; r > 0 && r < 8 {
			l -= 8
		}
		text := fmt.Sprintf("w%03d ", i) + strings.Repeat("x", l-5)
		ns = append(ns, notice.Notice{Project: project, Subject: text[:4], Kind: notice.Blocked, Text: text, Ack: func() { acked[text]++ }})
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

// texts is ns's lines.
func texts(ns []notice.Notice) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Text
	}
	return out
}

// settledOnce asserts that acked counts each of ns acknowledged exactly
// once, and nothing else acknowledged at all.
func settledOnce(t *testing.T, acked map[string]int, ns []notice.Notice, msg string) {
	t.Helper()
	want := map[string]int{}
	for _, n := range ns {
		want[n.Text] = 1
	}
	got := map[string]int{}
	for k, v := range acked {
		if v != 0 {
			got[k] = v
		}
	}
	assert.Equal(t, want, got, msg)
}

// queuedFor is the body of the message queued for to, claimed.
func queuedFor(t *testing.T, st *store.Store, to store.Address, now time.Time) string {
	t.Helper()
	m, ok, err := st.Claim(t.Context(), to, now)
	require.NoError(t, err)
	require.True(t, ok, "a message is queued for %s", to.ID)
	return m.Body
}

// ciHeader is acme/app, the repo of project app; ciFailed is the event of
// #5's CI failing at headA, as the seen-set records it.
var (
	ciHeader = settings.Header{Project: "app", GitHub: "acme/app"}
	ciFailed = store.GitHubEvent{PR: 5, Kind: string(notice.CIFailed), Key: headA}
)

// ciBosses gives project app's notices to ha-boss, haTeam's boss.
var ciBosses = map[string]string{"app": "ha-boss"}

// collectCI is what src hands the poll after a list in which #5's CI is ci.
func collectCI(t *testing.T, src daemon.GitHubSource, ci snapshot.CIState) []notice.Notice {
	t.Helper()
	src.Collected(t.Context(), github.Runner{}, ciHeader, github.Open{PRs: []github.OpenPR{{
		PR: snapshot.PR{Number: 5, CI: ci, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean}, Head: headA, Checks: 1, Failed: 1,
	}}})
	return src.Take()
}

// seenOnDisk is every acme/app event state.db records as told.
func seenOnDisk(t *testing.T, st *store.Store) map[store.GitHubEvent]bool {
	t.Helper()
	g, err := st.GitHub(t.Context(), ciHeader.Project, ciHeader.GitHub)
	require.NoError(t, err)
	return g.Seen
}

// cutCI is the first half of T1a and T1b, through c on st and p's herdr at
// now: a genuine CI failure, first in an 8,191-byte batch, which fits the
// Batcher, goes into the inbox, whose header pushes the failure out, and the
// boss is typed the rest. Each line the body kept is settled once; the
// failure is neither typed nor recorded. It returns the failure's line, and
// the rest with their counts.
func cutCI(t *testing.T, c *daemon.NoticeCycle, st *store.Store, p *prompter, now time.Time) (line string, fill []notice.Notice, acked map[string]int) {
	t.Helper()
	require.Empty(t, collectCI(t, c.Source(), snapshot.CIPending), "the seed")
	ci := collectCI(t, c.Source(), snapshot.CIFailure)
	require.Len(t, ci, 1)
	acked = map[string]int{}
	fill = fillTo("app", len(ci[0].Text), notice.MaxBody-1, acked)
	batch := slices.Concat(ci, fill)
	require.Equal(t, notice.MaxBody-1, joined(batch), "8,191 bytes: the Batcher cuts nothing")
	c.Add(now, batch)
	c.Flush(t.Context())
	settledOnce(t, acked, fill, "every line the body kept is settled, once")
	assert.Empty(t, seenOnDisk(t, st), "the failure the header cut is not recorded as told")
	c.Serve(t.Context(), ciBosses["app"])
	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: "[agentisan] notices (1 earlier dropped):\n" + strings.Join(texts(fill), "\n")}}, p.all(),
		"the boss is typed the rest, and not the failure")
	return ci[0].Text, fill, acked
}

// TestACutCIFailureStillFailingIsToldAgainByTheRunningSource (T1a): the
// inbox's header cut a CI failure, so it was never typed and is not
// recorded (A44, A45). It remains eligible for rediscovery: while the
// failure is still observable, the running source tells it again once its
// hold has passed, and this retry goes through the inbox as any notice
// does. Committed whole, it is settled, so its key, and only its key, is
// recorded, and it is not told a third time.
func TestACutCIFailureStillFailingIsToldAgainByTheRunningSource(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	o := teamOpts(t, dir, th, t.TempDir())
	o.Now = func() time.Time { return now }
	st := scratchStore(t)
	c := daemon.NewNoticeCycle(o, st, ciBosses)
	line, fill, acked := cutCI(t, c, st, p, now)

	hold := notice.DefaultGap + daemon.GitHubInterval
	now = now.Add(hold - time.Nanosecond)
	assert.Empty(t, collectCI(t, c.Source(), snapshot.CIFailure), "held for gap and interval")
	now = now.Add(time.Nanosecond)
	again := collectCI(t, c.Source(), snapshot.CIFailure)
	require.Len(t, again, 1, "still failing, never typed, never recorded: the running source tells it again")
	assert.Equal(t, line, again[0].Text)
	c.Add(now, again)
	c.Flush(t.Context())
	assert.Equal(t, map[store.GitHubEvent]bool{ciFailed: true}, seenOnDisk(t, st), "the retry, committed whole, is settled: its key alone is recorded")
	c.Serve(t.Context(), ciBosses["app"])
	require.Len(t, p.all(), 2)
	assert.Equal(t, typed{Target: "ha-boss", Pane: "w14:p1", Text: "[agentisan] notices:\n" + line}, p.all()[1], "the boss is typed the failure")

	now = now.Add(hold)
	assert.Empty(t, collectCI(t, c.Source(), snapshot.CIFailure), "told and recorded: not again")
	c.Flush(t.Context())
	c.Serve(t.Context(), ciBosses["app"])
	assert.Len(t, p.all(), 2, "no third prompt")
	settledOnce(t, acked, fill, "the first flush's lines are settled once, by it alone")
}

// TestACutCIFailureStillFailingIsToldAgainAfterARestart (T1b): as T1a, but
// the daemon restarts after the cut: the database is closed and its file
// opened again, with a fresh source, Batcher and dispatcher. The cut failure
// is not on disk, so the restarted daemon's first collection, the failure
// still observable, tells it, through the inbox; once committed it is
// recorded, and the next restart does not tell it again.
func TestACutCIFailureStillFailingIsToldAgainAfterARestart(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	o := teamOpts(t, dir, th, t.TempDir())
	o.Now = func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "state.db")
	var st *store.Store
	restart := func() *daemon.NoticeCycle {
		if st != nil {
			require.NoError(t, st.Close())
		}
		var err error
		st, err = store.Open(t.Context(), path)
		require.NoError(t, err)
		return daemon.NewNoticeCycle(o, st, ciBosses)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := restart()
	line, fill, acked := cutCI(t, c, st, p, now)

	c = restart()
	assert.Empty(t, seenOnDisk(t, st), "nothing recorded, on disk either")
	again := collectCI(t, c.Source(), snapshot.CIFailure)
	require.Len(t, again, 1, "still failing, never recorded: the restarted daemon tells it again")
	assert.Equal(t, line, again[0].Text)
	c.Add(now, again)
	c.Flush(t.Context())
	assert.Equal(t, map[store.GitHubEvent]bool{ciFailed: true}, seenOnDisk(t, st), "the retry, committed whole, is settled: its key alone is recorded")
	c.Serve(t.Context(), ciBosses["app"])
	require.Len(t, p.all(), 2)
	assert.Equal(t, typed{Target: "ha-boss", Pane: "w14:p1", Text: "[agentisan] notices:\n" + line}, p.all()[1], "the boss is typed the failure")

	c = restart()
	now = now.Add(notice.DefaultGap + daemon.GitHubInterval)
	assert.Empty(t, collectCI(t, c.Source(), snapshot.CIFailure), "recorded: another restart does not tell it again")
	c.Flush(t.Context())
	c.Serve(t.Context(), ciBosses["app"])
	assert.Len(t, p.all(), 2, "no third prompt")
	settledOnce(t, acked, fill, "the first flush's lines are settled once, by it alone")
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
	acked := map[string]int{}
	ack := func(text string) func() { return func() { acked[text]++ } }
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
	assert.Zero(t, acked[first], "the Batcher's cut is not settled")
	assert.Zero(t, acked[second], "nor the header's")
	settledOnce(t, acked, rest[1:], "every line the body kept is settled, once")
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

	earlier := map[string]int{}
	queued := fillTo("app", -1, 2000, earlier)
	for i := range queued {
		queued[i].Text = "q" + queued[i].Text[1:]
		text := queued[i].Text
		queued[i].Ack = func() { earlier[text]++ }
	}
	b.Add(now, queued)
	daemon.FlushToInbox(o, st, bosses, b)
	settledOnce(t, earlier, queued, "the first flush is settled whole, once")

	acked := map[string]int{}
	// 39 bytes and its break: what the header, already 40 bytes once the
	// queued lines are cut, needs past the 8,191 of the append.
	oldest := "n0 " + strings.Repeat("o", 36)
	ns := append([]notice.Notice{{Project: "app", Subject: "n0", Kind: notice.Blocked, Text: oldest, Ack: func() { acked[oldest]++ }}},
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
	assert.Zero(t, acked[oldest], "the append's own cut is not settled")
	settledOnce(t, acked, ns[1:], "every line of the append the body kept is settled, once")
	settledOnce(t, earlier, queued, "the queued lines are not settled again")
}

// TestTheCutBodyIsLoggedAndTypedByteForByte (T4): settling by what the body
// kept changes no byte the log holds or the boss is typed. The log's clock
// is fixed, so each flush's notice line is compared whole, its final line
// break included, with the inbox message's own id, which the queued event
// gives: first a batch the Batcher cuts, which the line counts as dropped
// (the Batcher's count, not the header's, which cuts one more), then an
// append to it. The dispatcher types a body as offToken(cleanText(body))
// leaves it (messages.go): one space is added when its last token starts
// with @ or $. This body's last token is plain and its only control
// characters are line breaks, so it is typed byte for byte.
//
// It is not parallel: it sets zerolog's clock, a global the other tests'
// loggers read.
func TestTheCutBodyIsLoggedAndTypedByteForByte(t *testing.T) {
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	stamp := zerolog.TimestampFunc
	zerolog.TimestampFunc = func() time.Time { return at }
	t.Cleanup(func() { zerolog.TimestampFunc = stamp })
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	events, rec := telemetrytest.NewEvents()
	th, p, dir := haTeam(t)
	now := at
	o := teamOpts(t, dir, th, t.TempDir())
	o.Logger = logger
	o.Team.Events = events
	o.Now = func() time.Time { return now }
	st := scratchStore(t)
	c := daemon.NewNoticeCycle(o, st, ciBosses)

	acked := map[string]int{}
	note := func(subject, text string) notice.Notice {
		return notice.Notice{Project: "app", Subject: subject, Kind: notice.Blocked, Text: text, Ack: func() { acked[text]++ }}
	}
	first := note("c0", "c0 "+strings.Repeat("y", notice.MaxLine-3))
	rest := slices.Concat([]notice.Notice{note("c1", "c1 the line the header cuts")}, fillTo("app", 27, notice.MaxBody-28-5, acked))
	c.Add(now, slices.Concat([]notice.Notice{first}, rest))
	c.Flush(t.Context())
	now = now.Add(notice.DefaultGap)
	more := []notice.Notice{note("a1", "a1 is blocked"), note("a2", "a2 is blocked")}
	c.Add(now, more)
	c.Flush(t.Context())
	c.Serve(t.Context(), ciBosses["app"])

	var queued []telemetrytest.Event
	for _, e := range rec.Events() {
		if e.Name == telemetry.EventMessageQueued {
			queued = append(queued, e)
		}
	}
	require.Len(t, queued, 1, "one inbox message, appended to")
	id, _ := queued[0].Attrs["agentisan.message.id"].(string)
	require.Len(t, id, 26)
	m, err := st.Message(t.Context(), id)
	require.NoError(t, err)
	want := "[agentisan] notices (3 earlier dropped):\n" + strings.Join(slices.Concat(texts(rest[2:]), texts(more)), "\n")
	assert.Equal(t, want, m.Body, "the Batcher cut c0, the header c1, the append w000")
	assert.Equal(t, store.MessageDelivered, m.State)
	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: want}}, p.all(), "typed byte for byte")

	line := func(lines []string, dropped string, appended bool) string {
		ls, err := json.Marshal(lines)
		require.NoError(t, err)
		return fmt.Sprintf(`{"level":"info","project":"app","notices":%s,%s"boss":"ha-boss","inbox":"%s","appended":%t,"time":"2026-10-21T09:00:00Z","message":"notice"}`+"\n", ls, dropped, id, appended)
	}
	var got []string
	for l := range strings.Lines(logs.String()) {
		if strings.Contains(l, `"message":"notice"`) {
			got = append(got, l)
		}
	}
	assert.Equal(t, []string{line(texts(rest), `"dropped":1,`, false), line(texts(more), "", true)}, got, "each notice line, byte for byte")

	assert.Zero(t, acked[first.Text], "the Batcher's cut is not settled")
	assert.Zero(t, acked[rest[0].Text], "nor the header's")
	settledOnce(t, acked, slices.Concat(rest[1:], more), "every line a committed body kept is settled, once: w000 at the first flush, before the append cut it (the known limit)")
}

// TestANoticeForNoBossIsSettledWhole (T5): a message for a project no
// team's boss is configured for is only written to daemon.log (at info;
// above that level, nowhere), and settled whole, as before: told again it
// would still go nowhere, since the daemon reads its teams once. The sink
// keeps every line, so each is settled once and none is told again.
func TestANoticeForNoBossIsSettledWhole(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	st := scratchStore(t)
	o := daemon.Options{Logger: logger, Now: func() time.Time { return now }, Team: &daemon.TeamOptions{}}
	acked := map[string]int{}
	ns := fillTo("nowhere", -1, notice.MaxBody-1, acked)
	b := notice.NewBatcher(notice.Configs{})
	b.Add(now, ns)
	daemon.FlushToInbox(o, st, map[string]string{"app": settleBoss.ID}, b)

	settledOnce(t, acked, ns, "settled whole, once")
	queued, err := st.QueuedRecipients(t.Context())
	require.NoError(t, err)
	assert.Empty(t, queued, "nothing queued")
	es := noticeLog(t, &logs)
	require.Len(t, es, 1)
	assert.Equal(t, "", es[0]["boss"], "logged, with no boss")
	assert.Nil(t, es[0]["inbox"])
}

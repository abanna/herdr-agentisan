package daemon_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// notPostedMsg is the warn of a board-mode flush that goes to the inbox
// instead (A74).
const notPostedMsg = "notice not posted; it goes to the inbox"

// batch adds ns to b, each acknowledged into acks by its text when acks is
// not nil, and returns b.
func batch(b *notice.Batcher, acks map[string]int, ns ...notice.Notice) *notice.Batcher {
	for i := range ns {
		if acks != nil {
			text := ns[i].Text
			ns[i].Ack = func() { acks[text]++ }
		}
	}
	b.Add(time.Now(), ns)
	return b
}

// blocked is a notice for project that worker, on pane w14:p2, is blocked.
func blocked(project, worker string) notice.Notice {
	return notice.Notice{Project: project, Subject: "w14:p2", Worker: worker, Kind: notice.Blocked, Text: worker + " is blocked"}
}

// unblocked is the notice that ends blocked's.
func unblocked(project, worker string) notice.Notice {
	return notice.Notice{Project: project, Subject: "w14:p2", Worker: worker, Kind: notice.Unblocked, Text: worker + " is no longer blocked"}
}

// queuedNotices are the bodies of the daemon's messages queued for boss in
// the state.db at db, oldest first.
func queuedNotices(t *testing.T, db, boss string) []string {
	t.Helper()
	return query(t, db, "SELECT body FROM messages WHERE recipient_kind = 'boss' AND recipient = '"+boss+
		"' AND sender_kind = 'daemon' AND state = 'queued' ORDER BY seq")
}

// withBoss is m with project's boss place holding a pane named boss on
// pane, showing agent.
func withBoss(m *team.Model, project, short, boss, pane, agent string) *team.Model {
	m.Projects = append(m.Projects, team.Project{Name: project, Short: short, Boss: []team.Worker{
		{ID: pane, Pane: team.Pane{ID: pane, TerminalID: "term_" + pane, Name: boss, Agent: agent}},
	}})
	return m
}

func kinds(ps []store.BoardPost) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Kind)
	}
	return out
}

func postBodies(ps []store.BoardPost) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Body)
	}
	return out
}

// TestBoardModeClaudeBossGetsPosts is A74's notices end to end: a flush
// for a board-mode Claude boss is one daemon post per notice, with its
// kind, subject and worker, and goes nowhere else; every notice is
// acknowledged, and nothing is typed.
func TestBoardModeClaudeBossGetsPosts(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	acks := map[string]int{}
	ns := []notice.Notice{
		blocked("ha", "ha-coder-1"),
		{Project: "ha", Subject: "#50", Kind: notice.Merged, Text: "#50 merged as 1e0fb1b"},
		unblocked("ha", "ha-coder-1"),
	}
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), acks, ns...))
	r.poll(haModel(bossPane("ha-boss", "claude")))

	posts := r.posts(t, "ha")
	require.Len(t, posts, 3)
	for i, n := range ns {
		p := posts[i]
		assert.Equal(t, store.BoardPost{
			Seq: p.Seq, ID: p.ID, Project: "ha", Channel: store.ChannelDaemon, Author: store.Daemon,
			Kind: string(n.Kind), Subject: n.Subject, Worker: n.Worker, Body: n.Text, At: p.At,
		}, p, "post %d", i)
	}
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"), "no inbox row")
	assert.Equal(t, map[string]int{ns[0].Text: 1, ns[1].Text: 1, ns[2].Text: 1}, acks, "every notice acknowledged once")
	assert.Empty(t, r.pr.all(), "nothing typed")
}

// TestBoardRuleReadsThisPollsModel: the flush reads the memory this poll
// has already updated, so a boss first seen running Claude in this poll
// gets this poll's notices as posts.
func TestBoardRuleReadsThisPollsModel(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1")))
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))

	assert.Equal(t, []string{"blocked"}, kinds(r.posts(t, "ha")), "the notice is a post")
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"), "and no inbox row")
}

// TestHeldNoticesArePostedBeforeNewerOnes is M-C: a notice held in the
// inbox, and a worker's message queued after it, are posted before the
// newer notice of the poll that first sees the boss's Claude, each row with
// its .delivered event, so the board reads in the order things happened.
func TestHeldNoticesArePostedBeforeNewerOnes(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1"))
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	r.poll(haModel())
	held := query(t, r.db, "SELECT id FROM messages WHERE recipient = 'ha-boss' AND sender_kind = 'daemon'")
	require.Len(t, held, 1, "the notice is held in the inbox")
	m := sent(t, r.st, haBoss, coder, "done")
	batch(b, nil, unblocked("ha", "ha-coder-1"))

	r.poll(haModel(bossPane("ha-boss", "claude")))
	posts := r.posts(t, "ha")
	assert.Equal(t, []string{"ha-coder-1 is blocked", "done", "ha-coder-1 is no longer blocked"}, postBodies(posts), "in seq order")
	assert.Equal(t, []string{"notices", "post", "unblocked"}, kinds(posts))
	assert.Len(t, messageEvents(r.rec, telemetry.EventMessageDelivered, held[0]), 1, "the held notice's row")
	assert.Len(t, messageEvents(r.rec, telemetry.EventMessageDelivered, m.ID), 1, "the worker's message")
}

// TestQueuePostFailureKeepsTheFlushBehindIt is M-C's failure branch: when
// the boss's queue cannot be posted, the flush is not posted ahead of it;
// it is a new inbox row behind the held one, which is unchanged, and a
// later pass posts the held row, then the flush, as two notices posts.
func TestQueuePostFailureKeepsTheFlushBehindIt(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1"))
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	r.poll(haModel())
	drop := daemon.FailBoardInserts(t, r.db, "NEW.kind = 'notices'")
	batch(b, nil, unblocked("ha", "ha-coder-1"))

	r.poll(haModel(bossPane("ha-boss", "claude")))
	assert.Zero(t, boardCount(t, r.db), "no post at all")
	assert.Equal(t, []string{"[agentisan] notices:\nha-coder-1 is blocked", "[agentisan] notices:\nha-coder-1 is no longer blocked"},
		queuedNotices(t, r.db, "ha-boss"), "the flush is a new inbox row behind the held one, which is unchanged")
	assert.Equal(t, 1, warnings(r.logs.String(), notPostedMsg))

	drop()
	r.p.Pass()
	posts := r.posts(t, "ha")
	assert.Equal(t, []string{"notices", "notices"}, kinds(posts))
	assert.Equal(t, []string{"ha-coder-1 is blocked", "ha-coder-1 is no longer blocked"}, postBodies(posts), "in order")
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
	assert.Empty(t, r.pr.all(), "nothing typed")
}

// TestFlushDuringADrainQueuesItsNoticeLast (PR 2's part of the PostQueued
// cap, c9c5680f, with its erratum f59d7807): a flush for a board-mode boss
// whose queue post leaves rows queued is not posted ahead of them. Its
// notice is a new inbox row behind every held one, the older notice row
// unchanged, and once the dispatcher has drained the queue the board holds
// every held row in order, then the notice.
//   - "text" is c9c5680f's set-up: MaxQueuePost+1 rows, the older notice
//     row first. It shows only PR 2 as built red (the notice posted ahead
//     of row 101): the flush's own queue post delivers the older notice
//     row, so a Tail ignored still makes a new row.
//   - "erratum": MaxQueuePost worker rows, the older notice row, then one
//     more worker row. A Tail ignored is red too: A44's append puts the
//     flush's lines into row 101, ahead of worker row 102.
func TestFlushDuringADrainQueuesItsNoticeLast(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ before, after int }{
		"text":    {0, store.MaxQueuePost},
		"erratum": {store.MaxQueuePost, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfgs := boardOn("ha")
			b := notice.NewBatcher(cfgs)
			r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
			var held []string // every held row's id, in seq order
			send := func(n int) {
				for range n {
					held = append(held, sent(t, r.st, haBoss, coder, fmt.Sprintf("m%d", len(held))).ID)
				}
			}
			send(c.before)
			batch(b, nil, blocked("ha", "ha-coder-1"))
			r.p.EndPoll(haModel()) // no boss place: held, so the notice is an inbox row
			older := query(t, r.db, "SELECT id FROM messages WHERE sender_kind = 'daemon'")
			require.Len(t, older, 1, "the older notice row")
			held = append(held, older[0])
			send(c.after)

			batch(b, nil, unblocked("ha", "ha-coder-1"))
			r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
			assert.Zero(t, warnings(r.logs.String(), notPostedMsg), "a partial batch is expected")
			k := boardCount(t, r.db)
			require.Positive(t, k, "the flush made progress")
			assert.LessOrEqual(t, k, store.MaxQueuePost, "at most one batch")
			require.LessOrEqual(t, k, len(held))
			queued := query(t, r.db, "SELECT id FROM messages WHERE recipient = 'ha-boss' AND state = 'queued' ORDER BY seq")
			require.NotEmpty(t, queued)
			last := queued[len(queued)-1]
			assert.Equal(t, held[k:], queued[:len(queued)-1], "the held rows the batch left, then the notice's row")
			assert.Equal(t, []string{"[agentisan] notices:\nha-coder-1 is no longer blocked"},
				query(t, r.db, "SELECT body FROM messages WHERE id = '"+last+"'"), "a new row")
			assert.Equal(t, []string{"[agentisan] notices:\nha-coder-1 is blocked"},
				query(t, r.db, "SELECT body FROM messages WHERE id = '"+older[0]+"'"), "the older notice row unchanged")

			for range len(held) + 1 {
				if !r.p.TakeWake() {
					break
				}
				r.p.Pass()
			}
			assert.Equal(t, append(held, last), query(t, r.db, "SELECT id FROM board_posts WHERE project = 'ha' ORDER BY seq"),
				"every held row in order, then the notice")
			assert.Empty(t, r.pr.all(), "nothing typed")
			assert.Zero(t, warnings(r.logs.String(), notPostedMsg), "draining the queue succeeded")
		})
	}
}

// TestBoardModeCodexBossIsTyped: on a team whose key is board, a boss
// running Codex gets its notices in the inbox, the body InboxNotice writes,
// typed as before; nothing is posted.
func TestBoardModeCodexBossIsTyped(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1")))
	r.poll(haModel(bossPane("ha-boss", "codex")))

	db := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, daemon.InboxNotice(r.o, st, map[string]string{"ha": "ha-boss"}, notice.Message{Project: "ha", Lines: []string{"ha-coder-1 is blocked"}}))
	want := query(t, db, "SELECT body FROM messages")
	require.Len(t, want, 1)
	assert.Equal(t, want, query(t, r.db, "SELECT body FROM messages WHERE recipient = 'ha-boss'"), "InboxNotice's body")
	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: want[0]}}, r.pr.all(), "typed")
	assert.Zero(t, boardCount(t, r.db), "no post")
}

// TestSharedBossNameNoticesAreTyped: two tabs teams whose bosses share a
// name, both on the board and both showing Claude, get their notices in the
// inbox, which the dispatcher types: the name does not say whose board they
// are for.
func TestSharedBossNameNoticesAreTyped(t *testing.T) {
	t.Parallel()
	twin := settings.Team{Project: "hb", Short: "ha", Layout: settings.LayoutTabs}
	cfgs := boardOn("ha", "hb")
	r := newBoardRig(t, []settings.Team{haTabs, twin}, cfgs,
		batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1"), blocked("hb", "hb-coder-1")))
	r.p.EndPoll(withBoss(haModel(bossPane("ha-boss", "claude")), "hb", "ha", "ha-boss", "w20:p1", "claude"))

	assert.Equal(t, []string{"[agentisan] notices:\nha-coder-1 is blocked\nhb-coder-1 is blocked"}, queuedNotices(t, r.db, "ha-boss"), "the inbox")
	assert.Zero(t, boardCount(t, r.db), "no post on either board")
}

// TestBoardModeHeldBossUsesTheInbox: a board-mode boss whose agent has not
// been seen since the start has its notices held in the inbox, neither
// posted nor typed.
func TestBoardModeHeldBossUsesTheInbox(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1")))
	r.poll(haModel())

	assert.Equal(t, []string{"[agentisan] notices:\nha-coder-1 is blocked"}, queuedNotices(t, r.db, "ha-boss"))
	assert.Zero(t, boardCount(t, r.db), "no post")
	assert.Empty(t, r.pr.all(), "no prompt")
}

// TestBoardModeWithNoModelFollowsTheMemory: a poll that builds no model
// changes nothing remembered, so a Claude boss seen before still gets its
// notices as posts.
func TestBoardModeWithNoModelFollowsTheMemory(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	batch(b, nil, blocked("ha", "ha-coder-1"))

	r.p.EndPoll(nil)
	assert.Equal(t, []string{"blocked"}, kinds(r.posts(t, "ha")), "the notice is a post")
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
}

// TestNoKeyIsUnchanged: with no [notices] table at all, a Claude boss's
// notices are what they were before A74, byte for byte: the start notice
// is typed with its body, its log line has exactly the inbox's keys and no
// delivery, and nothing is posted.
func TestNoKeyIsUnchanged(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	var logs lockedLog
	o := teamOpts(t, dir, th, haNotices(t, ""))
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	o.Logger, o.Poll = logger, time.Hour
	serving(t, o, th.fakeHerdr)

	assert.Equal(t, typed{
		Target: "ha-boss", Pane: "w14:p1",
		Text: "[agentisan] notices:\nagentisan: the daemon started; turns that ended while it was down are not reported",
	}, p.next(t))
	lines := slices.DeleteFunc(noticeLog(t, &logs), func(e map[string]any) bool { return e["project"] != "ha" })
	require.Len(t, lines, 1)
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range lines[0] {
			if !yield(k) {
				return
			}
		}
	})
	assert.Equal(t, []string{"appended", "boss", "inbox", "level", "message", "notices", "project", "time"}, keys, "no delivery")
	assert.Zero(t, boardCount(t, paths(t, dir, th.fakeHerdr).DB), "no post")
}

// TestBoardModeIsPerTeam: each team's key is its own: of two tabs teams
// whose bosses both show Claude, the one on the board gets posts and the
// typed one its inbox.
func TestBoardModeIsPerTeam(t *testing.T) {
	t.Parallel()
	sk := settings.Team{Project: "sk", Short: "sk", Layout: settings.LayoutTabs}
	cfgs := boardOn("ha")
	r := newBoardRig(t, []settings.Team{haTabs, sk}, cfgs,
		batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1"), blocked("sk", "sk-coder-1")))
	r.p.EndPoll(withBoss(haModel(bossPane("ha-boss", "claude")), "sk", "sk", "sk-boss", "w20:p1", "claude"))

	assert.Equal(t, []string{"ha-coder-1 is blocked"}, postBodies(r.posts(t, "ha")), "posts for the board team")
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
	assert.Empty(t, r.posts(t, "sk"))
	assert.Equal(t, []string{"[agentisan] notices:\nsk-coder-1 is blocked"}, queuedNotices(t, r.db, "sk-boss"), "the inbox for the typed one")
}

// TestBoardPostFailureFallsBackToTheInbox is M4: a flush the board refuses
// goes to the inbox, with the notices its lines carry acknowledged, and is
// typed to no one; the pass after the board takes posts again posts it.
func TestBoardPostFailureFallsBackToTheInbox(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	acks := map[string]int{}
	b := batch(notice.NewBatcher(cfgs), acks, blocked("ha", "ha-coder-1"), unblocked("ha", "ha-coder-1"))
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	var earlier store.Message
	r.p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, b, sinkFunc(func(m notice.Message) error {
		assert.False(t, m.Tail, "the queue check succeeded before the direct insert failed")
		earlier = sent(t, r.st, haBoss, store.Daemon, "[agentisan] notices:\nearlier")
		return daemon.InboxNotice(r.o, r.st, map[string]string{"ha": "ha-boss"}, m)
	}))
	drop := daemon.FailBoardInserts(t, r.db, "")
	r.poll(haModel(bossPane("ha-boss", "claude")))

	assert.Equal(t, []string{"[agentisan] notices:\nearlier\nha-coder-1 is blocked\nha-coder-1 is no longer blocked"}, queuedNotices(t, r.db, "ha-boss"), "the lines in the inbox")
	assert.Equal(t, map[string]int{"ha-coder-1 is blocked": 1, "ha-coder-1 is no longer blocked": 1}, acks, "the kept notices acknowledged")
	assert.Equal(t, 1, warnings(r.logs.String(), notPostedMsg))
	lines := noticeLog(t, r.logs)
	require.Len(t, lines, 2)
	bodies := []any{"ha-coder-1 is blocked", "ha-coder-1 is no longer blocked"}
	assert.Equal(t, "info", lines[0]["level"])
	assert.Equal(t, "board", lines[0]["delivery"])
	assert.Equal(t, bodies, lines[0]["notices"])
	assert.NotContains(t, lines[0], "posts")
	assert.Equal(t, "info", lines[1]["level"])
	assert.Equal(t, earlier.ID, lines[1]["inbox"])
	assert.Equal(t, true, lines[1]["appended"])
	assert.Equal(t, bodies, lines[1]["notices"])
	for _, event := range r.rec.Events() {
		assert.NotEqual(t, telemetry.EventBoardPosted, event.Name)
	}
	assert.Zero(t, boardCount(t, r.db))
	assert.Empty(t, r.pr.all(), "nothing typed")

	drop()
	r.p.Pass()
	posts := r.posts(t, "ha")
	require.Len(t, posts, 1)
	assert.Equal(t, "notices", posts[0].Kind)
	assert.Equal(t, "earlier\nha-coder-1 is blocked\nha-coder-1 is no longer blocked", posts[0].Body)
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
}

// TestBoardModeEmptiesARefusedField: a subject the board would refuse is
// posted empty, and the notice still posted with the rest of its fields.
func TestBoardModeEmptiesARefusedField(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	n := blocked("ha", "ha-coder-1")
	n.Subject = strings.Repeat("s", 300)
	badWorker := unblocked("ha", "ha-coder-1")
	badWorker.Worker = strings.Repeat("w", 257)
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), nil, n, badWorker))
	r.poll(haModel(bossPane("ha-boss", "claude")))

	posts := r.posts(t, "ha")
	require.Len(t, posts, 2)
	assert.Empty(t, posts[1].Worker)
	assert.Equal(t, badWorker.Subject, posts[1].Subject)
	assert.Empty(t, posts[0].Subject)
	assert.Equal(t, "ha-coder-1", posts[0].Worker)
	assert.Equal(t, "ha-coder-1 is blocked", posts[0].Body)
}

// TestBoardModeAcksWhatTheInboxWouldCut: a flush too long for one inbox
// message is posted whole, one post per notice, and every notice is
// acknowledged, the ones the inbox would cut included.
func TestBoardModeAcksWhatTheInboxWouldCut(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	acks := map[string]int{}
	ns := make([]notice.Notice, 0, 20)
	for i := range 20 {
		text := string(rune('a'+i)) + strings.Repeat("x", notice.MaxLine-1)
		ns = append(ns, notice.Notice{Project: "ha", Subject: "w14:p2", Kind: notice.TurnEnded, Text: text})
	}
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), acks, ns...))
	r.poll(haModel(bossPane("ha-boss", "claude")))

	posts := r.posts(t, "ha")
	require.Len(t, posts, 20)
	for i, n := range ns {
		assert.Equal(t, n.Text, posts[i].Body, "post %d", i)
	}
	assert.Len(t, acks, 20)
	for text, n := range acks {
		assert.Equal(t, 1, n, "%.8s… acknowledged once", text)
	}
}

// TestBoardModeLogLineAndEvent: a board flush logs one notice line with
// delivery board and the posts' seqs in place of the inbox's keys, and is
// one agentisan.board.posted event; its notices give no message event.
func TestBoardModeLogLineAndEvent(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	ns := []notice.Notice{blocked("ha", "ha-coder-1"), unblocked("ha", "ha-coder-1"), blocked("ha", "ha-coder-2")}
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), nil, ns...))
	r.poll(haModel(bossPane("ha-boss", "claude")))
	posts := r.posts(t, "ha")
	require.Len(t, posts, 3)

	lines := noticeLog(t, r.logs)
	require.Len(t, lines, 1)
	delete(lines[0], "time")
	assert.Equal(t, map[string]any{
		"level": "info", "message": "notice", "project": "ha", "boss": "ha-boss", "delivery": "board",
		"notices": []any{ns[0].Text, ns[1].Text, ns[2].Text},
		"posts":   []any{float64(posts[0].Seq), float64(posts[1].Seq), float64(posts[2].Seq)},
	}, lines[0])
	var got []map[string]any
	for _, e := range r.rec.Events() {
		assert.False(t, strings.HasPrefix(e.Name, "agentisan.message."), "a board notice gives no %s", e.Name)
		if e.Name == telemetry.EventBoardPosted {
			got = append(got, e.Attrs)
		}
	}
	assert.Equal(t, []map[string]any{{
		"agentisan.board.project": "ha", "agentisan.board.channel": "daemon", "agentisan.board.posts": int64(3),
		"agentisan.board.first_seq": posts[0].Seq, "agentisan.board.last_seq": posts[2].Seq,
	}}, got)
}

// TestBoardModeCeilingPostsItsKindAndStillToasts is M5.3: a board-mode
// Claude boss at its context ceiling still alerts the user with herdr's
// request sound, and its ceiling line is a ctx_ceiling post, never typed.
// It replaces PR 1b's TestCeilingStillToastsInBoardMode, whose notices
// post this PR makes one post per notice.
func TestBoardModeCeilingPostsItsKindAndStillToasts(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	serving(t, teamOpts(t, dir, th, boardConfig(t)), th.fakeHerdr)
	polled(t, th.srv, 2)
	th.setToken("w14:p1", "ctx", "80")

	const line = "you are at 80% context (ceiling: 75%): hand off now; the user has been alerted"
	var ceiling store.BoardPost
	require.Eventually(t, func() bool {
		for _, post := range boardOf(t, st, "ha") {
			if strings.Contains(post.Body, line) {
				ceiling = post
				return true
			}
		}
		return false
	}, ready, 10*time.Millisecond, "the ceiling line is never posted")
	assert.Equal(t, "ctx_ceiling", ceiling.Kind)
	assert.Equal(t, store.ChannelDaemon, ceiling.Channel)
	assert.Equal(t, "w14:p1", ceiling.Subject)
	assert.False(t, strings.HasPrefix(ceiling.Body, "[agentisan]"), "the line alone")
	var sounds []string
	for _, n := range toasts(t, th) {
		if strings.Contains(n.Body, "the boss is past the 75% ceiling") {
			sounds = append(sounds, n.Sound)
		}
	}
	assert.Equal(t, []string{"request"}, sounds, "one toast, with herdr's request sound")
	assert.Empty(t, p.all(), "nothing typed into the boss")
}

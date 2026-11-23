package daemon_test

import (
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

// An earlier typed pass resumes after the poll queues the tail.
func TestFlushDuringADrainQueuesItsNoticeLast(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	var ids []string
	for range store.MaxQueuePost {
		ids = append(ids, sent(t, r.st, haBoss, coder, "older").ID)
	}
	first, last := blocked("ha", "one"), unblocked("ha", "one")
	batch(b, nil, first)
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	older := query(t, r.db, "SELECT id FROM messages WHERE sender_kind = 'daemon'")[0]
	ids = append(ids, older, sent(t, r.st, haBoss, coder, "after notice").ID)
	resume := r.p.TypedTurn(haBoss)
	require.NotNil(t, resume)

	batch(b, nil, last)
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	prefix := boardCount(t, r.db)
	require.Positive(t, prefix)
	require.LessOrEqual(t, prefix, store.MaxQueuePost)
	tail := query(t, r.db, "SELECT id FROM messages WHERE state = 'queued' ORDER BY seq")
	require.Len(t, tail, len(ids)-prefix+1)
	assert.Equal(t, ids[prefix:], tail[:len(tail)-1])
	assert.Equal(t, []string{"[agentisan] notices:\n" + first.Text, "[agentisan] notices:\n" + last.Text}, queuedNotices(t, r.db, "ha-boss"))

	resume()
	for range len(tail) {
		if !r.p.TakeWake() {
			break
		}
		r.p.Pass()
	}
	assert.Empty(t, r.pr.all(), "the old pass cannot type newly eligible rows")
	assert.Equal(t, append(ids, tail[len(tail)-1]), query(t, r.db, "SELECT id FROM board_posts ORDER BY seq"))
	assert.Zero(t, warnings(r.logs.String(), notPostedMsg))
}

func TestBoardModeCodexBossIsTyped(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, batch(notice.NewBatcher(cfgs), nil, blocked("ha", "ha-coder-1")))
	r.poll(haModel(bossPane("ha-boss", "codex")))

	want := "[agentisan] notices:\nha-coder-1 is blocked"
	assert.Equal(t, []string{want}, query(t, r.db, "SELECT body FROM messages WHERE recipient = 'ha-boss'"))
	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: want}}, r.pr.all(), "typed")
	assert.Zero(t, boardCount(t, r.db), "no post")
}

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

package daemon_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
)

func TestDirectNoticeDecisionAndInsertShareOneRouteHold(t *testing.T) {
	cfg := boardOn("ha")
	n := blocked("ha", "worker")
	acks := map[string]int{}
	b := batch(notice.NewBatcher(cfg), acks, n)
	r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
	type observation struct {
		held  bool
		holds uint64
	}
	observe := func() observation { return observation{r.p.RouteHeld(), r.p.RouteHolds()} }
	var reads, writes []observation
	r.p.SetDeliveryProbe(func() { reads = append(reads, observe()) })
	onSQL(t, r, "BEFORE INSERT ON board_posts", func() { writes = append(writes, observe()) })

	start := r.p.RouteHolds()
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	require.Len(t, reads, 1, "one delivery decision")
	require.Len(t, writes, 1, "one durable insert")
	assert.True(t, reads[0].held, "the decision is made under route")
	assert.True(t, writes[0].held, "the insert runs under route")
	assert.Equal(t, reads[0].holds, writes[0].holds, "no relock between decision and insert")
	assert.EqualValues(t, 2, r.p.RouteHolds()-start, "one remember and one delivery hold")
	p := r.posts(t, "ha")[0]
	assert.Equal(t, store.BoardPost{
		Seq: p.Seq, ID: p.ID, Project: n.Project, Channel: store.ChannelDaemon, Author: store.Daemon,
		Kind: string(n.Kind), Subject: n.Subject, Worker: n.Worker, Body: n.Text, At: p.At,
	}, p)
	settledOnce(t, acks, []notice.Notice{n}, "committed once")
	r.p.Pass()
	assert.Empty(t, r.pr.all())
}

func TestDirectNoticeBatchBoundThrough601InAnHour(t *testing.T) {
	cfg := boardOn("ha")
	acks := map[string]int{}
	ns := directNotices(601)
	b := batch(notice.NewBatcher(cfg), acks, ns...)
	r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
	var committed []int
	n := 0
	onSQL(t, r, "BEFORE INSERT ON board_posts", func() {
		if n%100 == 0 || n%100 == 99 {
			committed = append(committed, boardCount(t, r.db))
		}
		n++
	})
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))

	assert.Equal(t, []int{0, 0, 100, 100, 200, 200, 300, 300, 400, 400, 500, 500, 600}, committed)
	assert.Equal(t, texts(ns), query(t, r.db, "SELECT body FROM board_posts ORDER BY seq"))
	settledOnce(t, acks, ns, "all 601 eligible notices are settled")
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
}

func TestDirectNoticeRefusalCostsOnlyItself(t *testing.T) {
	cfg := boardOn("ha")
	acks := map[string]int{}
	ns := directNotices(3)
	ns[1].Text += strings.Repeat("a", store.MaxBody)
	b := batch(notice.NewBatcher(cfg), acks, ns...)
	r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
	daemon.FailBoardInserts(t, r.db, "NEW.body LIKE '(part 2 of 2) %'")
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))

	posts := r.posts(t, "ha")
	require.Len(t, posts, 3)
	assert.Equal(t, []string{"turn_ended", "undelivered", "turn_ended"}, kinds(posts))
	assert.Equal(t, ns[0].Text, posts[0].Body)
	assert.Equal(t, store.ChannelDaemon, posts[1].Channel)
	assert.Contains(t, posts[1].Body, "notice 001")
	assert.Equal(t, ns[2].Text, posts[2].Body)
	settledOnce(t, acks, ns, "the refusal settles with its replacement")
	lines := noticeLog(t, r.logs)
	require.Len(t, lines, 1)
	assert.EqualValues(t, 1, lines[0]["refused"])
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
}

func TestDirectOversizedNoticeUsesQueuedBodyParts(t *testing.T) {
	cfg := boardOn("ha")
	text := strings.Repeat("界", store.MaxBody/2)
	b := batch(notice.NewBatcher(cfg), nil, notice.Notice{Project: "ha", Kind: notice.TurnEnded, Text: text})
	r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
	queued := sent(t, r.st, haBoss, coder, "old row")
	rawExec(t, r.db, "UPDATE messages SET body = ? WHERE id = ?", text, queued.ID)
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	posts := r.posts(t, "ha")
	require.Len(t, posts, 4)
	assert.Equal(t, postBodies(posts[:2]), postBodies(posts[2:]), "direct and queued splitting match")
	assert.Equal(t, []string{"turn_ended", "turn_ended"}, kinds(posts[2:]))
}

func TestDirectNoticeFlushStopsAtFirstFailedChunk(t *testing.T) {
	cfg := boardOn("ha")
	acks := map[string]int{}
	ns := directNotices(201)
	b := batch(notice.NewBatcher(cfg), acks, ns...)
	r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
	rawExec(t, r.db, `CREATE TABLE parent_notice (id INTEGER PRIMARY KEY);
	CREATE TABLE child_notice (parent INTEGER REFERENCES parent_notice DEFERRABLE INITIALLY DEFERRED);
	CREATE TRIGGER fail_notice_commit AFTER INSERT ON board_posts WHEN NEW.body = 'notice 101'
	BEGIN INSERT INTO child_notice VALUES (1); END;`)
	attempted := 0
	onSQL(t, r, "BEFORE INSERT ON board_posts", func() { attempted++ })
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))

	assert.Equal(t, 200, attempted, "the third chunk never starts")

	want := ns[:100]
	assert.Equal(t, texts(want), query(t, r.db, "SELECT body FROM board_posts ORDER BY seq"))
	assert.Len(t, acks, len(want))
	settledOnce(t, acks, want, "only committed notices settle")
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
	lines := noticeLog(t, r.logs)
	require.Len(t, lines, 1)
	assert.EqualValues(t, 101, lines[0]["dropped"])
}

func directNotices(count int) []notice.Notice {
	ns := make([]notice.Notice, count)
	for i := range ns {
		ns[i] = notice.Notice{Project: "ha", Kind: notice.TurnEnded, Text: fmt.Sprintf("notice %03d", i)}
	}
	return ns
}

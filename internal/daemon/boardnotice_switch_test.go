package daemon_test

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
)

func TestRefusedBoardClaimKeepsItsPlace(t *testing.T) {
	t.Parallel()
	cfg := boardOn("ha")
	b := notice.NewBatcher(cfg)
	r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
	a := sent(t, r.st, haBoss, coder, "A")
	sent(t, r.st, haBoss, coder, "B")
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	arrived, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	signal := sync.OnceFunc(func() { close(arrived) })
	r.th.mu.Lock()
	r.th.prompt = func(herdrtest.Request) herdrtest.Reply {
		signal()
		<-resume
		return errReply("agent_not_ready", "refused before typing")
	}
	r.th.mu.Unlock()
	go func() { defer close(done); r.p.Pass() }()
	t.Cleanup(func() { release(); <-done })
	select {
	case <-arrived:
	case <-time.After(ready):
		t.Fatal("the boss prompt never arrived")
	}
	require.Equal(t, store.MessageDispatching, stateOf(t, r.st, a.ID).State)
	batch(b, nil, blocked("ha", "ha-coder-1"))
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	release()
	<-done
	last := unblocked("ha", "ha-coder-1")
	batch(b, nil, last)
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	posts := r.posts(t, "ha")
	assert.Equal(t, []string{"A", "B", "ha-coder-1 is blocked", last.Text}, postBodies(posts))
	assert.Equal(t, []string{"post", "post", "notices", string(last.Kind)}, kinds(posts))
	assert.Equal(t, []string{"ha-boss"}, promptTargets(t, r.th))
}

// While a boss prompt is outstanding, each flush for that boss gets its
// own inbox row. Once settlement has been attempted, the next flush can
// drain those rows, including when the attempted write failed.
func TestBoardFlushWaitsForItsBossClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, wantState string
	}{
		{"accepted", store.MessageDelivered},
		{"timeout", store.MessageFailed},
		{"write failed", store.MessageDispatching},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := boardOn("ha", "sk")
			b := notice.NewBatcher(cfg)
			sk := settings.Team{Project: "sk", Short: "sk", Layout: settings.LayoutTabs}
			r := newBoardRig(t, []settings.Team{haTabs, sk}, cfg, b)
			a := sent(t, r.st, haBoss, coder, "A")
			r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
			release := r.pr.hold(t, "ha-boss")
			if tc.name == "timeout" {
				r.pr.setAfter("ha-boss", errReply("timeout", "may have typed"))
			}
			done := make(chan struct{})
			go func() { defer close(done); r.p.Pass() }()
			t.Cleanup(func() { release(); <-done })
			require.Equal(t, "ha-boss", r.pr.next(t).Target)
			model := withBoss(haModel(bossPane("ha-boss", "claude")), "sk", "sk", "sk-boss", "w20:p1", "claude")
			first, second := blocked("ha", "one"), unblocked("ha", "two")
			other := blocked("sk", "elsewhere")
			batch(b, nil, first, other)
			r.p.EndPoll(model)
			batch(b, nil, second)
			r.p.EndPoll(model)
			assert.Empty(t, r.posts(t, "ha"))
			assert.Equal(t, []string{"[agentisan] notices:\n" + first.Text, "[agentisan] notices:\n" + second.Text}, queuedNotices(t, r.db, "ha-boss"))
			assert.Equal(t, []string{other.Text}, postBodies(r.posts(t, "sk")))
			if tc.name == "write failed" {
				rawExec(t, r.db, "CREATE TRIGGER refuse_settle BEFORE UPDATE OF state ON messages WHEN OLD.id = '"+a.ID+"' BEGIN SELECT RAISE(ABORT, 'settlement refused'); END")
			}
			release()
			<-done
			assert.Equal(t, tc.wantState, stateOf(t, r.st, a.ID).State)
			if tc.name == "timeout" {
				assert.Equal(t, []string{coder.ID}, query(t, r.db, "SELECT recipient FROM messages WHERE notice_of = '"+a.ID+"'"))
			}
			last := blocked("ha", "three")
			batch(b, nil, last)
			r.p.EndPoll(model)
			posts := r.posts(t, "ha")
			assert.Equal(t, []string{first.Text, second.Text, last.Text}, postBodies(posts))
			assert.Equal(t, []string{"notices", "notices", string(last.Kind)}, kinds(posts))
			assert.Len(t, r.pr.to("w14:p1"), 1)
		})
	}
}

func TestBoardFallbackSettlesOnlyCommittedEntries(t *testing.T) {
	t.Parallel()
	for _, tail := range []bool{false, true} {
		name := "direct insert refused"
		if tail {
			name = "queue post refused"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := boardOn("app")
			b := notice.NewBatcher(cfg)
			r := newBoardRig(t, []settings.Team{{Project: "app", Short: "ha", Layout: settings.LayoutTabs}}, cfg, b)
			model := haModel(bossPane("ha-boss", "claude"))
			model.Projects[0].Name = "app"
			now := time.Now()
			r.o.Now = func() time.Time { return now }
			r.o.Team.ConfigDir = t.TempDir()
			r.p = daemon.NewPoller(r.o, r.st, r.teams, cfg, b, nil)
			src := daemon.NewGitHubSource(r.o, r.st)
			require.Empty(t, collectCI(t, src, snapshot.CIPending))
			ci := collectCI(t, src, snapshot.CIFailure)
			require.Len(t, ci, 1)
			counts := map[string]int{}
			fill := fillTo("app", len(ci[0].Text), notice.MaxBody-1, counts)
			ns := slices.Concat(ci, fill)
			require.Equal(t, notice.MaxBody-1, joined(ns))
			if tail {
				sent(t, r.st, haBoss, coder, "older")
			}
			drop := daemon.FailBoardInserts(t, r.db, "")
			b.Add(now, ns)
			r.p.EndPoll(model)
			require.Equal(t, []string{"[agentisan] notices (1 earlier dropped):\n" + strings.Join(texts(fill), "\n")}, queuedNotices(t, r.db, "ha-boss"))
			assert.Empty(t, seenOnDisk(t, r.st))
			settledOnce(t, counts, fill, "only retained entries")
			drop()
			r.p.Pass()
			now = now.Add(notice.DefaultGap + daemon.GitHubInterval)
			again := collectCI(t, src, snapshot.CIFailure)
			require.Len(t, again, 1, "the same running collector retells the cut line")
			calls, ack := 0, again[0].Ack
			again[0].Ack = func() { calls++; ack() }
			b.Add(now, again)
			r.p.EndPoll(model)
			posts := r.posts(t, "app")
			require.NotEmpty(t, posts)
			assert.Equal(t, ci[0].Text, posts[len(posts)-1].Body)
			assert.Equal(t, 1, calls)
			assert.Equal(t, map[store.GitHubEvent]bool{ciFailed: true}, seenOnDisk(t, r.st))
		})
	}
}

// Inspect the real publication and release, on the dispatcher's goroutine.
// A second SQLite connection sees only committed states.
func TestBoardClaimMarkFollowsItsDurableRow(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	row := sent(t, r.st, haBoss, coder, "A")
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	var states []string
	r.p.ObserveBossClaim(func(to *store.Address) {
		states = append(states, query(t, r.db, "SELECT state FROM messages WHERE id = '"+row.ID+"'")...)
		if to != nil {
			assert.Equal(t, haBoss, *to)
			assert.True(t, r.p.RouteHeld(), "publish before releasing route")
			assert.False(t, r.p.MuHeld(), "the inbox mutex is free")
		} else {
			assert.False(t, r.p.RouteHeld(), "release after disposition outside route")
		}
	})
	r.p.Pass()
	assert.Equal(t, []string{store.MessageDispatching, store.MessageDelivered}, states)
}

// Resume the previously selected typed path after a poll has completed
// the switch. Its queue may now contain the fallback or the unposted suffix.
func TestBoardSwitchSkipsUnclaimedRows(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{false, true} {
		name := "direct insert refused"
		if partial {
			name = "queue partially posted"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := boardOn("ha")
			b := notice.NewBatcher(cfg)
			r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
			r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
			var ids []string
			count := 1
			if partial {
				count = store.MaxQueuePost + 1
			}
			for range count {
				ids = append(ids, sent(t, r.st, haBoss, coder, "older").ID)
			}
			resume := r.p.TypedTurn(haBoss)
			require.NotNil(t, resume, "the old pass selected the typed target")
			if !partial {
				daemon.FailBoardInserts(t, r.db, "NEW.kind = 'blocked'")
			}
			n := blocked("ha", "one")
			batch(b, nil, n)
			r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
			prefix := boardCount(t, r.db)
			require.Positive(t, prefix)
			require.LessOrEqual(t, prefix, store.MaxQueuePost)
			tails := query(t, r.db, "SELECT id FROM messages WHERE state = 'queued' ORDER BY seq")
			require.Len(t, tails, len(ids)-prefix+1)
			ids = append(ids, tails[len(tails)-1])
			resume()
			assert.Empty(t, r.pr.all(), "the old pass cannot type any newly eligible row")
			for range len(tails) {
				if !r.p.TakeWake() {
					break
				}
				r.p.Pass()
			}
			assert.Equal(t, ids, query(t, r.db, "SELECT id FROM board_posts ORDER BY seq"))
			assert.Equal(t, []string{n.Text}, query(t, r.db, "SELECT body FROM board_posts ORDER BY seq DESC LIMIT 1"))
		})
	}
}

func TestBoardRefusalAtBothSinksSettlesNothing(t *testing.T) {
	t.Parallel()
	cfg := boardOn("ha")
	b := notice.NewBatcher(cfg)
	r := newBoardRig(t, []settings.Team{haTabs}, cfg, b)
	daemon.FailBoardInserts(t, r.db, "")
	rawExec(t, r.db, "CREATE TRIGGER refuse_notice BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'inbox refused'); END")
	acks := map[string]int{}
	batch(b, acks, blocked("ha", "one"))
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	assert.Empty(t, acks)
	assert.Empty(t, queuedNotices(t, r.db, "ha-boss"))
	assert.Empty(t, r.posts(t, "ha"))
	assert.Equal(t, 1, warnings(r.logs.String(), "notice not delivered"))
}

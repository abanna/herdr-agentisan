package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// heldMsg is the warn a board-mode boss's held queue is logged under (A74).
const heldMsg = "the boss's agent has not been seen since the daemon started"

// postFailedMsg is the warn of a post the board refused, its rows kept.
const postFailedMsg = "posting them to the board failed"

var (
	haTabs  = settings.Team{Project: "ha", Short: "ha", Layout: settings.LayoutTabs}
	skBoss  = store.Address{Kind: store.KindBoss, ID: "sk-boss"}
	skCoder = store.Address{Kind: store.KindWorker, ID: "sk-coder-1"}
)

// haConfig is a config dir with workforceConfig's ha team and extra after
// it.
func haConfig(t *testing.T, extra string) string {
	t.Helper()
	repo := t.TempDir()
	marker := filepath.Join(repo, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
	return configDir(t, fmt.Sprintf("[projects.ha]\nrepo = %q\nshort = \"ha\"\nlayout = \"tabs\"\n[projects.ha.groups.coder]\nworkers = 1\nslots = 2\n", repo)+extra)
}

// boardConfig is workforceConfig with the ha team's notices.delivery board.
func boardConfig(t *testing.T) string {
	t.Helper()
	return haConfig(t, quietNotices+"[projects.ha.notices]\ndelivery = \"board\"\n")
}

// boardOn is every team's notices settings with the key board for the
// projects named, typed for every other.
func boardOn(projects ...string) notice.Configs {
	c := notice.Configs{Projects: map[string]notice.Config{}}
	for _, p := range projects {
		c.Projects[p] = notice.Config{Board: true}
	}
	return c
}

// bossPane is the ha boss's pane, w14:p1, named name and showing agent.
func bossPane(name, agent string) team.Pane {
	return team.Pane{ID: "w14:p1", TerminalID: "term_w14:p1", Name: name, Agent: agent}
}

// haModel is the ha team as a poll builds it: its coder ha-coder-1 bound to
// w14:p2, and its boss place holding the panes boss.
func haModel(boss ...team.Pane) *team.Model {
	p := team.Project{Name: "ha", Short: "ha", Groups: []team.Group{{Name: "coder", Workers: []team.Worker{
		{ID: "ha-coder-1", Bound: true, Pane: team.Pane{ID: "w14:p2", TerminalID: "term_w14:p2", Name: "ha-coder-1", Agent: "claude"}},
	}}}}
	for _, b := range boss {
		p.Boss = append(p.Boss, team.Worker{ID: b.ID, Pane: b})
	}
	return &team.Model{Projects: []team.Project{p}}
}

// boardRig is a Poller on a fresh state.db at db, prompting through th's
// herdr, whose agent.list names ha-boss on w14:p1 and ha-coder-1 on w14:p2,
// with its log in logs and its events in rec.
type boardRig struct {
	p     *daemon.Poller
	o     daemon.Options
	teams []settings.Team
	cfgs  notice.Configs
	st    *store.Store
	db    string
	th    *teamHerdr
	pr    *prompter
	logs  *lockedLog
	rec   *telemetrytest.Recorder
}

func newBoardRig(t *testing.T, teams []settings.Team, cfgs notice.Configs, b *notice.Batcher) *boardRig {
	t.Helper()
	th := newTeamHerdr(t)
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"))
	pr := newPrompter(th)
	db := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	logs := &lockedLog{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the daemon's log:\n%s", logs.String())
		}
	})
	events, rec := telemetrytest.NewEvents()
	o := daemon.Options{Logger: zerolog.New(logs), Now: time.Now, Herdr: herdr.Client{SocketPath: th.path}, Team: &daemon.TeamOptions{Events: events}}
	r := &boardRig{o: o, teams: teams, cfgs: cfgs, st: st, db: db, th: th, pr: pr, logs: logs, rec: rec}
	r.p = daemon.NewPoller(o, st, teams, cfgs, b, nil)
	r.bind(t, "ha-coder-1", "ha", "w14:p2")
	return r
}

// restart is a new daemon's Poller on the same state.db: a fresh inbox,
// dispatcher and memory.
func (r *boardRig) restart() { r.p = daemon.NewPoller(r.o, r.st, r.teams, r.cfgs, nil, nil) }

// bind gives worker id a live workers row in project, on pane.
func (r *boardRig) bind(t *testing.T, id, project, pane string) {
	t.Helper()
	_, err := r.st.BindWorker(t.Context(), store.Worker{LogicalID: id, Provider: "claude", Project: project, Group: "coder", PaneID: pane, TerminalID: "term_" + pane}, time.Now())
	require.NoError(t, err)
	require.NoError(t, r.st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: id}, store.StatusActive, time.Now()))
}

// poll ends a poll on m, then runs one pass.
func (r *boardRig) poll(m *team.Model) {
	r.p.EndPoll(m)
	r.p.Pass()
}

// posts is project's board, oldest first.
func (r *boardRig) posts(t *testing.T, project string) []store.BoardPost {
	t.Helper()
	return boardOf(t, r.st, project)
}

// boardOf is project's board in st, oldest first.
func boardOf(t *testing.T, st *store.Store, project string) []store.BoardPost {
	t.Helper()
	ps, err := st.LatestPosts(t.Context(), project, store.MaxBoardRead)
	require.NoError(t, err)
	for i, j := 0, len(ps)-1; i < j; i, j = i+1, j-1 {
		ps[i], ps[j] = ps[j], ps[i]
	}
	return ps
}

// boardCount counts every post on every board in the state.db at db.
func boardCount(t *testing.T, db string) int {
	t.Helper()
	n, err := strconv.Atoi(query(t, db, "SELECT count(*) FROM board_posts")[0])
	require.NoError(t, err)
	return n
}

// rawExec runs q on a connection of its own to the state.db at db.
func rawExec(t *testing.T, db, q string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", db+"?_pragma=busy_timeout(10000)")
	require.NoError(t, err)
	defer raw.Close() //nolint:errcheck // test cleanup
	_, err = raw.ExecContext(t.Context(), q, args...)
	require.NoError(t, err)
}

// promptTargets is the target of every agent.prompt th's herdr got, typed
// or refused.
func promptTargets(t *testing.T, th *teamHerdr) []string {
	t.Helper()
	var out []string
	for _, r := range th.srv.Requests() {
		if r.Method != "agent.prompt" {
			continue
		}
		var params struct {
			Target string `json:"target"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &params))
		out = append(out, params.Target)
	}
	return out
}

// messageEvents are rec's events named name for message id.
func messageEvents(rec *telemetrytest.Recorder, name, id string) []telemetrytest.Event {
	var out []telemetrytest.Event
	for _, e := range rec.Events() {
		if e.Name == name && e.Attrs["agentisan.message.id"] == id {
			out = append(out, e)
		}
	}
	return out
}

// postedLines are logs' "messages: posted to the board" lines, decoded.
func postedLines(t *testing.T, logs *lockedLog) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(logs.String()) {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &e), line)
		if e["message"] == "messages: posted to the board" {
			out = append(out, e)
		}
	}
	return out
}

// teamPost is the post a's message m becomes on ha's board.
func teamPost(m store.Message, got store.BoardPost) store.BoardPost {
	return store.BoardPost{Seq: got.Seq, ID: m.ID, Project: "ha", Channel: store.ChannelTeam, Author: m.From, Kind: "post", Body: m.Body, At: got.At}
}

// TestSendToABoardModeBossIsPosted is A74 end to end: a worker's send to
// its Claude boss, on a team whose key is board, answers queued as before,
// and the dispatcher posts it on the team's board as the worker's, with the
// message's id, marks the row delivered and emits .delivered once; nothing
// is typed into the boss.
func TestSendToABoardModeBossIsPosted(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	st := stateStore(t, dir, th)
	events, rec := telemetrytest.NewEvents()
	o := teamOpts(t, dir, th, boardConfig(t))
	o.Team.Events = events
	run(t, o)
	healthy(t, dir, th.fakeHerdr)

	data, err := daemon.Workforce(t.Context(), socketPath(t, dir, th.path), daemon.OpSend, workforce.SendRequest{To: "boss", Body: "done", CallerPane: "w14:p2"})
	require.NoError(t, err)
	var res workforce.SendResult
	require.NoError(t, json.Unmarshal(data, &res))
	assert.Equal(t, store.MessageQueued, res.State, "send answers as before")
	m := eventuallyState(t, st, res.ID, store.MessageDelivered)
	posts := boardOf(t, st, "ha")
	require.Len(t, posts, 1)
	assert.Equal(t, teamPost(m, posts[0]), posts[0])
	assert.Equal(t, coder, posts[0].Author)
	polled(t, th.srv, 2)
	assert.Zero(t, prompts(th.srv), "nothing typed into the boss")
	assert.Len(t, messageEvents(rec, telemetry.EventMessageDelivered, res.ID), 1)
}

// TestCodexBossOnAPollWithoutNamesIsNotPosted is B2: a poll whose
// agent.list failed names no pane, which changes nothing remembered, so a
// Codex boss seen before is never posted to; once a poll names it again,
// the message is typed.
func TestCodexBossOnAPollWithoutNamesIsNotPosted(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	r.poll(haModel(bossPane("ha-boss", "codex")))
	m := sent(t, r.st, haBoss, coder, "done")
	nameless := haModel(bossPane("", "codex"))
	nameless.Projects[0].Groups[0].Workers[0].Pane.Name = ""
	r.poll(nameless)
	assert.Empty(t, r.posts(t, "ha"), "poll 2: nothing posted")
	assert.Empty(t, r.pr.all(), "poll 2: nothing typed")
	assert.Equal(t, store.MessageQueued, stateOf(t, r.st, m.ID).State)

	r.poll(haModel(bossPane("ha-boss", "codex")))
	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + m.ID + " from ha-coder-1] done"}}, r.pr.all(), "poll 3: typed")
	assert.Empty(t, r.posts(t, "ha"))
}

// TestClaudeBossThatExitedIsPosted: a Claude boss seen once keeps its board
// when a poll shows no agent on a pane bearing its name, whether its pane
// closed, its agent exited and took the name with it, or the named pane
// shows none (A74).
func TestClaudeBossThatExitedIsPosted(t *testing.T) {
	t.Parallel()
	for name, boss := range map[string][]team.Pane{
		"no pane in the boss place":     nil,
		"a shell left, unnamed":         {bossPane("", "")},
		"the named pane shows no agent": {bossPane("ha-boss", "")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
			r.poll(haModel(bossPane("ha-boss", "claude")))
			m := sent(t, r.st, haBoss, coder, "done")
			r.poll(haModel(boss...))
			posts := r.posts(t, "ha")
			require.Len(t, posts, 1, "posted")
			assert.Equal(t, teamPost(m, posts[0]), posts[0])
			assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, m.ID).State)
			assert.Empty(t, r.pr.all())
		})
	}
}

// TestBossAgentSwitchMovesItsDelivery: the memory is the agent last seen,
// so a newer one replaces it: posted while the boss runs Claude, typed once
// it runs Codex, and posted again once it runs Claude (A74).
func TestBossAgentSwitchMovesItsDelivery(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	step := func(agent, body string) store.Message {
		m := sent(t, r.st, haBoss, coder, body)
		r.poll(haModel(bossPane("ha-boss", agent)))
		return m
	}
	first, second, third := step("claude", "one"), step("codex", "two"), step("claude", "three")
	var ids []string
	for _, p := range r.posts(t, "ha") {
		ids = append(ids, p.ID)
	}
	assert.Equal(t, []string{first.ID, third.ID}, ids, "posted while it runs Claude")
	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + second.ID + " from ha-coder-1] two"}}, r.pr.all(), "typed while it runs Codex")
}

// TestBossThatTurnsClaudeMidDrainGetsTheRestPosted (#183's hosted P2): a
// board team's Codex boss has three messages queued, so a pass types them.
// While the first is being typed, a poll sees Claude on the boss's pane. The
// first finishes typed; the two still queued are never typed into the
// Claude boss, and the next pass posts them, in order, each once.
func TestBossThatTurnsClaudeMidDrainGetsTheRestPosted(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	a, b, c := sent(t, r.st, haBoss, coder, "a"), sent(t, r.st, haBoss, coder, "b"), sent(t, r.st, haBoss, coder, "c")
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	release := r.pr.hold(t, "ha-boss")
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.p.Pass()
	}()
	assert.Equal(t, "[msg "+a.ID+" from ha-coder-1] a", r.pr.next(t).Text, "the drain types the oldest first")
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	release()
	select {
	case <-done:
	case <-time.After(ready):
		t.Fatal("the pass did not end")
	}

	assert.Equal(t, []typed{{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + a.ID + " from ha-coder-1] a"}}, r.pr.all(),
		"only the message already being typed is typed")
	assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, a.ID).State)
	for _, m := range []store.Message{b, c} {
		assert.Equal(t, store.MessageQueued, stateOf(t, r.st, m.ID).State, "left queued for the board")
	}
	r.p.Pass()
	var ids []string
	for _, p := range r.posts(t, "ha") {
		ids = append(ids, p.ID)
	}
	assert.Equal(t, []string{b.ID, c.ID}, ids, "the rest posted, in order, each once")
	assert.Len(t, r.pr.all(), 1, "nothing more typed")
}

// noticeRig is a board rig for the ha team, its key board, whose notices
// go out every poll, with a notice "before" already queued in the inbox for
// its boss while it runs Codex: one daemon row, the one A44 appends a later
// notice to while it is still queued.
func noticeRig(t *testing.T, o func(*daemon.Options)) (*boardRig, *notice.Batcher, store.Message) {
	t.Helper()
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	if o != nil {
		o(&r.o)
		r.p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, b, nil)
	}
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "before"}})
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	ids := query(t, r.db, "SELECT id FROM messages WHERE recipient = 'ha-boss' AND state = 'queued'")
	require.Len(t, ids, 1, "the notice is queued for the Codex boss")
	return r, b, stateOf(t, r.st, ids[0])
}

// TestBossThatTurnsClaudeAtTheClaimGetsNoLaterNotice is must-fix 1 of PR
// 2's pre-check (NERD-5533 cbf65a5e), on the checker's schedule: a pass has
// read the delivery as typed for a Codex boss and is about to claim when a
// poll remembers Claude and flushes a notice, which A44 appends to the
// boss's row while it is still queued. The poll waits for the claim: the
// pass reads and claims under the lock remember takes, so the row typed is
// the one read as typed, and the notice, flushed after the claim, goes into
// a row of its own and is posted, never typed.
//
// The order is forced, whatever the locks do: the poll starts only once
// the pass has ended.
// The poll-wins order is TestBossThatTurnsClaudeBeforeTheClaimGetsNoLaterNotice;
// the lock itself is pinned by TestBoardModeCriticalSectionsHoldRoute and
// TestRememberTakesRouteBeforeMu.
func TestBossThatTurnsClaudeAtTheClaimGetsNoLaterNotice(t *testing.T) {
	t.Parallel()
	var pause atomic.Bool
	atClaim, resume := make(chan struct{}), make(chan struct{})
	r, b, row := noticeRig(t, func(o *daemon.Options) {
		o.Now = func() time.Time {
			if pause.Swap(false) {
				close(atClaim)
				<-resume
			}
			return time.Now()
		}
	})
	pause.Store(true) // the pass's first clock read is its claim's
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		r.p.Pass()
	}()
	select {
	case <-atClaim:
	case <-time.After(ready):
		close(resume)
		t.Fatal("the pass never came to its claim")
	}
	require.Equal(t, store.MessageQueued, stateOf(t, r.st, row.ID).State, "paused before the claim")
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Gone, Text: "after the switch"}})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		<-passed // the order this test names: the pass first, then the poll
		r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	}()
	close(resume)
	for _, c := range []chan struct{}{passed, polled} {
		select {
		case <-c:
		case <-time.After(ready):
			t.Fatal("the pass or the poll did not end")
		}
	}

	require.Len(t, r.pr.all(), 1, "the row claimed is typed")
	assert.Equal(t, "[agentisan] notices:\nbefore", r.pr.all()[0].Text, "as it stood at the claim: no notice flushed after the switch")
	assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, row.ID).State)
	r.p.Pass()
	var bodies []string
	for _, p := range r.posts(t, "ha") {
		bodies = append(bodies, p.Body)
	}
	assert.Equal(t, []string{"after the switch"}, bodies, "the later notice, in a row of its own, is posted")
	assert.Len(t, r.pr.all(), 1, "nothing more typed")
}

// TestBossThatTurnsClaudeBeforeTheClaimGetsNoLaterNotice is the poll
// winning: while a pass types the Codex boss an older message, a poll
// remembers Claude and flushes a notice, which A44 appends to the boss's
// notice row still queued behind it. The pass claims nothing more, and the
// row, both notices in it, is posted, never typed.
func TestBossThatTurnsClaudeBeforeTheClaimGetsNoLaterNotice(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	a := sent(t, r.st, haBoss, coder, "a")
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "before"}})
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	release := r.pr.hold(t, "ha-boss")
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.p.Pass()
	}()
	assert.Equal(t, "[msg "+a.ID+" from ha-coder-1] a", r.pr.next(t).Text, "the oldest is typed first")
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Gone, Text: "after the switch"}})
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	release()
	select {
	case <-done:
	case <-time.After(ready):
		t.Fatal("the pass did not end")
	}

	require.Len(t, r.pr.all(), 1, "only the message already being typed is typed")
	r.p.Pass()
	var bodies []string
	for _, p := range r.posts(t, "ha") {
		bodies = append(bodies, p.Body)
	}
	assert.Equal(t, []string{"before\nafter the switch"}, bodies, "the notice row, the later notice appended, is posted")
	assert.Len(t, r.pr.all(), 1, "nothing more typed")
}

// TestBossThatTurnsClaudeAfterTheClaimTypesOnlyWhatWasClaimed is the
// other order: the pass has claimed the boss's row and is typing it when a
// poll remembers Claude and flushes a notice. The row typed is the one
// claimed, as it stood: A44 appends only to a row still queued, so the
// notice goes into a row of its own, which is posted.
func TestBossThatTurnsClaudeAfterTheClaimTypesOnlyWhatWasClaimed(t *testing.T) {
	t.Parallel()
	r, b, row := noticeRig(t, nil)
	release := r.pr.hold(t, "ha-boss")
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.p.Pass()
	}()
	first := r.pr.next(t)
	assert.Contains(t, first.Text, "before")
	require.Equal(t, store.MessageDispatching, stateOf(t, r.st, row.ID).State, "claimed and being typed")
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Gone, Text: "after the switch"}})
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	release()
	select {
	case <-done:
	case <-time.After(ready):
		t.Fatal("the pass did not end")
	}

	require.Len(t, r.pr.all(), 1, "only the row claimed before the switch is typed")
	assert.NotContains(t, r.pr.all()[0].Text, "after the switch", "typed as it was claimed")
	assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, row.ID).State)
	r.p.Pass()
	var bodies []string
	for _, p := range r.posts(t, "ha") {
		bodies = append(bodies, p.Body)
	}
	assert.Equal(t, []string{"after the switch"}, bodies, "the later notice, in a row of its own, is posted")
	assert.Len(t, r.pr.all(), 1, "nothing more typed")
}

// TestBossThatTurnsCodexAtThePostIsNotPosted is the reverse switch (the
// cold check of PR 2's design, NERD-5533 7c30e8aa, C2), on its probe's
// schedule: a pass has read the delivery as board for a Claude boss and is
// about to post when a poll remembers Codex and flushes a notice, which
// goes into the inbox to be typed. The poll waits for the post: the pass
// reads and posts under the lock remember takes, so only the row queued
// before the switch is posted, and the notice is typed into the Codex boss,
// never posted (A74: a Codex boss is never posted to).
//
// The order is forced, whatever the locks do: the poll starts only once
// the pass has ended.
// The poll-wins order is TestBossThatTurnsCodexBeforeThePostIsTyped; the
// lock itself is pinned by TestBoardModeCriticalSectionsHoldRoute and
// TestRememberTakesRouteBeforeMu.
func TestBossThatTurnsCodexAtThePostIsNotPosted(t *testing.T) {
	t.Parallel()
	var pause atomic.Bool
	atPost, resume := make(chan struct{}), make(chan struct{})
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	r.o.Now = func() time.Time {
		if pause.Swap(false) {
			close(atPost)
			<-resume
		}
		return time.Now()
	}
	r.p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, b, nil)
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	row := sent(t, r.st, haBoss, coder, "worker row")
	pause.Store(true) // the pass's first clock read is its post's
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		r.p.Pass()
	}()
	select {
	case <-atPost:
	case <-time.After(ready):
		close(resume)
		t.Fatal("the pass never came to its post")
	}
	require.Zero(t, boardCount(t, r.db), "paused before the post")
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "after the switch"}})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		<-passed // the order this test names: the pass first, then the poll
		r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	}()
	close(resume)
	for _, c := range []chan struct{}{passed, polled} {
		select {
		case <-c:
		case <-time.After(ready):
			t.Fatal("the pass or the poll did not end")
		}
	}

	var bodies []string
	for _, p := range r.posts(t, "ha") {
		bodies = append(bodies, p.Body)
	}
	assert.Equal(t, []string{"worker row"}, bodies, "only the row queued before the switch is posted")
	assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, row.ID).State)
	r.p.Pass()
	require.Len(t, r.pr.all(), 1, "the notice is typed into the Codex boss")
	assert.Equal(t, "[agentisan] notices:\nafter the switch", r.pr.all()[0].Text)
	assert.Len(t, r.posts(t, "ha"), 1, "nothing more posted")
}

// TestBossThatTurnsCodexBeforeThePostIsTyped is the reverse switch with
// the poll winning: a poll remembers Codex for a board-mode boss, with a
// notice to flush, before the pass decides. The pass reads typed, so the
// row queued while the boss ran Claude and the notice are both typed into
// Codex, and nothing is posted.
func TestBossThatTurnsCodexBeforeThePostIsTyped(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	row := sent(t, r.st, haBoss, coder, "worker row")
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "after the switch"}})
	r.p.EndPoll(haModel(bossPane("ha-boss", "codex")))
	r.p.Pass()

	assert.Empty(t, r.posts(t, "ha"), "nothing posted for a Codex boss")
	var texts []string
	for _, p := range r.pr.all() {
		texts = append(texts, p.Text)
	}
	assert.Equal(t, []string{"[msg " + row.ID + " from ha-coder-1] worker row", "[agentisan] notices:\nafter the switch"}, texts, "both typed, oldest first")
}

// TestBoardModeDecisionsReadTheClockUnderRoute is C4 of the cold check
// (NERD-5533 7c30e8aa): the schedules alone pass on code that reads the
// clock outside the lock, so this pins it. A board-key boss's typed claim
// and its board post each read the clock with route held: the delivery
// they acted on cannot have turned since.
func TestBoardModeDecisionsReadTheClockUnderRoute(t *testing.T) {
	t.Parallel()
	for name, agent := range map[string]string{"the typed claim": "codex", "the board post": "claude"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var (
				pause atomic.Bool
				held  atomic.Int32 // 0 not read, 1 held, 2 free
				p     *daemon.Poller
			)
			cfgs := boardOn("ha")
			r := newBoardRig(t, []settings.Team{haTabs}, cfgs, nil)
			r.o.Now = func() time.Time {
				if pause.Swap(false) {
					if p.RouteHeld() {
						held.Store(1)
					} else {
						held.Store(2)
					}
				}
				return time.Now()
			}
			p = daemon.NewPoller(r.o, r.st, r.teams, cfgs, nil, nil)
			p.EndPoll(haModel(bossPane("ha-boss", agent)))
			sent(t, r.st, haBoss, coder, "done")
			pause.Store(true) // the pass's first clock read is its claim's or its post's
			p.Pass()
			assert.Equal(t, int32(1), held.Load(), "the clock is read with route held")
			assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, query(t, r.db, "SELECT id FROM messages")[0]).State)
		})
	}
}

// TestTypedClaimsTakeNoRoute: only the boss of a tabs team whose key is
// board takes the route lock, for its board decision (untyped) and its
// claim (claimTyped). With the key unset, and for the ◆ spaces under the
// shared key board, a pass types its boss while another holds route, so
// the typed path waits on nothing it did not wait on before the lock
// (NERD-5533 cbf65a5e, 7c30e8aa).
func TestTypedClaimsTakeNoRoute(t *testing.T) {
	t.Parallel()
	spaces := settings.Team{Project: "spaces", Layout: settings.LayoutSpaces}
	spacesBoss := store.Address{Kind: store.KindBoss, ID: team.SpacesBoss}
	for name, c := range map[string]struct {
		teams []settings.Team
		cfgs  notice.Configs
		to    store.Address
		model *team.Model
	}{
		"the key unset": {[]settings.Team{haTabs}, notice.Configs{}, haBoss, haModel(bossPane("ha-boss", "claude"))},
		"the spaces, the shared key board": {
			[]settings.Team{spaces},
			notice.Configs{Default: notice.Config{Board: true}},
			spacesBoss,
			&team.Model{Projects: []team.Project{{Name: "spaces", Boss: []team.Worker{{ID: "wJ:p1", Pane: team.Pane{ID: "wJ:p1", TerminalID: "term_wJ:p1", Name: team.SpacesBoss, Agent: "claude"}}}}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newBoardRig(t, c.teams, c.cfgs, nil)
			r.th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("wJ:p1", team.SpacesBoss))
			m := sent(t, r.st, c.to, coder, "done")
			r.p.EndPoll(c.model)
			release := r.p.HoldRoute()
			t.Cleanup(release)
			done := make(chan struct{})
			go func() {
				defer close(done)
				r.p.Pass()
			}()
			select {
			case <-done:
			case <-time.After(ready):
				release()
				<-done
				t.Fatal("the typed pass waited on route")
			}
			assert.Equal(t, []string{"[msg " + m.ID + " from ha-coder-1] done"}, func() []string {
				var out []string
				for _, p := range r.pr.all() {
					out = append(out, p.Text)
				}
				return out
			}(), "typed while route is held")
		})
	}
}

// TestBossNotSeenSinceStartIsHeld: until a poll sees the boss's agent
// since the daemon started, its queue is held, neither typed nor posted, and
// warned of once; once Claude is seen, it is posted. A restart starts from
// no memory, so the boss is held again.
func TestBossNotSeenSinceStartIsHeld(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	m := sent(t, r.st, haBoss, coder, "done")
	r.poll(haModel())
	r.poll(haModel(bossPane("ha-boss", "")))
	assert.Empty(t, r.posts(t, "ha"), "held: not posted")
	assert.Empty(t, r.pr.all(), "held: not typed")
	assert.Equal(t, store.MessageQueued, stateOf(t, r.st, m.ID).State)
	assert.Equal(t, 1, warnings(r.logs.String(), heldMsg), "warned of once")

	r.poll(haModel(bossPane("ha-boss", "claude")))
	require.Len(t, r.posts(t, "ha"), 1, "posted once Claude is seen")
	assert.Equal(t, m.ID, r.posts(t, "ha")[0].ID)

	r.restart()
	again := sent(t, r.st, haBoss, coder, "after the restart")
	r.poll(haModel(bossPane("ha-boss", "")))
	assert.Len(t, r.posts(t, "ha"), 1, "a restart starts from no memory")
	assert.Equal(t, store.MessageQueued, stateOf(t, r.st, again.ID).State)
	assert.Empty(t, r.pr.all())
	assert.Equal(t, 2, warnings(r.logs.String(), heldMsg), "held again after the restart")
}

// TestDispatcherWithoutAModelPostsNothing is minor 4: with no model the
// dispatcher does nothing, the board path included, until a poll builds
// one again.
func TestDispatcherWithoutAModelPostsNothing(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	r.poll(haModel(bossPane("ha-boss", "claude")))
	r.p.EndPoll(nil)
	m := sent(t, r.st, haBoss, coder, "done")
	r.p.Pass()
	assert.Empty(t, r.posts(t, "ha"), "no model: nothing posted")
	assert.Empty(t, r.pr.all(), "no model: nothing typed")
	assert.Equal(t, store.MessageQueued, stateOf(t, r.st, m.ID).State)

	r.poll(haModel(bossPane("ha-boss", "claude")))
	require.Len(t, r.posts(t, "ha"), 1)
	assert.Equal(t, m.ID, r.posts(t, "ha")[0].ID)
}

// TestFailedDeliveryNoticeIsPosted: the daemon's notice that a boss's
// message failed, after a prompt that may have typed it, is posted on the
// boss's board in the next pass, as undelivered, and never typed into it.
func TestFailedDeliveryNoticeIsPosted(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	r.pr.setAfter("ha-coder-1", errReply("timeout", "timed out"))
	m := sent(t, r.st, coder, haBoss, "build it")
	r.poll(haModel(bossPane("ha-boss", "claude")))
	require.Equal(t, store.MessageFailed, stateOf(t, r.st, m.ID).State)

	r.p.Pass()
	posts := r.posts(t, "ha")
	require.Len(t, posts, 1)
	assert.Equal(t, store.ChannelDaemon, posts[0].Channel)
	assert.Equal(t, store.Daemon, posts[0].Author)
	assert.Equal(t, "undelivered", posts[0].Kind)
	assert.Equal(t, m.ID, posts[0].Subject)
	assert.Empty(t, r.pr.to("w14:p1"), "nothing typed into the boss")
}

// TestReturnedQueueIsPosted: an ended binding's queue, returned to its
// board-mode boss by the sweep, is posted in the same pass, as returned,
// with the worker it was for.
func TestReturnedQueueIsPosted(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	r.poll(haModel(bossPane("ha-boss", "claude")))
	m := sent(t, r.st, coder, aPane, "for the coder")
	require.NoError(t, r.st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: "ha-coder-1"}, store.StatusRetired, time.Now()))
	r.poll(haModel(bossPane("ha-boss", "claude")))
	posts := r.posts(t, "ha")
	require.Len(t, posts, 1, "posted in the pass that returned it")
	want := teamPost(m, posts[0])
	want.Kind, want.Subject = "returned", "ha-coder-1"
	assert.Equal(t, want, posts[0])
	got := stateOf(t, r.st, m.ID)
	assert.Equal(t, store.MessageDelivered, got.State)
	assert.Equal(t, haBoss, got.To)
	assert.Empty(t, r.pr.all())
}

// TestNoticesArePostedNotTyped: a poll's notices for a board-mode Claude
// boss go to the inbox, and the pass the poll wakes posts them as one
// notices post from the daemon, without the bare header; nothing is typed.
func TestNoticesArePostedNotTyped(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "ha-coder-1 is blocked"}})
	r := newBoardRig(t, []settings.Team{haTabs}, cfgs, b)
	r.poll(haModel(bossPane("ha-boss", "claude")))
	posts := r.posts(t, "ha")
	require.Len(t, posts, 1)
	assert.Equal(t, store.ChannelDaemon, posts[0].Channel)
	assert.Equal(t, "notices", posts[0].Kind)
	assert.Equal(t, "ha-coder-1 is blocked", posts[0].Body, "without the bare header")
	assert.Empty(t, r.pr.all())
}

// TestEndPollRemembersBeforeItFlushes is §5's order: a poll remembers its
// boss agents before it flushes its notices, and so before the wake that
// ends it, so the flush and the pass that wake starts read a memory already
// updated with this poll; the other way round, a boss that just switched to
// Claude would be typed one more time.
func TestEndPollRemembersBeforeItFlushes(t *testing.T) {
	t.Parallel()
	cfgs := boardOn("ha")
	b := notice.NewBatcher(cfgs)
	b.Add(time.Now(), []notice.Notice{{Project: "ha", Kind: notice.Blocked, Text: "ha-coder-1 is blocked"}})
	var p *daemon.Poller
	var atFlush []notice.Delivery
	p = daemon.NewPoller(daemon.Options{Logger: zerolog.Nop()}, scratchStore(t), []settings.Team{haTabs}, cfgs, b, sinkFunc(func(notice.Message) error {
		atFlush = append(atFlush, p.Delivery("ha"))
		return nil
	}))
	p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	assert.Equal(t, []notice.Delivery{notice.DeliverBoard}, atFlush, "the flush reads this poll's memory")
}

// TestCodexBossIsStillTyped: on a team whose key is board, a boss running
// Codex is typed, as A30 says, and nothing is posted.
func TestCodexBossIsStillTyped(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	th.setPane("w14:p1", "agent", "codex")
	st := stateStore(t, dir, th)
	m := sent(t, st, haBoss, coder, "done")
	serving(t, teamOpts(t, dir, th, boardConfig(t)), th.fakeHerdr)

	assert.Equal(t, typed{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + m.ID + " from ha-coder-1] done"}, p.next(t))
	eventuallyState(t, st, m.ID, store.MessageDelivered)
	polled(t, th.srv, 2)
	assert.Empty(t, boardOf(t, st, "ha"))
}

// TestSharedBossNameIsTyped: two tabs teams whose bosses share a name, both
// on the board and both showing Claude, are typed: the name does not say
// whose board a message is for.
func TestSharedBossNameIsTyped(t *testing.T) {
	t.Parallel()
	twin := settings.Team{Project: "hb", Short: "ha", Layout: settings.LayoutTabs}
	r := newBoardRig(t, []settings.Team{haTabs, twin}, boardOn("ha", "hb"), nil)
	r.th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("w20:p1", "ha-boss"))
	m := haModel(bossPane("ha-boss", "claude"))
	m.Projects = append(m.Projects, team.Project{Name: "hb", Short: "ha", Boss: []team.Worker{{ID: "w20:p1", Pane: team.Pane{ID: "w20:p1", Name: "ha-boss", Agent: "claude"}}}})
	sent(t, r.st, haBoss, coder, "done")
	r.poll(m)
	assert.Equal(t, []string{"ha-boss"}, promptTargets(t, r.th), "agent.prompt to the boss")
	assert.Zero(t, boardCount(t, r.db), "no post on either board")
}

// TestSendIsPerTeam: each team's key is its own: on two tabs teams whose
// bosses both show Claude, the one on the board gets a post and the typed
// one a prompt.
func TestSendIsPerTeam(t *testing.T) {
	t.Parallel()
	sk := settings.Team{Project: "sk", Short: "sk", Layout: settings.LayoutTabs}
	r := newBoardRig(t, []settings.Team{haTabs, sk}, boardOn("ha"), nil)
	r.th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), named("w20:p1", "sk-boss"))
	r.bind(t, "sk-coder-1", "sk", "w20:p2")
	m := haModel(bossPane("ha-boss", "claude"))
	m.Projects = append(m.Projects, team.Project{Name: "sk", Short: "sk", Boss: []team.Worker{{ID: "w20:p1", Pane: team.Pane{ID: "w20:p1", Name: "sk-boss", Agent: "claude"}}}})
	toHa := sent(t, r.st, haBoss, coder, "for ha")
	toSk := sent(t, r.st, skBoss, skCoder, "for sk")
	r.poll(m)
	posts := r.posts(t, "ha")
	require.Len(t, posts, 1)
	assert.Equal(t, teamPost(toHa, posts[0]), posts[0])
	assert.Equal(t, []typed{{Target: "sk-boss", Pane: "w20:p1", Text: "[msg " + toSk.ID + " from sk-coder-1] for sk"}}, r.pr.all())
	assert.Empty(t, r.posts(t, "sk"))
}

// TestNoKeyIsStillTyped: with notices.delivery unset, no [notices] table at
// all, or set to typed, a Claude boss is typed exactly as before A74, the
// same prompts as TestDispatcherDeliversByName, and nothing is posted. (A
// [notices] table without the key is every other dispatcher test's.)
func TestNoKeyIsStillTyped(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string]string{
		"no notices table": "",
		"the key typed":    quietNotices + "delivery = \"typed\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th, p, dir := haTeam(t)
			st := stateStore(t, dir, th)
			toCoder := sent(t, st, coder, haBoss, "build it")
			toBoss := sent(t, st, haBoss, coder, "done")
			serving(t, teamOpts(t, dir, th, haConfig(t, extra)), th.fakeHerdr)

			for _, m := range []store.Message{toCoder, toBoss} {
				assert.Equal(t, 1, eventuallyState(t, st, m.ID, store.MessageDelivered).Attempts)
			}
			polled(t, th.srv, 2)
			var got []typed
			for _, tp := range p.all() {
				if !strings.HasPrefix(tp.Text, "[agentisan] notices") {
					got = append(got, tp)
				}
			}
			assert.Equal(t, []typed{
				{Target: "ha-coder-1", Pane: "w14:p2", Text: "[msg " + toCoder.ID + " from ha-boss] build it"},
				{Target: "ha-boss", Pane: "w14:p1", Text: "[msg " + toBoss.ID + " from ha-coder-1] done"},
			}, got)
			assert.Zero(t, boardCount(t, paths(t, dir, th.fakeHerdr).DB))
		})
	}
}

// TestSpacesStayTyped: the ◆ spaces are always typed, even with the shared
// key board and their boss showing Claude (A74).
func TestSpacesStayTyped(t *testing.T) {
	t.Parallel()
	th := newSpacesHerdr(t)
	th.setAgents(named("wJ:p1", "boss"))
	th.setPane("wJ:p1", "agent", "claude")
	p := newPrompter(th)
	dir := shortDir(t)
	st := stateStore(t, dir, th)
	m := sent(t, st, store.Address{Kind: store.KindBoss, ID: "boss"}, aPane, "hello")
	serving(t, teamOpts(t, dir, th, configDir(t, quietNotices+"delivery = \"board\"\n")), th.fakeHerdr)

	assert.Equal(t, typed{Target: "boss", Pane: "wJ:p1", Text: "[msg " + m.ID + " from term_gone] hello"}, p.next(t))
	eventuallyState(t, st, m.ID, store.MessageDelivered)
	polled(t, th.srv, 2)
	assert.Zero(t, boardCount(t, paths(t, dir, th.fakeHerdr).DB))
}

// TestBoardPostIgnoresACancel: a post runs on its own budget, not on the
// pass's context, as claim does (b92cbbbb): a stop that cancels the pass
// does not keep a row from being posted (#183 pre-check 1c1ac52d, S1).
func TestBoardPostIgnoresACancel(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	m := sent(t, r.st, haBoss, coder, "after the stop")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.p.Post(ctx, haBoss, "ha")
	assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, m.ID).State)
	assert.Equal(t, 1, boardCount(t, r.db))
}

// TestPostFailureTypesNothing is M4 and M-E: a post the board refuses
// leaves the rows queued, never typed, and is warned of once; the next pass
// after the board takes posts again posts them.
func TestPostFailureTypesNothing(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	drop := daemon.FailBoardInserts(t, r.db, "")
	m := sent(t, r.st, haBoss, coder, "done")
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	r.p.TakeWake() // the poll's own
	r.p.Pass()
	assert.False(t, r.p.TakeWake(), "a failed post wakes no pass: the next one follows the next poll or send")
	assert.Equal(t, store.MessageQueued, stateOf(t, r.st, m.ID).State)
	assert.Empty(t, r.pr.all(), "never typed")
	assert.Empty(t, r.posts(t, "ha"))
	assert.Equal(t, 1, warnings(r.logs.String(), postFailedMsg))

	drop()
	r.p.Pass()
	require.Len(t, r.posts(t, "ha"), 1)
	assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, m.ID).State)
	assert.Empty(t, r.pr.all())
}

// TestRefusedRowIsFailedWithAnEvent: a row the board refuses, a pane sender
// no post can carry, is failed with an undelivered post in its place and
// one .failed event with its reason; nothing is typed.
func TestRefusedRowIsFailedWithAnEvent(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	now := time.Now().UnixNano()
	rawExec(t, r.db, `INSERT INTO messages (id, recipient_kind, recipient, project, sender_kind, sender, body, created_at, updated_at)
VALUES (?1, 'boss', 'ha-boss', '', 'pane', ?2, 'hello', ?3, ?3)`, id, strings.Repeat("p", 300), now)
	r.poll(haModel(bossPane("ha-boss", "claude")))
	posts := r.posts(t, "ha")
	require.Len(t, posts, 1)
	assert.Equal(t, "undelivered", posts[0].Kind)
	assert.Equal(t, id, posts[0].Subject)
	got := stateOf(t, r.st, id)
	assert.Equal(t, store.MessageFailed, got.State)
	failed := messageEvents(r.rec, telemetry.EventMessageFailed, id)
	require.Len(t, failed, 1)
	assert.Equal(t, got.Reason, failed[0].Attrs["agentisan.message.reason"])
	assert.NotEmpty(t, got.Reason)
	assert.Empty(t, r.pr.all())
}

// TestPostedLogLineAndWarnReset: a post that lands logs one line naming
// the recipient, the project, the messages and the posts, and clears the
// failure's warning, so the next failure warns again.
func TestPostedLogLineAndWarnReset(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	drop := daemon.FailBoardInserts(t, r.db, "")
	first := sent(t, r.st, haBoss, coder, "one")
	r.poll(haModel(bossPane("ha-boss", "claude")))
	assert.Equal(t, 1, warnings(r.logs.String(), postFailedMsg))

	drop()
	r.p.Pass()
	posts := r.posts(t, "ha")
	require.Len(t, posts, 1)
	lines := postedLines(t, r.logs)
	require.Len(t, lines, 1)
	assert.Equal(t, "ha-boss", lines[0]["recipient"])
	assert.Equal(t, "ha", lines[0]["project"])
	assert.Equal(t, []any{first.ID}, lines[0]["messages"])
	assert.Equal(t, []any{float64(posts[0].Seq)}, lines[0]["posts"])

	daemon.FailBoardInserts(t, r.db, "")
	sent(t, r.st, haBoss, coder, "two")
	r.p.Pass()
	assert.Equal(t, 2, warnings(r.logs.String(), postFailedMsg), "the next failure warns again")
}

// TestBoardQueueDrainsInBackToBackPasses (1b's part of the PostQueued
// cap, c9c5680f, and a8611aa0): a board-mode boss's backlog of
// 2×MaxQueuePost+1 rows is posted at most MaxQueuePost rows a pass, fewer
// when a pass's budget runs short, and each pass that leaves rows queued
// wakes the dispatcher again, so the next pass follows at once; the last
// does not. Every pass posts at least one row. Every row is posted once, in
// seq order, delivered, with one .delivered event, and each pass's log line
// says whether more remain.
func TestBoardQueueDrainsInBackToBackPasses(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	n := 2*store.MaxQueuePost + 1
	ms := make([]store.Message, n)
	for i := range ms {
		ms[i] = sent(t, r.st, haBoss, coder, fmt.Sprintf("m%d", i))
	}
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	r.p.TakeWake() // each poll leaves one
	posted, passes := 0, 0
	for posted < n {
		r.p.Pass()
		passes++
		now := boardCount(t, r.db)
		require.Greater(t, now, posted, "pass %d posts at least one row", passes)
		require.LessOrEqual(t, now-posted, store.MaxQueuePost, "pass %d posts at most a batch", passes)
		posted = now
		assert.Equal(t, posted < n, r.p.TakeWake(), "woken after pass %d, %d of %d posted", passes, posted, n)
	}
	ids := query(t, r.db, "SELECT id FROM board_posts WHERE project = 'ha' ORDER BY seq")
	require.Len(t, ids, n)
	for i, m := range ms {
		assert.Equal(t, m.ID, ids[i], "post %d, in the messages' seq order", i)
		assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, m.ID).State, m.ID)
		assert.Len(t, messageEvents(r.rec, telemetry.EventMessageDelivered, m.ID), 1, m.ID)
	}
	var more []any
	for _, l := range postedLines(t, r.logs) {
		more = append(more, l["more"])
	}
	want := make([]any, passes)
	for i := range want {
		want[i] = i < passes-1
	}
	assert.Equal(t, want, more, "each pass's log line")
	assert.Empty(t, r.pr.all(), "never typed")
}

// A batch whose rows together take longer than the post's budget still
// drains, in order: every pass posts the rows that fit in half the budget,
// and wakes the dispatcher while rows remain (NERD-5533 a8611aa0, A). Each
// post is made to take 1.5 times its share of the budget, so the 100 rows
// of a full batch take 7.5 s against settleTimeout's 5 s, as a slow host
// or -race under load makes them. Half the budget fits at most 33 of them,
// fewer under load, so the test asks only that each pass posts.
func TestBoardQueueDrainsWhenABatchOutlastsItsBudget(t *testing.T) {
	t.Parallel()
	r := newBoardRig(t, []settings.Team{haTabs}, boardOn("ha"), nil)
	ms := make([]store.Message, store.MaxQueuePost)
	for i := range ms {
		ms[i] = sent(t, r.st, haBoss, coder, fmt.Sprintf("m%d", i))
	}
	daemon.SlowBoardInserts(t, r.db, 3*store.BusyTimeout/(2*store.MaxQueuePost))
	r.p.EndPoll(haModel(bossPane("ha-boss", "claude")))
	r.p.TakeWake() // each poll leaves one
	posted, passes := 0, 0
	for posted < len(ms) {
		r.p.Pass()
		passes++
		now := boardCount(t, r.db)
		require.Greater(t, now, posted, "pass %d posts at least one row", passes)
		if passes == 1 {
			require.Less(t, now, len(ms), "the first pass stops at half its budget, leaving rows queued")
		}
		posted = now
		assert.Equal(t, posted < len(ms), r.p.TakeWake(), "woken after pass %d, %d of %d posted", passes, posted, len(ms))
	}
	ids := query(t, r.db, "SELECT id FROM board_posts WHERE project = 'ha' ORDER BY seq")
	require.Len(t, ids, len(ms), "posted after %d passes", passes)
	for i, m := range ms {
		assert.Equal(t, m.ID, ids[i], "post %d, in the messages' seq order", i)
		assert.Equal(t, store.MessageDelivered, stateOf(t, r.st, m.ID).State, m.ID)
	}
	assert.Empty(t, r.pr.all(), "never typed")
}

// TestCeilingStillToastsInBoardMode is M5.3: a board-mode Claude boss at
// its context ceiling still alerts the user with herdr's request sound, and
// its ceiling line is a notices post, never typed.
func TestCeilingStillToastsInBoardMode(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	st := stateStore(t, dir, th)
	serving(t, teamOpts(t, dir, th, boardConfig(t)), th.fakeHerdr)
	polled(t, th.srv, 2)
	th.setToken("w14:p1", "ctx", "80")

	const line = "you are at 80% context (ceiling: 75%): hand off now; the user has been alerted"
	require.Eventually(t, func() bool {
		for _, post := range boardOf(t, st, "ha") {
			if post.Kind == "notices" && post.Channel == store.ChannelDaemon && strings.Contains(post.Body, line) {
				return true
			}
		}
		return false
	}, ready, 10*time.Millisecond, "the ceiling line is never posted")
	var sounds []string
	for _, n := range toasts(t, th) {
		if strings.Contains(n.Body, "the boss is past the 75% ceiling") {
			sounds = append(sounds, n.Sound)
		}
	}
	assert.Equal(t, []string{"request"}, sounds, "one toast, with herdr's request sound")
	assert.Empty(t, p.all(), "nothing typed into the boss")
}

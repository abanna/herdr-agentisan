package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// failAgents makes agent.list fail, or answer again.
func (th *teamHerdr) failAgents(fail bool) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.agentsErr = fail
}

// turned is agent.list's entry for pane, named name, in its terminal of
// liveSpaces, with its last turn's end at seq (nil for none).
func turned(pane, name string, seq any) map[string]any {
	a := named(pane, name)
	a["terminal_id"] = "term_" + pane
	if seq != nil {
		a["completion_seq"] = seq
	}
	return a
}

// noticeMsg is one notice message as the log sink writes it.
type noticeMsg struct {
	Project string   `json:"project"`
	Notices []string `json:"notices"`
}

// noticesIn are the notice messages in logs, in order.
func noticesIn(logs *lockedLog) []noticeMsg {
	var out []noticeMsg
	for line := range strings.Lines(logs.String()) {
		var e struct {
			Msg string `json:"message"`
			noticeMsg
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Msg == "notice" {
			out = append(out, e.noticeMsg)
		}
	}
	return out
}

// noticeLines are every line of every notice message in logs.
func noticeLines(logs *lockedLog) []string {
	var out []string
	for _, m := range noticesIn(logs) {
		out = append(out, m.Notices...)
	}
	return out
}

// noticesConfig is a config dir whose shared file sets [notices] body.
func noticesConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[notices]\n"+body), 0o600))
	return dir
}

// noticeRun runs a daemon on th with config dir cfg, its log in logs,
// written as `daemon run` writes daemon.log: JSON at info, scrubbed.
func noticeRun(t *testing.T, th *teamHerdr, cfg string, logs *lockedLog) {
	t.Helper()
	o := teamOpts(t, shortDir(t), th, cfg)
	logger, err := logging.New(logs, "info", "json")
	require.NoError(t, err)
	o.Logger = logger
	serving(t, o, th.fakeHerdr)
}

// waitLine waits for a notice line holding want, and returns how many do.
func waitLine(t *testing.T, logs *lockedLog, want string) int {
	t.Helper()
	count := func() int {
		return len(slices.DeleteFunc(noticeLines(logs), func(l string) bool { return !strings.Contains(l, want) }))
	}
	if !assert.Eventually(t, func() bool { return count() > 0 }, ready, 5*time.Millisecond, "no notice %q in %v", want, noticeLines(logs)) {
		t.Fatalf("the daemon's log:\n%s", logs.String())
	}
	return count()
}

// TestNoticesReachTheLog is A44's part 1 end to end on herdrtest: the
// first poll tells each team the daemon started; a turn's end, a worker
// turning blocked and one leaving each reach daemon.log as a notice, once,
// under the message a boss follows.
func TestNoticesReachTheLog(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(turned("wJ:p1", "boss", nil), turned("wN:p1", "pee01", nil), turned("wY:p1", "", 3))
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "gap = 0\n"), &logs)

	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)
	require.Equal(t, []noticeMsg{{Project: "", Notices: []string{"agentisan: the daemon started; turns that ended while it was down are not reported"}}}, noticesIn(&logs),
		"the first poll is the baseline: no blocked pane or ready agent is told")

	th.setPane("wN:p1", "agent_status", "done")
	th.setAgents(turned("wJ:p1", "boss", 9), turned("wN:p1", "pee01", 7), turned("wY:p1", "", 3))
	waitLine(t, &logs, "pee01 finished a turn · ctx 40%")

	th.setPane("wN:p1", "agent_status", "blocked")
	waitLine(t, &logs, "pee01 is blocked · ctx 40%")

	panes := th.listedPanes()
	th.setPanes(slices.DeleteFunc(panes, func(p map[string]any) bool { return p["pane_id"] == "wY:p1" }))
	waitLine(t, &logs, "pane wY:p1 left the team")

	polled(t, th.srv, 3)
	assert.Equal(t, []string{
		"agentisan: the daemon started; turns that ended while it was down are not reported",
		"pee01 finished a turn · ctx 40%",
		"pee01 is blocked · ctx 40%",
		"pane wY:p1 left the team",
	}, noticeLines(&logs), "each once; the boss's own turn is not told")
}

// TestNoticesWaitOutAFailedAgentList: a turn that ends while agent.list
// fails is told once agent.list answers again, not before, and not twice.
func TestNoticesWaitOutAFailedAgentList(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(turned("wN:p1", "pee01", 1))
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "gap = 0\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)

	th.failAgents(true)
	th.setAgents(turned("wN:p1", "pee01", 2))
	require.Eventually(t, func() bool { return strings.Count(logs.String(), "team poll: list agents") >= 2 }, ready, 5*time.Millisecond)
	assert.Len(t, noticeLines(&logs), 1, "nothing is compared while agent.list fails")

	th.failAgents(false)
	waitLine(t, &logs, "finished a turn")
	polled(t, th.srv, 3)
	assert.Equal(t, 1, waitLine(t, &logs, "finished a turn"), "told once")
}

// TestNoticesFollowTheConfig: a team's skipped kinds are never told, and
// its gap holds the next message back.
func TestNoticesFollowTheConfig(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(turned("wN:p1", "pee01", 1))
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "skip = [\"started\", \"turn_ended\"]\ngap = 0\n"), &logs)
	polled(t, th.srv, 2)
	th.setAgents(turned("wN:p1", "pee01", 2))
	polled(t, th.srv, 2)
	th.setPane("wN:p1", "agent_status", "blocked")
	waitLine(t, &logs, "pee01 is blocked")
	assert.Equal(t, []string{"pee01 is blocked · ctx 40%"}, noticeLines(&logs))

	slow := newTeamHerdr(t)
	var slowLogs lockedLog
	noticeRun(t, slow, noticesConfig(t, "gap = 3600\n"), &slowLogs)
	waitLine(t, &slowLogs, "the daemon started")
	slow.setPane("wN:p1", "agent_status", "blocked")
	polled(t, slow.srv, 3)
	assert.Len(t, noticesIn(&slowLogs), 1, "inside the gap the blocked notice waits")
}

// TestNoticesWithABrokenConfig: a configuration that does not resolve is
// logged, and every boss gets the defaults.
func TestNoticesWithABrokenConfig(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "gap = -1\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	assert.Contains(t, logs.String(), "every boss gets the default notices")
}

// TestNoticesQueueNothing is A44's hard rule for part 1: notices reach the
// log, and state.db's messages table stays empty.
func TestNoticesQueueNothing(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(turned("wN:p1", "pee01", 1))
	var logs lockedLog
	dir := shortDir(t)
	o := teamOpts(t, dir, th, noticesConfig(t, "gap = 0\n"))
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	o.Logger = logger
	serving(t, o, th.fakeHerdr)
	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)
	th.setAgents(turned("wN:p1", "pee01", 2))
	waitLine(t, &logs, "pee01 finished a turn")
	th.setPane("wN:p1", "agent_status", "blocked")
	waitLine(t, &logs, "pee01 is blocked")
	polled(t, th.srv, 2)
	assert.Equal(t, []string{"0"}, query(t, paths(t, dir, th.fakeHerdr).DB, "SELECT count(*) FROM messages"))
}

// haNotices is a config dir with the tabs team ha and body appended.
func haNotices(t *testing.T, body string) string {
	t.Helper()
	repo := t.TempDir()
	marker := filepath.Join(repo, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
	return configDir(t, fmt.Sprintf("[projects.ha]\nrepo = %q\nshort = \"ha\"\nlayout = \"tabs\"\n[projects.ha.groups.coder]\nworkers = 1\nslots = 2\n", repo)+body)
}

// TestNoticeLineShape pins the line a boss greps daemon.log for (A44): its
// exact keys, level info, the team's project, and the lines.
func TestNoticeLineShape(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	var logs lockedLog
	noticeRun(t, th, haNotices(t, "[notices]\ngap = 0\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)
	th.setPane("w14:p2", "agent_status", "blocked")
	waitLine(t, &logs, "pane w14:p2 is blocked")
	var raw string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "is blocked") {
			raw = line
		}
	}
	var e map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &e), raw)
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range e {
			if !yield(k) {
				return
			}
		}
	})
	assert.Equal(t, []string{"level", "message", "notices", "project", "time"}, keys, raw)
	assert.Equal(t, "info", e["level"])
	assert.Equal(t, "notice", e["message"])
	assert.Equal(t, "ha", e["project"])
	assert.Equal(t, []any{"pane w14:p2 is blocked · ctx 31%"}, e["notices"])
}

// TestNoticeLineSaysWhatWasDropped: a message cut for size says how many
// notices it lost, under dropped; one that lost none has no such key.
func TestNoticeLineSaysWhatWasDropped(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	require.NoError(t, daemon.LogNotice(logger, notice.Message{Project: "ha", Lines: []string{"a"}, Dropped: 3}))
	require.NoError(t, daemon.LogNotice(logger, notice.Message{Project: "ha", Lines: []string{"b"}}))
	ls := slices.Collect(strings.Lines(logs.String()))
	require.Len(t, ls, 2)
	assert.Contains(t, ls[0], `"dropped":3`)
	assert.NotContains(t, ls[1], `"dropped"`)
}

// failingSink refuses every message.
type failingSink struct{}

func (failingSink) Send(context.Context, notice.Message) error { return errors.New("refused") }

// orderSink records each send, and whether the ack had run by then.
type orderSink struct {
	acked, sentBeforeAck *bool
}

func (s orderSink) Send(context.Context, notice.Message) error {
	*s.sentBeforeAck = !*s.acked
	return nil
}

// TestNoticesSettleOnlyWhatTheSinkAccepted is Stage 3's flush rule: a
// notice is acknowledged only after the sink accepted its message, and a
// refused message acknowledges nothing.
func TestNoticesSettleOnlyWhatTheSinkAccepted(t *testing.T) {
	t.Parallel()
	o := daemon.Options{Logger: zerolog.Nop(), Now: func() time.Time { return time.Unix(0, 0) }}
	acked := false
	n := notice.Notice{Project: "ha", Subject: "p1", Kind: notice.Blocked, Text: "p1 is blocked", Ack: func() { acked = true }}

	b := notice.NewBatcher(notice.Configs{})
	b.Add(time.Unix(0, 0), []notice.Notice{n})
	daemon.FlushNotices(o, b, failingSink{})
	assert.False(t, acked, "a refused message acknowledges nothing")

	sentBeforeAck := false
	b = notice.NewBatcher(notice.Configs{})
	b.Add(time.Unix(0, 0), []notice.Notice{n})
	daemon.FlushNotices(o, b, orderSink{acked: &acked, sentBeforeAck: &sentBeforeAck})
	assert.True(t, sentBeforeAck, "the sink had it before the ack ran")
	assert.True(t, acked)
}

// TestNoticesDropARepeat: the default repeat window (300 s) holds at the
// poll's pace: a worker blocked, unblocked and blocked again is told once.
func TestNoticesDropARepeat(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(turned("wN:p1", "pee01", 1))
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "gap = 0\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)
	th.setPane("wN:p1", "agent_status", "blocked")
	waitLine(t, &logs, "pee01 is blocked")
	th.setPane("wN:p1", "agent_status", "working")
	polled(t, th.srv, 2)
	th.setPane("wN:p1", "agent_status", "blocked")
	polled(t, th.srv, 3)
	assert.Equal(t, 1, waitLine(t, &logs, "pee01 is blocked"), "the repeat inside 300 s is dropped")
}

// TestNoticesWithABrokenConfigKeepTheDefaultGap: a file that does not
// resolve gives every boss the default 30 s gap, not none.
func TestNoticesWithABrokenConfigKeepTheDefaultGap(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "gap = -1\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	th.setPane("wN:p1", "agent_status", "blocked")
	polled(t, th.srv, 3)
	assert.Len(t, noticesIn(&logs), 1, "inside the default gap the blocked notice waits")
}

// TestNoticesFollowEachProjectsTable: a project's own [projects.<p>.notices]
// applies to its team only.
func TestNoticesFollowEachProjectsTable(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	var logs lockedLog
	noticeRun(t, th, haNotices(t, "[projects.ha.notices]\nskip = [\"blocked\"]\n[notices]\ngap = 0\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)
	th.setPane("w14:p2", "agent_status", "blocked")
	th.setPane("wN:p1", "agent_status", "blocked")
	waitLine(t, &logs, "is blocked · ctx 40%")
	polled(t, th.srv, 3)
	for _, m := range noticesIn(&logs) {
		if m.Project == "ha" {
			assert.NotContains(t, strings.Join(m.Notices, "\n"), "is blocked", "ha skips blocked")
		}
	}
}

// TestNoticesTellAnAgentExiting: an agent that exits while its pane stays
// open, a plain shell now that agent.list no longer names, is told as the
// worker leaving the team, once, with the ctx it last reported.
func TestNoticesTellAnAgentExiting(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(turned("wN:p1", "pee01", 1))
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "gap = 0\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)
	th.setPane("wN:p1", "agent", nil)
	th.setPane("wN:p1", "agent_status", "unknown")
	th.setAgents()
	assert.Equal(t, 1, waitLine(t, &logs, "pee01's agent exited (pane wN:p1) · ctx 40%"))
	polled(t, th.srv, 3)
	assert.Equal(t, 1, waitLine(t, &logs, "agent exited"), "told once")
}

// TestNoticesTellNoExitForAFailedAgentList: a live worker herdr knows only
// by name reads as a shell on a poll whose agent.list failed; it is never
// told as exited, and its pane's real close is told.
func TestNoticesTellNoExitForAFailedAgentList(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setPane("wN:p1", "agent", nil)
	th.setAgents(turned("wN:p1", "pee01", 1))
	var logs lockedLog
	noticeRun(t, th, noticesConfig(t, "gap = 0\n"), &logs)
	waitLine(t, &logs, "the daemon started")
	polled(t, th.srv, 2)
	th.failAgents(true)
	require.Eventually(t, func() bool { return strings.Count(logs.String(), "team poll: list agents") >= 2 }, ready, 5*time.Millisecond)
	th.failAgents(false)
	polled(t, th.srv, 2)
	th.setPanes(slices.DeleteFunc(th.listedPanes(), func(p map[string]any) bool { return p["pane_id"] == "wN:p1" }))
	waitLine(t, &logs, "pee01 left the team (pane wN:p1)")
	assert.NotContains(t, strings.Join(noticeLines(&logs), "\n"), "agent exited")
}

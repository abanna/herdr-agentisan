package daemon_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// setAgents replaces what herdr answers agent.list with.
func (th *teamHerdr) setAgents(agents ...map[string]any) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.agents = agents
}

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

// noticeRun runs a daemon on th with config dir cfg, its log in logs.
func noticeRun(t *testing.T, th *teamHerdr, cfg string, logs *lockedLog) {
	t.Helper()
	o := teamOpts(t, shortDir(t), th, cfg)
	o.Logger = zerolog.New(logs)
	run(t, o)
}

// waitLine waits for a notice line holding want, and returns how many do.
func waitLine(t *testing.T, logs *lockedLog, want string) int {
	t.Helper()
	count := func() int {
		return len(slices.DeleteFunc(noticeLines(logs), func(l string) bool { return !strings.Contains(l, want) }))
	}
	require.Eventually(t, func() bool { return count() > 0 }, ready, 5*time.Millisecond, "no notice %q in %v", want, noticeLines(logs))
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
	waitLine(t, &logs, "pee01 finished a turn (done) · ctx 40%")

	th.setPane("wN:p1", "agent_status", "blocked")
	waitLine(t, &logs, "pee01 is blocked · ctx 40%")

	panes := th.listedPanes()
	th.setPanes(slices.DeleteFunc(panes, func(p map[string]any) bool { return p["pane_id"] == "wY:p1" }))
	waitLine(t, &logs, "pane wY:p1 left the team")

	polled(t, th.srv, 3)
	assert.Equal(t, []string{
		"agentisan: the daemon started; turns that ended while it was down are not reported",
		"pee01 finished a turn (done) · ctx 40%",
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

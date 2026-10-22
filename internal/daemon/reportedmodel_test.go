package daemon_test

import (
	"maps"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// TestSnapshotPrefersTheReportedModel is A56 on a herdrtest team: a model
// the pane's agent reported since it started wins over its launch model, the
// boss's too, and fills a row started with no --model; one stamped before
// the agent started is a previous agent's and gives way to the launch
// model; a shell's leftover token is no model.
func TestSnapshotPrefersTheReportedModel(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, panes, agents := fullTeam()
	// Each process starts at its pid in ticks since boot: 1002 at boot+10.02 s.
	after, before := boot.Add(time.Minute), boot.Add(time.Second)
	stamp := func(id, model string, at time.Time) {
		for _, p := range panes {
			if p["pane_id"] != id {
				continue
			}
			tokens := map[string]string{report.ModelKey: model, report.ModelAtKey: report.FormatModelAt(at)}
			if old, ok := p["tokens"].(map[string]string); ok {
				maps.Copy(tokens, old)
			}
			p["tokens"] = tokens
		}
	}
	stamp("wJ:p1", "claude-opus-5-5[1m]", after) // the boss's /model
	stamp("wN:p1", "claude-sonnet-5-5", after)   // a /model switch since launch
	stamp("wN:p2", "claude-haiku-5-5", after)    // launched with no --model
	stamp("wN:p3", "gpt-5", before)              // left by an agent before this one
	stamp("wN:p5", "gpt-5.5", after)             // a shell: no agent in the pane
	th.layout(ws, panes, agents)
	th.mu.Lock()
	th.procs = map[string][]map[string]any{
		"wJ:p1": {agentProc(1001, "claude", "--model", "opus", "--name", "boss")},
		"wN:p1": {agentProc(1002, "claude", "--model", "claude-opus-5-5")},
		"wN:p2": {agentProc(1003, "claude")},
		"wN:p3": {agentProc(1005, "claude", "--model=sonnet")},
		"wN:p4": {agentProc(1006, "codex", "-m", "o3")},
	}
	th.mu.Unlock()
	pt := &procs{start: map[int]uint64{}}
	for _, pid := range []int{1001, 1002, 1003, 1005, 1006} {
		pt.set(pid, uint64(pid))
	}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, projectConfig(t))
	pt.set(os.Getpid(), 77)
	o.Hooks.StartTime = pt.startTime
	o.Now = func() time.Time { return t0 }
	serving(t, o, th.fakeHerdr)

	want := withBossStart(withModels(wantFullTeam(), map[string]string{
		"wJ:p1": "claude-opus-5-5[1m]",
		"wN:p1": "claude-sonnet-5-5",
		"wN:p2": "claude-haiku-5-5",
		"wN:p3": "sonnet",
		"wN:p4": "o3",
	}), map[string]time.Time{"wJ:p1": boot.Add(10010 * time.Millisecond)})
	got := served(t, socketPath(t, dir, th.path), func(s snapshot.Snapshot) bool { return assert.ObjectsAreEqual(want, s) })
	assert.Equal(t, want, got)
}

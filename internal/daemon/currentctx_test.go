package daemon_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// lastBands is the ctx display the last push to pane carried: its three
// band keys.
func lastBands(t *testing.T, th *teamHerdr, pane string) map[string]string {
	t.Helper()
	pushes := panePushes(t, th.srv)[pane]
	require.NotEmpty(t, pushes, "the poll pushed %s its ctx display", pane)
	var params struct {
		Tokens map[string]string `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(pushes[len(pushes)-1]), &params))
	return map[string]string{"ctx_ok": params.Tokens["ctx_ok"], "ctx_warn": params.Tokens["ctx_warn"], "ctx_hot": params.Tokens["ctx_hot"]}
}

// agentCtx is the snapshot row's ctx for pane, and whether s has the row.
func agentCtx(s snapshot.Snapshot, pane string) (*int, bool) {
	for _, g := range s.Groups {
		for _, a := range g.Agents {
			if a.PaneID == pane {
				return a.Ctx, true
			}
		}
	}
	return nil, false
}

// TestALastAgentsCtxIsNoLevel is A61 on a herdrtest team: a ctx stamped
// before its pane's agent process started is one an agent before it left,
// as a restarted agent finds it. From the first poll that sees the agent,
// CtxWatch tells no level for it, the sidebar shows "ctx  --" and the
// snapshot's ctx is null; once the agent stamps a ctx of its own, all three
// follow it. A poll whose agent.list fails gates it too, since CtxWatch and
// the sidebar read it then as well; such a poll caches no snapshot. A
// Claude pane: one rule for every agent.
func TestALastAgentsCtxIsNoLevel(t *testing.T) {
	t.Parallel()
	for name, failing := range map[string]bool{"agent.list answers": false, "agent.list fails": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			th.setAgents(named("wN:p1", "pee01"))
			th.failAgents(failing)
			th.setProcs("wN:p1", agentProc(1002, "claude"))
			th.setToken("wN:p1", report.CtxKey, "70")
			// The agent started at boot+10.02 s: its start, in ticks, is its pid.
			th.setToken("wN:p1", report.CtxAtKey, report.FormatModelAt(boot.Add(time.Second)))
			dir := shortDir(t)
			o := teamOpts(t, dir, th, noticesConfig(t, "gap = 0\n"))
			o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 77, 1002: 1002}}).startTime
			var logs lockedLog
			_, rec := ctxRun(t, o, &logs)
			sock := socketPath(t, dir, th.path)

			waitLine(t, &logs, "the daemon started")
			if failing {
				require.Eventually(t, func() bool { return strings.Count(logs.String(), "team poll: list agents") >= 2 }, ready, 5*time.Millisecond)
			} else {
				before := served(t, sock, func(s snapshot.Snapshot) bool { _, ok := agentCtx(s, "wN:p1"); return ok })
				ctx, _ := agentCtx(before, "wN:p1")
				assert.Nil(t, ctx, "the snapshot reads the last agent's 70% as unknown")
			}
			polled(t, th.srv, 3)
			assert.Empty(t, ctxEvents(rec), "the last agent's 70% is no level")
			assert.Equal(t, map[string]string{"ctx_ok": "ctx  --", "ctx_warn": "", "ctx_hot": ""}, lastBands(t, th, "wN:p1"))

			th.setToken("wN:p1", report.CtxAtKey, report.FormatModelAt(boot.Add(time.Minute)))
			require.Eventually(t, func() bool { return len(ctxEvents(rec)) > 0 }, ready, 5*time.Millisecond, "the agent's own 70%% is a level")
			assert.Equal(t, []string{"wN:p1 worker hard 70 65 false false"}, ctxEvents(rec))
			polled(t, th.srv, 1)
			assert.Equal(t, "ctx 70%", lastBands(t, th, "wN:p1")["ctx_hot"], "the band shows the agent's own")
			if !failing {
				waitLine(t, &logs, "pee01 is at 70% context (hard: 65%): rotate it now")
				after := served(t, sock, func(s snapshot.Snapshot) bool { ctx, _ := agentCtx(s, "wN:p1"); return ctx != nil })
				ctx, _ := agentCtx(after, "wN:p1")
				assert.Equal(t, 70, *ctx)
			}
		})
	}
}

// TestTeamStatusGatesALastAgentsCtx: the daemon's team status gates each
// member's ctx by the starts its poll last read (A61): a ctx stamped before
// the coder's agent process started is null, and one it stamps since is its
// own.
func TestTeamStatusGatesALastAgentsCtx(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	withTeam(th)
	th.setProcs("w14:p2", agentProc(1002, "claude"))
	th.setToken("w14:p2", report.CtxKey, "70")
	// The agent started at boot+10.02 s: its start, in ticks, is its pid.
	th.setToken("w14:p2", report.CtxAtKey, report.FormatModelAt(boot.Add(time.Second)))
	o := teamOpts(t, shortDir(t), th, workforceConfig(t))
	o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 77, 1002: 1002}}).startTime
	run(t, o)
	healthy(t, o.StateDir, th.fakeHerdr)
	polled(t, th.srv, 2)
	sock := socketPath(t, o.StateDir, th.path)
	coderCtx := func() *int {
		t.Helper()
		data, err := daemon.Workforce(t.Context(), sock, daemon.OpTeamStatus, workforce.StatusRequest{})
		require.NoError(t, err)
		var st workforce.StatusResult
		require.NoError(t, json.Unmarshal(data, &st))
		require.Len(t, st.Groups, 1)
		require.Len(t, st.Groups[0].Members, 1)
		return st.Groups[0].Members[0].Ctx
	}
	assert.Nil(t, coderCtx(), "the last agent's 70%")

	th.setToken("w14:p2", report.CtxAtKey, report.FormatModelAt(boot.Add(time.Minute)))
	got := coderCtx()
	require.NotNil(t, got, "the agent's own")
	assert.Equal(t, 70, *got)
}

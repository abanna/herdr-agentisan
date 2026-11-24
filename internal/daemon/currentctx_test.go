package daemon_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/settings"
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

// The previous agent can remain alive after a different job takes the pane.
func TestContextDoesNotFollowABackgroundAgent(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setProcs(pane, agentProc(1002, "claude"))
	// Linux rounds boot time down. This old report can appear later than
	// the replacement's wall-clock start by several hundred milliseconds.
	oldAt := boot.Add(20500 * time.Millisecond)
	showCtx(th, pane, "70", report.FormatModelAt(oldAt))
	repo := t.TempDir()
	marker := filepath.Join(repo, settings.BundleMarker)
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
	cfg := configDir(t, strings.Replace(liveTOML, "/x", repo, 1))
	o := teamOpts(t, shortDir(t), th, cfg)
	fg := fakeForeground(t, &o)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		ctx, _ := agentCtx(s, pane)
		return ctx != nil && *ctx == 70
	})
	statusCtx := func() *int {
		t.Helper()
		req := workforce.StatusRequest{Selector: workforce.Selector{Project: liveProject}}
		data, err := daemon.Workforce(t.Context(), sock, daemon.OpTeamStatus, req)
		require.NoError(t, err)
		var status workforce.StatusResult
		require.NoError(t, json.Unmarshal(data, &status))
		for _, group := range status.Groups {
			for _, member := range group.Members {
				if member.PaneID == pane {
					return member.Ctx
				}
			}
		}
		t.Fatalf("team status omitted %s", pane)
		return nil
	}
	assert.Equal(t, new(70), statusCtx())
	polled(t, th.srv, 2)
	assert.Equal(t, 1, processAsks(th.srv)[pane], "stable foreground costs no further asks")

	checkUnknown := func() {
		t.Helper()
		polled(t, th.srv, 2)
		ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
		require.True(t, present)
		assert.Nil(t, ctx, "new foreground agent inherited context")
		assert.Nil(t, statusCtx(), "team status uses the poll's gate")
		assert.Equal(t, map[string]string{"ctx_ok": "ctx  --", "ctx_warn": "", "ctx_hot": ""}, lastBands(t, th, pane))
	}

	// Keep 1002 alive with its original start. First the probe fails, then
	// succeeds without a process, then identifies the new foreground job.
	th.setShell(pane, 0, true)
	fg.Store(2002)
	checkUnknown()
	th.setProcs(pane)
	th.setShell(pane, 0, false)
	checkUnknown()
	misses := processAsks(th.srv)[pane]
	polled(t, th.srv, 2)
	assert.Greater(t, processAsks(th.srv)[pane], misses, "an unresolved replacement is retried each poll")

	// A poll of the plain shell in between must retain the old report stamp.
	th.setPane(pane, "agent", nil)
	polled(t, th.srv, 2)
	th.setProcs(pane, agentProc(2002, "claude"))
	th.setPane(pane, "agent", "claude")
	checkUnknown()
	showCtx(th, pane, "70", "")
	checkUnknown()

	showCtx(th, pane, "10", report.FormatModelAt(boot.Add(30*time.Second)))
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Equal(t, new(10), ctx)
	assert.Equal(t, new(10), statusCtx())
	assert.Equal(t, "ctx 10%", lastBands(t, th, pane)["ctx_ok"])
}

func TestContextNoticeIgnoresAnUnreadBackgroundReport(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setProcs(pane, agentProc(1002, "claude"))
	showCtx(th, pane, "10", report.FormatModelAt(boot.Add(12*time.Second)))
	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fg := fakeForeground(t, &o)
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		ctx, _ := agentCtx(s, pane)
		return ctx != nil && *ctx == 10
	})

	th.setProcs(pane, agentProc(2002, "claude"))
	fg.Store(2002)
	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(15*time.Second)))
	polled(t, th.srv, 2)
	assert.Empty(t, ctxEvents(rec), "no old crossing to deduplicate")
	assert.Empty(t, toasts(t, th), "no toast for the previous agent")

	// Check that B cannot claim the report on returning. Finish a poll
	// with the token cleared before placing its actual reporter in front.
	showCtx(th, pane, "", "")
	polled(t, th.srv, 2)
	th.setProcs(pane, agentProc(1002, "claude"))
	fg.Store(1002)
	polled(t, th.srv, 2)
	th.setProcs(pane, agentProc(2002, "claude"))
	fg.Store(2002)
	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(15*time.Second)))
	polled(t, th.srv, 2)
	assert.Empty(t, ctxEvents(rec), "a rejected report earns no retained stamp")

	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(30*time.Second)))
	polled(t, th.srv, 2)
	assert.Equal(t, []string{"wN:p1 worker ceiling 80 75 true true"}, ctxEvents(rec))
	assert.Len(t, toasts(t, th), 1)
}

func TestContextWithoutForegroundGroupIsUnknown(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setProcs(pane, agentProc(1002, "claude"))
	showCtx(th, pane, "10", report.FormatModelAt(boot.Add(12*time.Second)))
	o := teamOpts(t, shortDir(t), th, liveConfig(t, ""))
	fg := fakeForeground(t, &o)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, o.StateDir, th.path)
	reported := func(s snapshot.Snapshot) bool {
		ctx, _ := agentCtx(s, pane)
		return ctx != nil && *ctx == 10
	}
	served(t, sock, reported)

	// Equal zero groups do not verify the cached process. Even a reply
	// still listing that PID cannot establish a current foreground job.
	fg.Store(0)
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx)

	fg.Store(1002)
	served(t, sock, reported)
}

func TestContextReturnsWithTheOriginalForegroundProcess(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wJ:p1"
	th.setAgents(named(pane, liveShort+"-boss"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(1002, "codex"))
	stamp := report.FormatModelAt(boot.Add(20500 * time.Millisecond))
	showCtx(th, pane, "74", stamp)

	o := teamOpts(t, shortDir(t), th, liveConfig(t, ""))
	fg := fakeForeground(t, &o)
	var reused atomic.Bool
	stat := o.Hooks.ProcessStat
	o.Hooks.ProcessStat = func(pid int) (report.Stat, error) {
		s, err := stat(pid)
		if reused.Load() && pid == 1002 {
			s.Start++
		}
		return s, err
	}
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		return s.Boss != nil && s.Boss.Ctx != nil && *s.Boss.Ctx == 74
	})
	foreground := func(pid int32, want *int) {
		t.Helper()
		th.setProcs(pane, agentProc(int(pid), "codex"))
		fg.Store(pid)
		polled(t, th.srv, 2)
		boss := served(t, sock, anySnapshot).Boss
		require.NotNil(t, boss)
		assert.Equal(t, want, boss.Ctx, "foreground PID %d", pid)
	}

	foreground(2002, nil)
	foreground(1002, new(74))
	assert.Equal(t, "ctx 74%", lastBands(t, th, pane)["ctx_hot"])
	showCtx(th, pane, "65", report.FormatModelAt(boot.Add(20*time.Second)))
	foreground(1002, nil)
	showCtx(th, pane, "74", stamp)
	foreground(1002, new(74))

	// The same report still cannot pass for a different process that
	// happens to receive 1002. Its rounded wall start precedes the stamp.
	foreground(2002, nil)
	reused.Store(true)
	foreground(1002, nil)

	// A fresh report from 2002 belongs to it on later returns.
	foreground(2002, nil)
	showCtx(th, pane, "78", report.FormatModelAt(boot.Add(30*time.Second)))
	foreground(2002, new(78))
	foreground(1002, nil)
	foreground(2002, new(78))
}

func TestContextDoesNotAssignAnUnseenJobsReportToThePrecedingJob(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(2002, "codex"))
	showCtx(th, pane, "10", report.FormatModelAt(boot.Add(22*time.Second)))

	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fg := fakeForeground(t, &o)
	fg.Store(2002)
	stat := o.Hooks.ProcessStat
	o.Hooks.ProcessStat = func(pid int) (report.Stat, error) {
		if pid == 3002 {
			return report.Stat{Start: 3002, Pgrp: 3002, Tpgid: int(fg.Load())}, nil
		}
		return stat(pid)
	}
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		ctx, _ := agentCtx(s, pane)
		return ctx != nil && *ctx == 10
	})
	th.setPane(pane, "agent", nil)
	fg.Store(4002)
	polled(t, th.srv, 2)

	// A reported 80% between observations. The next foreground agent the
	// poll sees is C. A's stamp falls between B's and C's starts.
	th.setProcs(pane, agentProc(3002, "codex"))
	fg.Store(3002)
	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(25*time.Second)))
	th.setPane(pane, "agent", "codex")
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx, "C did not write this report")
	assert.Empty(t, ctxEvents(rec))

	th.setProcs(pane, agentProc(2002, "codex"))
	fg.Store(2002)
	polled(t, th.srv, 2)
	ctx, present = agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx, "B did not write it either")
	assert.Equal(t, "ctx  --", lastBands(t, th, pane)["ctx_ok"])
	assert.Empty(t, ctxEvents(rec))
	assert.Empty(t, toasts(t, th))

	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(35*time.Second)))
	polled(t, th.srv, 2)
	assert.Equal(t, []string{"wN:p1 worker ceiling 80 75 true true"}, ctxEvents(rec))
	assert.Len(t, toasts(t, th), 1)
}

func TestContextColdForegroundRejectionWaitsForStart(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(2002, "codex"))
	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(15*time.Second)))

	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fg := fakeForeground(t, &o)
	// Herdr listed B, but B has lost the foreground by the kernel read.
	fg.Store(4002)
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	checkUnknown := func() {
		t.Helper()
		polled(t, th.srv, 2)
		ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
		require.True(t, present)
		assert.Nil(t, ctx)
		assert.Equal(t, "ctx  --", lastBands(t, th, pane)["ctx_ok"])
		assert.Empty(t, ctxEvents(rec))
		assert.Empty(t, toasts(t, th))
	}
	checkUnknown()
	assert.Greater(t, processAsks(th.srv)[pane], 1, "the cold rejection is retried before ModelRetry")

	th.setShell(pane, 0, true)
	fg.Store(2002)
	checkUnknown()

	th.setShell(pane, 0, false)
	checkUnknown()
	showCtx(th, pane, "80", "")
	checkUnknown()
	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(35*time.Second)))
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Equal(t, new(80), ctx, "B's report counts after the probe recovers")
	assert.Equal(t, []string{"wN:p1 worker ceiling 80 75 true true"}, ctxEvents(rec))
}

func TestContextDoesNotRestoreAReportOlderThanThePreviousProcess(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setProcs(pane, agentProc(1002, "claude", "--model", "opus"))
	showCtx(th, pane, "", "")
	o := teamOpts(t, shortDir(t), th, liveConfig(t, ""))
	fg := fakeForeground(t, &o)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		for _, group := range s.Groups {
			for _, a := range group.Agents {
				if a.PaneID == pane && a.Model == "opus" {
					return true
				}
			}
		}
		return false
	})

	th.setProcs(pane, agentProc(2002, "claude"))
	fg.Store(2002)
	showCtx(th, pane, "80", report.FormatModelAt(boot.Add(5*time.Second)))
	polled(t, th.srv, 2)
	th.setProcs(pane, agentProc(1002, "claude"))
	fg.Store(1002)
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx, "a stamp older than both processes never qualifies")
}

// fakeForeground lets both agent PIDs survive while the foreground job
// changes. Start and process group match the PID for each fake agent.
func fakeForeground(t *testing.T, o *daemon.Options) *atomic.Int32 {
	t.Helper()
	fg := &atomic.Int32{}
	fg.Store(1002)
	pt := &procs{start: map[int]uint64{os.Getpid(): 77, 1002: 1002, 2002: 2002}}
	o.Hooks.ProcessStat = func(pid int) (report.Stat, error) {
		start, err := pt.startTime(pid)
		foreground := int(fg.Load())
		group := pid
		if foreground == 0 {
			group = 0
		}
		return report.Stat{Start: start, Pgrp: group, Tpgid: foreground}, err
	}
	return fg
}

func showCtx(th *teamHerdr, pane, ctx, at string) {
	th.setPane(pane, "tokens", map[string]string{report.CtxKey: ctx, report.CtxAtKey: at})
}

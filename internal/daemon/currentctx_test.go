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
	showCtx(th, pane, "70", 1002)
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
	assert.Greater(t, processAsks(th.srv)[pane], misses, "a lost agent's replacement is looked for each poll")

	th.setProcs(pane, agentProc(2002, "claude"))
	checkUnknown()

	showCtx(th, pane, "10", 2002)
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Equal(t, new(10), ctx)
	assert.Equal(t, new(10), statusCtx())
	assert.Equal(t, "ctx 10%", lastBands(t, th, pane)["ctx_ok"])
}

// A report another job wrote is no crossing for the agent in front, and the
// agent's own is one.
func TestContextNoticeIgnoresAnotherAgentsReport(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setProcs(pane, agentProc(1002, "claude"))
	showCtx(th, pane, "10", 1002)
	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fg := fakeForeground(t, &o)
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		ctx, _ := agentCtx(s, pane)
		return ctx != nil && *ctx == 10
	})

	// A background job reports 80% while 2002 is in front, and 1002 returns.
	th.setProcs(pane, agentProc(2002, "claude"))
	fg.Store(2002)
	showCtx(th, pane, "80", 1002)
	polled(t, th.srv, 2)
	th.setProcs(pane, agentProc(3002, "claude"))
	fg.Store(3002)
	polled(t, th.srv, 2)
	assert.Empty(t, ctxEvents(rec), "a report 3002 did not write")
	assert.Empty(t, toasts(t, th), "no toast for the previous agent")

	showCtx(th, pane, "80", 3002)
	polled(t, th.srv, 2)
	assert.Equal(t, []string{"wN:p1 worker ceiling 80 75 true true"}, ctxEvents(rec))
	assert.Len(t, toasts(t, th), 1)
}

func TestContextWithoutForegroundGroupIsUnknown(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setProcs(pane, agentProc(1002, "claude"))
	showCtx(th, pane, "10", 1002)
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

// The report names its writer, so a job that returns to the foreground has
// its own ctx back without the daemon having seen it write, and no other
// job, or a process that reused its pid, has it.
func TestContextReturnsWithTheOriginalForegroundProcess(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wJ:p1"
	th.setAgents(named(pane, liveShort+"-boss"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(1002, "codex"))
	showCtx(th, pane, "74", 1002)

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

	// A different process that happens to receive 1002 is not its writer.
	foreground(2002, nil)
	reused.Store(true)
	foreground(1002, nil)
	reused.Store(false)

	// 2002's own report replaces the token: it is the only owner now.
	foreground(2002, nil)
	showCtx(th, pane, "78", 2002)
	foreground(2002, new(78))
	foreground(1002, nil)
	foreground(2002, new(78))
}

// Daemon restart: the pane holds an exited agent's 80%, and the new agent B is
// listed but not in front at the kernel read, then in front. The 80% is
// hidden on both polls and B's own report shows (ha-precheck-1, scenario 4;
// the review of 5bbf3cf, P1 2).
func TestContextOfAnExitedAgentStaysHiddenAfterAColdRejection(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(2002, "codex"))
	showCtx(th, pane, "80", 5002) // 5002 exited

	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fg := fakeForeground(t, &o)
	fg.Store(1002) // B, 2002, has lost the foreground by the kernel read
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
	fg.Store(2002)
	checkUnknown()

	showCtx(th, pane, "30", 2002)
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Equal(t, new(30), ctx, "B's own report counts")
	assert.Empty(t, ctxEvents(rec))
}

// A, B, C (ha-precheck-1, scenario 3; the review of 5bbf3cf, P1 1): B is
// verified at 10%, is suspended, and A resumes and reports 80%. C launches
// and is suspended, and B returns. A report first seen while another job was
// in front is never given to B.
func TestContextOfAnotherJobIsNeverGivenToAReturningAgent(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(2002, "codex"))
	showCtx(th, pane, "10", 2002)
	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fg := fakeForeground(t, &o)
	fg.Store(2002)
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	ctxNow := func() *int {
		t.Helper()
		polled(t, th.srv, 2)
		ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
		require.True(t, present)
		return ctx
	}
	require.Equal(t, new(10), ctxNow())

	th.setProcs(pane, agentProc(4002, "/bin/sh")) // B suspended
	fg.Store(4002)
	assert.Nil(t, ctxNow())
	showCtx(th, pane, "80", 1002) // A resumed and reported
	th.setProcs(pane, agentProc(3002, "codex"))
	fg.Store(3002) // C launches, reports nothing
	assert.Nil(t, ctxNow(), "A's 80% is not C's")
	th.setProcs(pane, agentProc(4002, "/bin/sh")) // C suspended
	fg.Store(4002)
	assert.Nil(t, ctxNow())
	th.setProcs(pane, agentProc(2002, "codex")) // B returns
	fg.Store(2002)
	assert.Nil(t, ctxNow(), "A's 80% is not B's")
	assert.Empty(t, ctxEvents(rec))
	assert.Empty(t, toasts(t, th))

	showCtx(th, pane, "15", 2002)
	assert.Equal(t, new(15), ctxNow())
}

// A report first seen while another job is in front shows when its writer
// returns, with no hook needed (ha-precheck-1, scenario 5, which the earlier
// design could only leave blank).
func TestContextFirstSeenInAnotherJobsTurnShowsWhenItsWriterReturns(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(2002, "codex"))
	showCtx(th, pane, "80", 1002)
	o := teamOpts(t, shortDir(t), th, liveConfig(t, ""))
	fg := fakeForeground(t, &o)
	fg.Store(2002)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, o.StateDir, th.path)
	polled(t, th.srv, 3)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx, "B is in front")

	th.setProcs(pane, agentProc(1002, "codex"))
	fg.Store(1002)
	got := served(t, sock, func(s snapshot.Snapshot) bool { c, _ := agentCtx(s, pane); return c != nil })
	ctx, _ = agentCtx(got, pane)
	assert.Equal(t, new(80), ctx)
}

// A staggered upgrade: an older reporter writes B's ctx and ctx_at and leaves
// A's ctx_owner where it was. The owner belongs to the stamp it was written
// with, so A's returning does not give A B's 80% (review of 13d84f7, P1 1).
func TestContextOwnerNeverOutlivesItsCtx(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(1002, "codex"))
	showCtxAt(th, pane, "20", boot.Add(time.Hour), 1002)
	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fakeForeground(t, &o)
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		ctx, _ := agentCtx(s, pane)
		return ctx != nil && *ctx == 20
	})

	// B's report, by a build that knows no ctx_owner: new ctx and ctx_at, the
	// old owner stays. B then exits and A is in front again.
	tokens := map[string]string{
		report.CtxKey: "80", report.CtxAtKey: report.FormatModelAt(boot.Add(2 * time.Hour)),
		report.CtxOwnerKey: report.FormatCtxOwner(boot.Add(time.Hour), "20", []report.Proc{{PID: 1002, Start: 1002}}),
	}
	th.setPane(pane, "tokens", tokens)
	polled(t, th.srv, 3)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx, "B's 80% is not A's")
	assert.Equal(t, "ctx  --", lastBands(t, th, pane)["ctx_ok"])
	assert.Empty(t, ctxEvents(rec))
	assert.Empty(t, toasts(t, th))

	showCtxAt(th, pane, "30", boot.Add(3*time.Hour), 1002)
	polled(t, th.srv, 2)
	ctx, _ = agentCtx(served(t, sock, anySnapshot), pane)
	assert.Equal(t, new(30), ctx, "A's own next report counts")
}

// Two jobs report in one millisecond (review of cf0df58, P1 2): A's report
// is stamped, then an older build's B writes ctx and ctx_at, the same
// millisecond, over A's owner. B's 80% is not A's.
func TestContextOfALegacyWriterInTheSameMillisecondIsHidden(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(1002, "codex"))
	at := boot.Add(time.Hour)
	showCtxAt(th, pane, "20", at, 1002)
	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fakeForeground(t, &o)
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	served(t, sock, func(s snapshot.Snapshot) bool {
		ctx, _ := agentCtx(s, pane)
		return ctx != nil && *ctx == 20
	})

	th.setPane(pane, "tokens", map[string]string{
		report.CtxKey: "80", report.CtxAtKey: report.FormatModelAt(at), report.CtxOwnerKey: ownerOf("20", at, 1002),
	})
	polled(t, th.srv, 3)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx, "B's 80% is not A's")
	assert.Empty(t, ctxEvents(rec))
	assert.Empty(t, toasts(t, th))
}

// A restart finds an exited A's 99% from a build that wrote no owner. B is
// listed, but is suspended when the kernel is read (review of cf0df58, P1 3):
// B's start still dates the report, as it did before the foreground check,
// so A's older 99% is hidden and raises nothing.
func TestLegacyContextOfAnExitedAgentStaysHiddenWhenItsReplacementIsRejected(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	const pane = "wN:p1"
	th.setAgents(named(pane, "pee01"))
	th.setPane(pane, "agent", "codex")
	th.setProcs(pane, agentProc(2002, "codex"))
	// B, 2002, started at boot+20.02 s: A's report is older.
	th.setPane(pane, "tokens", map[string]string{report.CtxKey: "99", report.CtxAtKey: report.FormatModelAt(boot.Add(time.Second))})
	o := teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n"))
	fg := fakeForeground(t, &o)
	fg.Store(1002)
	var logs lockedLog
	_, rec := ctxRun(t, o, &logs)
	sock := socketPath(t, o.StateDir, th.path)
	polled(t, th.srv, 2)
	ctx, present := agentCtx(served(t, sock, anySnapshot), pane)
	require.True(t, present)
	assert.Nil(t, ctx, "A's 99% is older than B")
	assert.Equal(t, "ctx  --", lastBands(t, th, pane)["ctx_ok"])
	assert.Empty(t, ctxEvents(rec))
	assert.Empty(t, toasts(t, th))
}

// fakeForeground lets the agent PIDs survive while the foreground job
// changes. Start and process group match the PID for each fake agent.
func fakeForeground(t *testing.T, o *daemon.Options) *atomic.Int32 {
	t.Helper()
	fg := &atomic.Int32{}
	fg.Store(1002)
	pt := &procs{start: map[int]uint64{os.Getpid(): 77, 1002: 1002, 2002: 2002, 3002: 3002}}
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

// showCtx is a ctx report as the current build writes it: dated after every
// fake agent started, and naming the fake agents, whose start is their pid,
// that wrote it.
func showCtx(th *teamHerdr, pane, ctx string, owner ...int) {
	showCtxAt(th, pane, ctx, boot.Add(time.Hour), owner...)
}

// showCtxAt is showCtx stamped at.
func showCtxAt(th *teamHerdr, pane, ctx string, at time.Time, owner ...int) {
	th.setPane(pane, "tokens", map[string]string{
		report.CtxKey: ctx, report.CtxAtKey: report.FormatModelAt(at), report.CtxOwnerKey: ownerOf(ctx, at, owner...),
	})
}

// ownerOf is the ctx_owner a report of ctx made at at by the fake agents
// owner writes.
func ownerOf(ctx string, at time.Time, owner ...int) string {
	procs := make([]report.Proc, 0, len(owner))
	for _, pid := range owner {
		procs = append(procs, report.Proc{PID: pid, Start: uint64(pid)}) //nolint:gosec // small test pids
	}
	return report.FormatCtxOwner(at, ctx, procs)
}

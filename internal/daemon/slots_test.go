package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/testslots"
)

// slotsConfig writes a config dir naming project agentisan, with no colour,
// and [tests] locks.
func slotsConfig(t *testing.T, locks ...string) string {
	t.Helper()
	quoted := make([]string, len(locks))
	for i, l := range locks {
		quoted[i] = fmt.Sprintf("%q", l)
	}
	dir := t.TempDir()
	body := "schema_version = 1\n[tests]\nlocks = [" + strings.Join(quoted, ", ") + "]\n[projects.agentisan]\nrepo = \"~/Development/agentisan\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(body), 0o600))
	return dir
}

// hold takes an exclusive flock on path, creating it, as a test suite's
// gate would; the returned func releases it.
func hold(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	return func() { require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)) }
}

func slotsAre(used, total int) func(snapshot.Snapshot) bool {
	return func(s snapshot.Snapshot) bool {
		return s.Project != nil && s.Project.Slots != nil && *s.Project.Slots == snapshot.Slots{Used: used, Total: total}
	}
}

// TestSnapshotCarriesTheTestSlots: the snapshot's project carries its test
// slots (ADR-001 A31): which of [tests] locks some process holds a lock on,
// as the kernel lists them, counted every poll. A suite taking a slot shows,
// and its slot frees when it lets go. A held file that is not listed counts
// for nothing, and a project with no colour still gets its slots.
func TestSnapshotCarriesTheTestSlots(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(testslots.ProcLocks); err != nil {
		t.Skipf("no %s: %v", testslots.ProcLocks, err)
	}
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	locks := t.TempDir()
	gate0, gate1 := filepath.Join(locks, "gate.0.lock"), filepath.Join(locks, "gate.1.lock")
	release := hold(t, gate0)
	hold(t, filepath.Join(locks, "decoy.lock"))
	dir := shortDir(t)
	o := teamOpts(t, dir, th, slotsConfig(t, gate0, gate1))
	o.Now = func() time.Time { return t0 }
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)

	taken := served(t, sock, slotsAre(1, 2))
	assert.Equal(t, &snapshot.Project{Slots: &snapshot.Slots{Used: 1, Total: 2}}, taken.Project)
	assert.Equal(t, wantFullTeam().Groups, taken.Groups, "the team is served as before")

	release()
	served(t, sock, slotsAre(0, 2))
	hold(t, gate1)
	served(t, sock, slotsAre(1, 2))
}

// TestTestSlotsNotCounted: with no [tests] locks, or a file that does not
// resolve, no slots are reported. When the kernel's lock list cannot be
// read, none are either, never all free; that is logged once however many
// polls it lasts, and the slots return, with one more line, once it can be.
func TestTestSlotsNotCounted(t *testing.T) {
	t.Parallel()
	broken := func(t *testing.T) string {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[tests]\nlocks = [\"gate.lock\"]\n"), 0o600))
		return dir
	}
	tests := map[string]struct {
		config  func(t *testing.T) string
		wantLog string
	}{
		"no config":       {config: func(*testing.T) string { return "" }},
		"no [tests]":      {config: projectConfig},
		"a broken config": {config: broken, wantLog: "test slots: herdr-agentisan.toml does not resolve"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			th.layout(fullTeam())
			var logs lockedLog
			dir := shortDir(t)
			o := teamOpts(t, dir, th, tc.config(t))
			o.Logger = zerolog.New(&logs)
			serving(t, o, th.fakeHerdr)

			served(t, socketPath(t, dir, th.path), anySnapshot)
			polled(t, th.srv, 3)
			got := served(t, socketPath(t, dir, th.path), anySnapshot)
			assert.True(t, got.Project == nil || got.Project.Slots == nil, "no slots reported")
			if tc.wantLog != "" {
				assert.Equal(t, 1, strings.Count(logs.String(), tc.wantLog), "logged once")
			} else {
				assert.NotContains(t, logs.String(), "test slots:")
			}
		})
	}

	t.Run("the lock list cannot be read, then can", func(t *testing.T) {
		t.Parallel()
		th := newTeamHerdr(t)
		th.layout(fullTeam())
		locks := t.TempDir()
		standIn := filepath.Join(locks, "proc-locks")
		var logs lockedLog
		dir := shortDir(t)
		o := teamOpts(t, dir, th, slotsConfig(t, filepath.Join(locks, "gate.0.lock"), filepath.Join(locks, "gate.1.lock")))
		o.Team = new(daemon.WithProcLocks(*o.Team, standIn))
		o.Logger = zerolog.New(&logs)
		serving(t, o, th.fakeHerdr)
		sock := socketPath(t, dir, th.path)

		served(t, sock, anySnapshot)
		polled(t, th.srv, 5)
		got := served(t, sock, anySnapshot)
		assert.True(t, got.Project == nil || got.Project.Slots == nil, "no slots while they cannot be counted")
		assert.Equal(t, 1, strings.Count(logs.String(), "test slots: cannot count them"), "logged once over five polls")

		require.NoError(t, os.WriteFile(standIn, nil, 0o600))
		served(t, sock, slotsAre(0, 2))
		assert.Equal(t, 1, strings.Count(logs.String(), "test slots: counting again"))
		assert.Equal(t, 1, strings.Count(logs.String(), `"message":"test slots: counting"}`), "the locks logged once, at start")
	})
}

// TestTestSlotsLogABadListOnce: a lock list the daemon cannot read is
// logged once while it stays unreadable, even as the line it stops at moves
// with other locks coming and going.
func TestTestSlotsLogABadListOnce(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	locks := t.TempDir()
	standIn := filepath.Join(locks, "proc-locks")
	require.NoError(t, os.WriteFile(standIn, []byte("1: FLOCK\n"), 0o600))
	var logs lockedLog
	dir := shortDir(t)
	o := teamOpts(t, dir, th, slotsConfig(t, filepath.Join(locks, "gate.0.lock")))
	o.Team = new(daemon.WithProcLocks(*o.Team, standIn))
	o.Logger = zerolog.New(&logs)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)

	served(t, sock, anySnapshot)
	polled(t, th.srv, 3)
	// Replaced whole, as the kernel's list is: a poll must never read it half
	// written, which would be a good list and a recovery.
	next := standIn + ".next"
	require.NoError(t, os.WriteFile(next, []byte("1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n2: FLOCK\n"), 0o600))
	require.NoError(t, os.Rename(next, standIn))
	polled(t, th.srv, 3)
	got := served(t, sock, anySnapshot)
	assert.True(t, got.Project == nil || got.Project.Slots == nil, "no slots while the list cannot be read")
	assert.Equal(t, 1, strings.Count(logs.String(), "test slots: cannot count them"), "logged once, though the bad line moved from 1 to 2")
	assert.Contains(t, logs.String(), "line 1", "the first failure is the one logged")
}

// setProcs sets pane.process_info's foreground processes for the pane id.
func (th *teamHerdr) setProcs(id string, procs ...map[string]any) {
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.procs == nil {
		th.procs = map[string][]map[string]any{}
	}
	th.procs[id] = procs
}

// TestSnapshotBossRuntime: the boss's started_at is when its agent process
// started, from the pane's processes and the OS's start time on the wall
// clock (A47), in UTC. It keeps that start poll after poll; a new agent
// process in the pane, or a handoff to another pane, starts it again at
// that process's start; with no agent process found it is left out, never
// guessed from when the daemon saw the pane.
func TestSnapshotBossRuntime(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws := []herdr.WorkspaceInfo{{WorkspaceID: "wJ", Label: "◆ boss"}, {WorkspaceID: "wN", Label: "◆ coders"}}
	bossIn := func(id string) []map[string]any {
		return []map[string]any{
			teamPane(id, "wJ", "claude", "working", "", nil),
			teamPane("wN:p1", "wN", "claude", "working", "", nil),
		}
	}
	agents := []map[string]any{named("wJ:p1", "boss"), named("wJ:p2", "boss2"), named("wN:p1", "pee01")}
	th.layout(ws, bossIn("wJ:p1"), agents)
	th.setProcs("wJ:p1", agentProc(1001, "claude", "--model", "opus"))
	th.setProcs("wJ:p2", agentProc(1003, "claude"))
	th.setProcs("wN:p1", agentProc(1002, "claude"))
	pt := &procs{start: map[int]uint64{os.Getpid(): 77, 1001: 4200, 1002: 5000, 1003: 6000}}
	c := &clock{now: t0}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, "")
	o.Hooks.StartTime = pt.startTime
	o.Now = c.Now
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)
	// ticks is when a process ticksSinceBoot places at n ticks started.
	ticks := func(n int) time.Time { return boot.Add(time.Duration(n) * 10 * time.Millisecond) }
	startedAt := func(want time.Time) func(snapshot.Snapshot) bool {
		return func(s snapshot.Snapshot) bool {
			return s.Boss != nil && s.Boss.StartedAt != nil && s.Boss.StartedAt.Equal(want)
		}
	}

	first := served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t0) && s.Boss != nil && s.Boss.StartedAt != nil })
	assert.Equal(t, ticks(4200), *first.Boss.StartedAt, "its agent process's start, not the daemon's first sight of it")
	assert.Equal(t, time.UTC, first.Boss.StartedAt.Location())

	c.advance(3 * time.Second)
	t1 := t0.Add(3 * time.Second)
	later := served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t1) })
	require.NotNil(t, later.Boss.StartedAt)
	assert.Equal(t, ticks(4200), *later.Boss.StartedAt, "the same process keeps its start")

	// The boss's session is started again in its pane: another process.
	exit(pt, 1001)
	pt.set(1004, 7000)
	th.setProcs("wJ:p1", agentProc(1004, "claude"))
	served(t, sock, startedAt(ticks(7000)))

	th.layout(ws, bossIn("wJ:p2"), agents)
	handed := served(t, sock, func(s snapshot.Snapshot) bool {
		return s.Boss != nil && s.Boss.PaneID == "wJ:p2" && s.Boss.StartedAt != nil
	})
	assert.Equal(t, ticks(6000), *handed.Boss.StartedAt, "a handoff takes the new pane's agent process's start")

	// herdr still detects an agent, but the pane's processes show none.
	exit(pt, 1003)
	th.setProcs("wJ:p2", agentProc(1005, "/bin/zsh"))
	unread := served(t, sock, func(s snapshot.Snapshot) bool {
		return s.Boss != nil && s.Boss.PaneID == "wJ:p2" && s.Boss.StartedAt == nil
	})
	assert.Equal(t, "boss2", unread.Boss.Name, "still the boss, with no runtime")
}

// TestBossRuntimeSurvivesADaemonRestart: a daemon that follows one that
// exited reads the same boss agent's start again (A47), so the runtime goes
// on counting from it instead of starting at the new daemon's first poll.
func TestBossRuntimeSurvivesADaemonRestart(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws := []herdr.WorkspaceInfo{{WorkspaceID: "wJ", Label: "◆ boss"}}
	th.layout(ws, []map[string]any{teamPane("wJ:p1", "wJ", "claude", "working", "", nil)}, []map[string]any{named("wJ:p1", "boss")})
	th.setProcs("wJ:p1", agentProc(1001, "claude"))
	started := boot.Add(42 * time.Second)
	c := &clock{now: t0}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, "")
	o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 77, 1001: 4200}}).startTime
	o.Now = c.Now
	first := serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)
	before := served(t, sock, func(s snapshot.Snapshot) bool { return s.Boss != nil && s.Boss.StartedAt != nil })
	assert.Equal(t, started, *before.Boss.StartedAt)

	first.cancel()
	require.NoError(t, first.wait(t, ready))
	c.advance(time.Hour)
	// As restarted gives it, a later start for this process, which the next
	// daemon runs as; the boss's agent is the same process.
	next := o
	next.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 78, 1001: 4200}}).startTime
	serving(t, next, th.fakeHerdr)
	after := served(t, sock, func(s snapshot.Snapshot) bool {
		return s.At.Equal(t0.Add(time.Hour)) && s.Boss != nil && s.Boss.StartedAt != nil
	})
	assert.Equal(t, started, *after.Boss.StartedAt, "the runtime survives the restart, not counted from the new daemon's first poll")
}

// servedTeam waits until the daemon at sock serves project's snapshot and ok
// holds of it, and returns it. A wait that runs out reports the last answer
// read.
func servedTeam(t *testing.T, sock, project string, ok func(snapshot.Snapshot) bool) snapshot.Snapshot {
	t.Helper()
	var got snapshot.Snapshot
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		s, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: project})
		if !assert.NoError(c, err) {
			return
		}
		got = s
		assert.Truef(c, ok(s), "team %s was never served as wanted; last: %+v", project, s)
	}, ready, 5*time.Millisecond)
	return got
}

// nameAgent sets agent.list's entry for the pane id to name, or drops it for
// "".
func nameAgent(th *teamHerdr, id, name string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	out := make([]map[string]any, 0, len(th.agents)+1)
	for _, a := range th.agents {
		if a["pane_id"] != id {
			out = append(out, a)
		}
	}
	if name != "" {
		out = append(out, named(id, name))
	}
	th.agents = out
}

// swapPane replaces the pane id in pane.list with p.
func swapPane(th *teamHerdr, id string, p map[string]any) {
	panes := th.listedPanes()
	for i := range panes {
		if panes[i]["pane_id"] == id {
			panes[i] = p
		}
	}
	th.setPanes(panes)
}

// replaceFile puts content at path in one rename, so a reader opens the old
// file or the new one, never one half written.
func replaceFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".new"
	require.NoError(t, os.WriteFile(tmp, []byte(content), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

// TestEveryTeamCarriesTheSlotsAndItsOwnBoss: with one snapshot per team
// (A24), every team's carries the same test slots, from the poll's one
// reading of the held locks (A31, A40), and a slot let go before a poll
// stamped t1 is free for every team at t1: the poll takes its time before it
// reads the locks. Each team's boss.started_at is its own boss agent's
// process start (A47), and each pane's stage clock its own: a boss starting
// in one team, or a coder moving on there, restarts nothing in another. The
// held locks are a stand-in /proc/locks, so no lock elsewhere on the
// machine, and no page of the real file, can move a count.
func TestEveryTeamCarriesTheSlotsAndItsOwnBoss(t *testing.T) {
	t.Parallel()
	locks := t.TempDir()
	gate0, gate1 := filepath.Join(locks, "gate.0.lock"), filepath.Join(locks, "gate.1.lock")
	for _, f := range []string{gate0, gate1} {
		require.NoError(t, os.WriteFile(f, nil, 0o600))
	}
	standIn := filepath.Join(locks, "proc-locks")
	replaceFile(t, standIn, lockLine(t, 1, 200, gate0))
	th := newTeamHerdr(t)
	everyTeam(th)
	th.setProcs("wJ:p1", agentProc(1001, "claude"))
	th.setProcs("w14:p1", agentProc(1010, "claude"))
	// alpha's boss tab runs no agent yet.
	swapPane(th, "w14:p1", tabPane("w14:p1", "w14", "w14:t1", "", "unknown", nil))
	nameAgent(th, "w14:p1", "")
	c := &clock{now: t0}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, configDir(t, everyTeamConfig+fmt.Sprintf("[tests]\nlocks = [%q, %q]\n", gate0, gate1)))
	o.Team = new(daemon.WithProc(daemon.WithProcLocks(*o.Team, standIn), t.TempDir()))
	o.Hooks.StartTime = (&procs{start: map[int]uint64{os.Getpid(): 77, 1001: 4200, 1010: 9000}}).startTime
	o.Now = c.Now
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)
	at := func(ts time.Time) func(snapshot.Snapshot) bool {
		return func(s snapshot.Snapshot) bool { return s.At.Equal(ts) }
	}
	teams := []string{"agentisan", "alpha", "beta"}

	for _, p := range teams {
		s := servedTeam(t, sock, p, at(t0))
		require.NotNil(t, s.Project, p)
		assert.Equal(t, &snapshot.Slots{Used: 1, Total: 2}, s.Project.Slots, "team %s carries the slots", p)
	}
	assert.Nil(t, servedTeam(t, sock, "alpha", at(t0)).Boss, "alpha has no boss yet")
	spaces := servedTeam(t, sock, "agentisan", func(s snapshot.Snapshot) bool {
		return at(t0)(s) && s.Boss != nil && s.Boss.StartedAt != nil
	})
	assert.Equal(t, boot.Add(42*time.Second), *spaces.Boss.StartedAt, "the spaces' boss agent's start")

	// The held slot is let go, then the clock moves to t1 and alpha's boss
	// starts and alpha's coder moves on to review.
	replaceFile(t, standIn, "")
	c.advance(3 * time.Second)
	t1 := t0.Add(3 * time.Second)
	swapPane(th, "w14:p1", tabPane("w14:p1", "w14", "w14:t1", "claude", "working", nil))
	nameAgent(th, "w14:p1", "al-boss")
	swapPane(th, "w14:p3", tabPane("w14:p3", "w14", "w14:t3", "claude", "working", map[string]string{"ctx": "50", "item": "NERD-5285", "stage": "review"}))

	alpha := servedTeam(t, sock, "alpha", func(s snapshot.Snapshot) bool {
		return at(t1)(s) && s.Boss != nil && s.Boss.Status == "working" && s.Boss.StartedAt != nil && s.Groups[0].Agents[0].Stage == "review"
	})
	assert.Equal(t, boot.Add(90*time.Second), *alpha.Boss.StartedAt, "alpha's boss runtime is its own agent's")
	require.NotNil(t, alpha.Groups[0].Agents[0].StageStartedAt)
	assert.Equal(t, t1, *alpha.Groups[0].Agents[0].StageStartedAt, "alpha's coder's stage restarted")
	spaces = servedTeam(t, sock, "agentisan", at(t1))
	require.NotNil(t, spaces.Boss.StartedAt)
	assert.Equal(t, boot.Add(42*time.Second), *spaces.Boss.StartedAt, "the spaces' boss keeps its own start")
	require.NotNil(t, spaces.Groups[0].Agents[0].StageStartedAt)
	assert.Equal(t, t0, *spaces.Groups[0].Agents[0].StageStartedAt, "the spaces' coder keeps its stage start")
	for _, p := range teams {
		s := servedTeam(t, sock, p, at(t1))
		require.NotNil(t, s.Project, p)
		assert.Equal(t, &snapshot.Slots{Used: 0, Total: 2}, s.Project.Slots, "team %s at t1 counts the slot freed", p)
	}
}

// TestAPollReadsTheHeldLocksOnceForEveryTeam: a poll reads the held locks
// once, and every team's snapshot counts from that one reading (A31, A40),
// never from a reading of its own (NERD-5349's TM3, QA #48's SL2). The
// stand-in's readings alternate held and free, and the poll stops in its
// fourth: the teams' snapshots are then all the last whole poll's, its one
// reading the third, held. A reading per team would have given the first
// poll's three teams three readings, held, free and held.
func TestAPollReadsTheHeldLocksOnceForEveryTeam(t *testing.T) {
	t.Parallel()
	locks := t.TempDir()
	gate0, gate1 := filepath.Join(locks, "gate.0.lock"), filepath.Join(locks, "gate.1.lock")
	for _, f := range []string{gate0, gate1} {
		require.NoError(t, os.WriteFile(f, nil, 0o600))
	}
	held, free := filepath.Join(locks, "held"), filepath.Join(locks, "free")
	replaceFile(t, held, lockLine(t, 1, 200, gate0))
	replaceFile(t, free, "")
	const stopAt = 4
	var reads atomic.Int32
	stopped, release := make(chan struct{}), make(chan struct{})
	read := func(_, proc string) (*testslots.Reading, error) {
		n := reads.Add(1)
		if n == stopAt {
			close(stopped)
			<-release
		}
		if n%2 == 1 {
			return testslots.Read(held, proc)
		}
		return testslots.Read(free, proc)
	}
	th := newTeamHerdr(t)
	everyTeam(th)
	dir := shortDir(t)
	o := teamOpts(t, dir, th, configDir(t, everyTeamConfig+fmt.Sprintf("[tests]\nlocks = [%q, %q]\n", gate0, gate1)))
	o.Team = new(daemon.WithLockReads(daemon.WithProc(*o.Team, t.TempDir()), read))
	run(t, o)
	t.Cleanup(func() { close(release) }) // before run's own cleanup stops the daemon
	sock := socketPath(t, dir, th.path)

	select {
	case <-stopped:
	case <-time.After(ready):
		t.Fatalf("the polls made %d readings, never %d", reads.Load(), stopAt)
	}
	for _, p := range []string{"agentisan", "alpha", "beta"} {
		s := servedTeam(t, sock, p, func(snapshot.Snapshot) bool { return true })
		require.NotNil(t, s.Project, p)
		assert.Equal(t, &snapshot.Slots{Used: 1, Total: 2}, s.Project.Slots, "team %s counts the last whole poll's one reading", p)
	}
	assert.EqualValues(t, stopAt, reads.Load(), "nothing reads past the stopped poll")
}

// lockLine is a /proc/locks line: pid holding an exclusive flock on path.
func lockLine(t *testing.T, n, pid int, path string) string {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	dev := uint64(st.Dev) //nolint:unconvert // Dev is int32 on darwin
	return fmt.Sprintf("%d: FLOCK  ADVISORY  WRITE %d %02x:%02x:%d 0 EOF\n", n, pid, unix.Major(dev), unix.Minor(dev), st.Ino)
}

// TestEachTeamCountsItsOwnLocks: each team counts its own project's lock
// files, from one reading of the held locks for every team (A40). A
// project's own list replaces the shared one, an empty one included; a slot
// a hold keeps is held, not used. A lock file one project cannot look at
// costs that project's slots alone, logged once for it; the lock list itself
// read every poll, so nothing logs a recovery (QA #58's D8).
func TestEachTeamCountsItsOwnLocks(t *testing.T) {
	t.Parallel()
	locks := t.TempDir()
	gate, s1, s2, s3 := filepath.Join(locks, "gate.0.lock"), filepath.Join(locks, "s1.lock"), filepath.Join(locks, "s2.lock"), filepath.Join(locks, "s3.lock")
	for _, p := range []string{gate, s1, s2} {
		require.NoError(t, os.WriteFile(p, nil, 0o600))
	}
	// A stand-in /proc: pid 100 is the boss's hold on s1.
	proc := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "100", "fd"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(proc, "100", "cmdline"), []byte("flock\x00"+s1+"\x00sleep\x0086400\x00"), 0o600))
	require.NoError(t, os.Symlink(s1, filepath.Join(proc, "100", "fd", "3")))
	standIn := filepath.Join(locks, "proc-locks")
	require.NoError(t, os.WriteFile(standIn, []byte(lockLine(t, 1, 200, gate)+lockLine(t, 2, 100, s1)+lockLine(t, 3, 201, s2)), 0o600))
	notDir := filepath.Join(locks, "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))

	tests := map[string]struct {
		alpha     string // alpha's own list, after its table
		wantAlpha *snapshot.Slots
		wantLog   string
	}{
		"its own list": {
			alpha:     fmt.Sprintf("[projects.alpha.tests]\nlocks = [%q, %q, %q]\n", s1, s2, s3),
			wantAlpha: &snapshot.Slots{Used: 1, Held: 1, Total: 3},
		},
		"a lock file it cannot look at": {
			alpha:   fmt.Sprintf("[projects.alpha.tests]\nlocks = [%q, %q]\n", s1, filepath.Join(notDir, "x.lock")),
			wantLog: "test slots: cannot count them",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			everyTeam(th)
			var logs lockedLog
			dir := shortDir(t)
			config := everyTeamConfig + fmt.Sprintf("[tests]\nlocks = [%q]\n", gate) + tc.alpha + "[projects.beta.tests]\nlocks = []\n"
			o := teamOpts(t, dir, th, configDir(t, config))
			o.Team = new(daemon.WithProc(daemon.WithProcLocks(*o.Team, standIn), proc))
			o.Logger = zerolog.New(&logs)
			serving(t, o, th.fakeHerdr)
			sock := socketPath(t, dir, th.path)

			spaces := servedTeam(t, sock, "agentisan", slotsAre(1, 1))
			assert.Equal(t, &snapshot.Slots{Used: 1, Total: 1}, spaces.Project.Slots, "the spaces count the shared list")
			polled(t, th.srv, 3)
			alpha := servedTeam(t, sock, "alpha", anySnapshot)
			if tc.wantAlpha == nil {
				assert.True(t, alpha.Project == nil || alpha.Project.Slots == nil, "alpha reports no slots")
			} else {
				require.NotNil(t, alpha.Project)
				assert.Equal(t, tc.wantAlpha, alpha.Project.Slots, "alpha counts its own list")
			}
			beta := servedTeam(t, sock, "beta", anySnapshot)
			assert.True(t, beta.Project == nil || beta.Project.Slots == nil, "beta's empty list reports no slots")
			assert.Equal(t, &snapshot.Slots{Used: 1, Total: 1}, servedTeam(t, sock, "agentisan", anySnapshot).Project.Slots, "alpha's failure costs the spaces nothing")
			if tc.wantLog != "" {
				assert.Equal(t, 1, strings.Count(logs.String(), tc.wantLog), "logged once over the polls")
				assert.Contains(t, logs.String(), `"project":"alpha"`)
			} else {
				assert.NotContains(t, logs.String(), "cannot count")
			}
			assert.NotContains(t, logs.String(), "counting again", "nothing failed that could recover")
		})
	}
}

// TestAProjectsLockFileRecovers: a project whose lock file cannot be looked
// at reports no slots, logged once for it, and its slots return, with one
// more line, once the file can be looked at again (A40).
func TestAProjectsLockFileRecovers(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root looks into a directory it may not search")
	}
	locks := t.TempDir()
	shut := filepath.Join(locks, "shut")
	require.NoError(t, os.MkdirAll(filepath.Join(shut, "in"), 0o700))
	slot := filepath.Join(shut, "in", "slot.lock")
	require.NoError(t, os.WriteFile(slot, nil, 0o600))
	standIn := filepath.Join(locks, "proc-locks")
	require.NoError(t, os.WriteFile(standIn, []byte(lockLine(t, 1, 200, slot)), 0o600))
	require.NoError(t, os.Chmod(shut, 0o000))
	t.Cleanup(func() { _ = os.Chmod(shut, 0o700) })

	th := newTeamHerdr(t)
	everyTeam(th)
	var logs lockedLog
	dir := shortDir(t)
	o := teamOpts(t, dir, th, configDir(t, everyTeamConfig+fmt.Sprintf("[projects.alpha.tests]\nlocks = [%q]\n", slot)))
	o.Team = new(daemon.WithProc(daemon.WithProcLocks(*o.Team, standIn), t.TempDir()))
	o.Logger = zerolog.New(&logs)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)

	servedTeam(t, sock, "alpha", anySnapshot)
	polled(t, th.srv, 3)
	alpha := servedTeam(t, sock, "alpha", anySnapshot)
	assert.True(t, alpha.Project == nil || alpha.Project.Slots == nil, "no slots while the lock file cannot be looked at")
	assert.Equal(t, 1, strings.Count(logs.String(), "test slots: cannot count them"), "logged once over the polls")

	require.NoError(t, os.Chmod(shut, 0o700))
	servedTeam(t, sock, "alpha", slotsAre(1, 1))
	polled(t, th.srv, 2)
	assert.Equal(t, 1, strings.Count(logs.String(), `"project":"alpha","message":"test slots: counting again"`), "the recovery logged once")
}

// TestOnePollOneReading: a poll reads the lock list once, and every project
// counts its lock files from that one reading (A40). The stand-in
// /proc/locks is a FIFO whose first two passes list a lock as held, both
// within the poll's first reading, three staggered reads of the list, and
// whose every later pass lists nothing: two projects that list that lock
// count it held, the same, where a reading of its own for the second would
// read it free (QA #58, F1).
func TestOnePollOneReading(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock := filepath.Join(dir, "gate.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0o600))
	fifo := filepath.Join(dir, "locks")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	// Each open of the FIFO is one pass; the first two, both in the first
	// reading, get the held lock.
	held := lockLine(t, 1, 999999, lock)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for pass := 0; ; pass++ {
			f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err != nil || stop.Load() {
				if f != nil {
					_ = f.Close()
				}
				return
			}
			if pass < 2 {
				_, _ = f.WriteString(held)
			}
			_ = f.Close()
		}
	}()
	t.Cleanup(func() {
		stop.Store(true)
		// Let a writer blocked on its next open go.
		for {
			select {
			case <-done:
				return
			default:
			}
			if f, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = f.Close()
			}
			time.Sleep(time.Millisecond)
		}
	})

	slots := daemon.SlotsOfOnePoll(fifo, filepath.Join(dir, "proc"), settings.TestLocks{Projects: map[string][]string{"alpha": {lock}, "beta": {lock}}})
	alpha, beta := slots("alpha"), slots("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)
	assert.Equal(t, snapshot.Slots{Used: 1, Total: 1}, *alpha, "the poll's reading lists the lock held")
	assert.Equal(t, *alpha, *beta, "both projects count the poll's one reading")
}

package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	run(t, o)
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
			run(t, o)

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
		run(t, o)
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
	run(t, o)
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

// TestSnapshotBossRuntime: the boss's started_at is when this daemon first
// saw its pane run an agent in the boss's place (A31). It keeps that start
// poll after poll; a handoff to another pane starts again at the poll that
// first sees it; workers carry none.
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
	c := &clock{now: t0}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, "")
	o.Now = c.Now
	run(t, o)
	sock := socketPath(t, dir, th.path)

	first := served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t0) })
	require.NotNil(t, first.Boss)
	require.NotNil(t, first.Boss.StartedAt)
	assert.Equal(t, t0, *first.Boss.StartedAt)
	assert.Nil(t, first.Groups[0].Agents[0].StageStartedAt, "a worker has no runtime")

	t1 := t0.Add(3 * time.Second)
	c.advance(3 * time.Second)
	later := served(t, sock, func(s snapshot.Snapshot) bool { return s.At.Equal(t1) })
	require.NotNil(t, later.Boss.StartedAt)
	assert.Equal(t, t0, *later.Boss.StartedAt, "the same boss keeps its start")

	t2 := t0.Add(6 * time.Second)
	c.advance(3 * time.Second)
	th.layout(ws, bossIn("wJ:p2"), agents)
	handed := served(t, sock, func(s snapshot.Snapshot) bool { return s.Boss != nil && s.Boss.PaneID == "wJ:p2" })
	require.NotNil(t, handed.Boss.StartedAt)
	assert.Equal(t, t2, *handed.Boss.StartedAt, "a new boss pane starts at the poll that first sees it")
}

// servedTeam waits until the daemon at sock serves project's snapshot and ok
// holds of it, and returns it.
func servedTeam(t *testing.T, sock, project string, ok func(snapshot.Snapshot) bool) snapshot.Snapshot {
	t.Helper()
	var got snapshot.Snapshot
	require.Eventually(t, func() bool {
		s, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: project})
		if err != nil || !ok(s) {
			return false
		}
		got = s
		return true
	}, ready, 5*time.Millisecond, "team %s was never served as wanted; last: %+v", project, got)
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

// TestEveryTeamCarriesTheSlotsAndItsOwnBoss: with one snapshot per team
// (A24), every team's carries the machine's test slots, the same count, and
// sees a slot freed at the same poll (A31). Each team's boss.started_at is
// its own boss pane's, and each pane's stage clock its own: a boss starting
// in one team, or a coder moving on there, restarts nothing in another.
func TestEveryTeamCarriesTheSlotsAndItsOwnBoss(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(testslots.ProcLocks); err != nil {
		t.Skipf("no %s: %v", testslots.ProcLocks, err)
	}
	locks := t.TempDir()
	gate0, gate1 := filepath.Join(locks, "gate.0.lock"), filepath.Join(locks, "gate.1.lock")
	release := hold(t, gate0)
	th := newTeamHerdr(t)
	everyTeam(th)
	// alpha's boss tab runs no agent yet.
	swapPane(th, "w14:p1", tabPane("w14:p1", "w14", "w14:t1", "", "unknown", nil))
	nameAgent(th, "w14:p1", "")
	c := &clock{now: t0}
	dir := shortDir(t)
	o := teamOpts(t, dir, th, configDir(t, everyTeamConfig+fmt.Sprintf("[tests]\nlocks = [%q, %q]\n", gate0, gate1)))
	o.Now = c.Now
	run(t, o)
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
	spaces := servedTeam(t, sock, "agentisan", at(t0))
	require.NotNil(t, spaces.Boss.StartedAt)
	assert.Equal(t, t0, *spaces.Boss.StartedAt)

	// t1: alpha's boss starts, alpha's coder moves on to review, and the
	// held slot is let go.
	c.advance(3 * time.Second)
	t1 := t0.Add(3 * time.Second)
	swapPane(th, "w14:p1", tabPane("w14:p1", "w14", "w14:t1", "claude", "working", nil))
	nameAgent(th, "w14:p1", "al-boss")
	swapPane(th, "w14:p3", tabPane("w14:p3", "w14", "w14:t3", "claude", "working", map[string]string{"ctx": "50", "item": "NERD-5285", "stage": "review"}))
	release()

	alpha := servedTeam(t, sock, "alpha", func(s snapshot.Snapshot) bool {
		return s.At.Equal(t1) && s.Boss != nil && s.Boss.Status == "working" && s.Groups[0].Agents[0].Stage == "review"
	})
	require.NotNil(t, alpha.Boss.StartedAt)
	assert.Equal(t, t1, *alpha.Boss.StartedAt, "alpha's boss runtime is its own pane's")
	require.NotNil(t, alpha.Groups[0].Agents[0].StageStartedAt)
	assert.Equal(t, t1, *alpha.Groups[0].Agents[0].StageStartedAt, "alpha's coder's stage restarted")
	spaces = servedTeam(t, sock, "agentisan", at(t1))
	require.NotNil(t, spaces.Boss.StartedAt)
	assert.Equal(t, t0, *spaces.Boss.StartedAt, "the spaces' boss keeps its own start")
	require.NotNil(t, spaces.Groups[0].Agents[0].StageStartedAt)
	assert.Equal(t, t0, *spaces.Groups[0].Agents[0].StageStartedAt, "the spaces' coder keeps its stage start")
	// A poll stamped t1 can see both swaps and still count the locks before
	// release(), so each team waits for the freed slot itself.
	for _, p := range teams {
		servedTeam(t, sock, p, func(s snapshot.Snapshot) bool { return at(t1)(s) && slotsAre(0, 2)(s) })
	}
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
// costs that project's slots alone, logged once for it.
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
			run(t, o)
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
	run(t, o)
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

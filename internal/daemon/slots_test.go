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
	require.NoError(t, os.WriteFile(standIn, []byte("1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n2: FLOCK\n"), 0o600))
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

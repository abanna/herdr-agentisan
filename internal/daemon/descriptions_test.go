package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// describedConfig is everyTeamConfig with group descriptions: the ◆ spaces'
// by space name, one for a space herdr does not show, and a shared one for
// every project's coder group.
const describedConfig = everyTeamConfig + `[projects.agentisan.groups.coders]
description = "write fixes + new code · run their own review loop"
[projects.agentisan.groups.research]
description = "read-only digging"
[projects.agentisan.groups.clerk]
description = "keeps Linear in sync"
[groups.coder]
workers = 1
slots = 2
description = "writes code"
`

// describedDaemon runs a daemon on everyTeam at t0 with the shared file
// file, logging to logs, and waits until it serves all three teams; it
// returns the config dir and the daemon's socket.
func describedDaemon(t *testing.T, file string, logs *lockedLog) (*teamHerdr, string, string) {
	t.Helper()
	th := newTeamHerdr(t)
	everyTeam(th)
	dir, cfg := shortDir(t), configDir(t, file)
	o := teamOpts(t, dir, th, cfg)
	o.Now = func() time.Time { return t0 }
	o.Logger = zerolog.New(logs)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)
	require.Eventually(t, func() bool {
		_, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: "beta"})
		return err == nil
	}, ready, 5*time.Millisecond, "the daemon never served the tabs teams")
	return th, cfg, sock
}

// TestSnapshotCarriesGroupDescriptions is A36's snapshot: each team's groups
// carry the descriptions its project configures, matched by the group's
// name, the ◆ spaces' by space name and a tabs team's by worker type; a
// group none names has none, and a description for a group herdr does not
// show adds nothing. Everything else is A24's snapshot, byte for byte.
func TestSnapshotCarriesGroupDescriptions(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	_, _, sock := describedDaemon(t, describedConfig, &logs)

	want := wantTeams()
	describe := func(project string, by map[string]string) {
		s := want[project]
		for i := range s.Groups {
			s.Groups[i].Description = by[s.Groups[i].Name]
		}
	}
	describe("agentisan", map[string]string{"coders": "write fixes + new code · run their own review loop", "research": "read-only digging"})
	describe("alpha", map[string]string{"coder": "writes code"})
	for project, w := range want {
		data, code := snapshotOp(t, sock, fmt.Sprintf(`{"project":%q}`, project))
		require.Empty(t, code, project)
		wantRaw, err := json.Marshal(w)
		require.NoError(t, err)
		assert.Equal(t, string(wantRaw), string(data), "team %s, byte for byte", project)
		got, err := snapshot.Decode(bytes.NewReader(data))
		require.NoError(t, err, "still a strict snapshot v1 document")
		assert.Equal(t, w, got)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "group descriptions read"), "read once while the file stays as it is")
}

// TestGroupDescriptionsFollowTheFile is A36's re-read: a poll that finds the
// shared file changed (its time, size or inode) reads the descriptions
// again, with no daemon restart, and only them; the teams and their colours
// stay as read at start. A file that does not resolve keeps the last good
// descriptions and is logged once, not every poll; one that is gone has
// none; one that comes back is read.
func TestGroupDescriptionsFollowTheFile(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	th, cfg, sock := describedDaemon(t, describedConfig, &logs)
	path := filepath.Join(cfg, settings.FileName)

	coders := func() (string, string) {
		data, code := snapshotOp(t, sock, `{"project":"agentisan"}`)
		require.Empty(t, code)
		s, err := snapshot.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		return s.Groups[0].Description, s.Project.Color
	}
	shows := func(want, msg string) {
		t.Helper()
		require.Eventually(t, func() bool { d, _ := coders(); return d == want }, ready, 5*time.Millisecond, msg)
	}
	polls := func() int {
		n := 0
		for _, r := range th.srv.Requests() {
			if r.Method == "workspace.list" {
				n++
			}
		}
		return n
	}
	// settle waits for five more polls, so a change a poll would act on has
	// been acted on.
	settle := func() {
		t.Helper()
		from := polls()
		require.Eventually(t, func() bool { return polls() >= from+5 }, ready, 5*time.Millisecond, "the poll stopped")
	}
	stamp := time.Unix(1_700_000_000, 0)
	// replace saves content as an editor that renames over the file does: a
	// polling reader never sees it half written.
	replace := func(content string) {
		t.Helper()
		tmp := path + ".tmp"
		require.NoError(t, os.WriteFile(tmp, []byte("schema_version = 1\n"+content), 0o600))
		stamp = stamp.Add(time.Second)
		require.NoError(t, os.Chtimes(tmp, stamp, stamp))
		require.NoError(t, os.Rename(tmp, path))
	}
	withCoders := func(d string) string {
		return strings.Replace(describedConfig, "write fixes + new code · run their own review loop", d, 1)
	}
	shows("write fixes + new code · run their own review loop", "the description read at start")

	// An edit: read at a poll, with no restart. The colour it also changes
	// is read only at start.
	replace(strings.Replace(withCoders("write fixes"), `color = "#7c3aed"`, `color = "#000000"`, 1))
	shows("write fixes", "an edit is read without a restart")
	_, color := coders()
	assert.Equal(t, "#7c3aed", color, "the colour stays as read at start")

	// A file that does not resolve: the last good descriptions stay, and it
	// is logged once however many polls see it.
	replace(withCoders("a\\u001bb"))
	settle()
	settle()
	d, _ := coders()
	assert.Equal(t, "write fixes", d)
	assert.Equal(t, 1, strings.Count(logs.String(), "does not resolve"), "logged once, not every poll")
	assert.NotContains(t, logs.String(), "\x1b", "the log never holds the value")

	// Fixed: read again.
	replace(withCoders("fixed"))
	shows("fixed", "a fixed file is read again")

	// Written in place and cut short inside a value: it does not parse, so
	// the last good ones stay; the rest of the write is then read.
	full := []byte("schema_version = 1\n" + withCoders("written in place"))
	cut := bytes.Index(full, []byte("written in place")) + len("written")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // the test's own temp file
	require.NoError(t, err)
	_, err = f.Write(full[:cut])
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	stamp = stamp.Add(time.Second)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
	settle()
	d, _ = coders()
	assert.Equal(t, "fixed", d, "a half-written file keeps the last ones")
	// The write passed through states a poll may each have seen (truncated,
	// half written, stamped), each logged once; none is logged again.
	logged := strings.Count(logs.String(), "does not resolve")
	assert.Greater(t, logged, 1)
	settle()
	settle()
	assert.Equal(t, logged, strings.Count(logs.String(), "does not resolve"), "a state seen once is not logged again")
	_, err = f.Write(full[cut:])
	require.NoError(t, err)
	require.NoError(t, f.Close())
	stamp = stamp.Add(time.Second)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
	shows("written in place", "the finished write is read")

	// Gone: no group has one, as with no file at start. Back: read.
	require.NoError(t, os.Remove(path))
	shows("", "a file that is gone describes nothing")
	settle()
	assert.Equal(t, 1, strings.Count(logs.String(), "is gone"), "logged once")
	replace(withCoders("back"))
	shows("back", "a file that comes back is read")

	_, err = daemon.SnapshotOf(t.Context(), sock, daemon.Selector{Project: "beta"})
	require.NoError(t, err, "the tabs teams read at start are still served")
}

// TestGroupDescriptionsWithoutAConfigDir: with no config dir the poll reads
// nothing and no group has a description.
func TestGroupDescriptionsWithoutAConfigDir(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	th := newTeamHerdr(t)
	dir := shortDir(t)
	o := teamOpts(t, dir, th, "")
	o.Logger = zerolog.New(&logs)
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)
	served(t, sock, anySnapshot)
	data, code := snapshotOp(t, sock, "")
	require.Empty(t, code)
	s, err := snapshot.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.NotEmpty(t, s.Groups)
	for _, g := range s.Groups {
		assert.Empty(t, g.Description, g.Name)
	}
	assert.NotContains(t, logs.String(), "group descriptions")
}

// TestGroupDescriptionsLookBeforeTheyRead: an edit that lands just after a
// poll read the file is read at the next poll, since the poll looks at the
// file before it reads it, so what it last looked at is never newer than
// what it read (A36). A poll that read first would take the edited file for
// the one it read, and never read the edit.
func TestGroupDescriptionsLookBeforeTheyRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, settings.FileName)
	stamp := time.Unix(1_700_000_000, 0)
	// save saves d as an editor that renames over the file does, stamped at
	// the next second.
	save := func(d string) {
		t.Helper()
		tmp := path + ".tmp"
		require.NoError(t, os.WriteFile(tmp, []byte("schema_version = 1\n[projects.app]\nrepo = \"/x\"\n[projects.app.groups.qa]\ndescription = \""+d+"\"\n"), 0o600))
		stamp = stamp.Add(time.Second)
		require.NoError(t, os.Chtimes(tmp, stamp, stamp))
		require.NoError(t, os.Rename(tmp, path))
	}
	save("one")
	reads := 0
	g := daemon.NewGroupDescriptionsReading(dir, func(dir string) ([]settings.Team, error) {
		teams, err := settings.Teams(dir)
		reads++
		if reads == 1 {
			save("two") // the edit lands after the poll's read
		}
		return teams, err
	})
	qa := func() string { return g.Of("app")["qa"] }

	g.Refresh(zerolog.Nop())
	require.Equal(t, "one", qa(), "the poll shows what it read, from before the edit")
	g.Refresh(zerolog.Nop())
	assert.Equal(t, "two", qa(), "the next poll reads the edit")
	g.Refresh(zerolog.Nop())
	assert.Equal(t, "two", qa())
	assert.Equal(t, 2, reads, "the file is read again once, for the edit")
}

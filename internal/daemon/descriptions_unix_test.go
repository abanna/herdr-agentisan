//go:build unix

package daemon_test

import (
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
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestGroupDescriptionsRefreshClasses walks what one poll can find at the
// shared file, one poll at a time (A36): nothing; a regular file, read once
// until it changes; a rename that keeps its time and size, seen by the file;
// a change that keeps time, size and file, not seen until the next; a change
// of size alone, seen, as is one written through a hard link; a directory, an empty file, a FIFO and an unreadable
// file, each keeping the last good descriptions, logged once and read again
// after the next change; a parent that cannot be searched, likewise, and
// logged again once the file was looked at in between, found or gone; a
// symlink, followed, and retargeted; a dangling one, which is no file.
func TestGroupDescriptionsRefreshClasses(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, settings.FileName)
	var logs lockedLog
	log := zerolog.New(&logs)
	g := daemon.NewGroupDescriptions(dir)

	qa := func() string { return g.Of("app")["qa"] }
	count := func(msg string) int { return strings.Count(logs.String(), msg) }
	stamp := time.Unix(1_700_000_000, 0)
	config := func(d string) []byte {
		return []byte("schema_version = 1\n[projects.app]\nrepo = \"/x\"\n[projects.app.groups.qa]\ndescription = \"" + d + "\"\n")
	}
	// write puts content at name, stamped at the next second, or at the
	// same one when keep is set.
	write := func(name string, content []byte, keep bool) {
		t.Helper()
		require.NoError(t, os.WriteFile(name, content, 0o600))
		if !keep {
			stamp = stamp.Add(time.Second)
		}
		require.NoError(t, os.Chtimes(name, stamp, stamp))
	}
	poll := func(want, msg string) {
		t.Helper()
		g.Refresh(log)
		assert.Equal(t, want, qa(), msg)
	}

	poll("", "no file at start: none")
	assert.Empty(t, logs.String(), "nothing to log")

	write(path, config("one"), false)
	poll("one", "a regular file is read")
	poll("one", "an unchanged file")
	poll("one", "an unchanged file")
	assert.Equal(t, 1, count("group descriptions read"), "read once while it stays as it is")

	tmp := filepath.Join(dir, "next.toml")
	write(tmp, config("two"), true)
	require.NoError(t, os.Rename(tmp, path))
	poll("two", "a rename with the same time and size is another file")

	write(path, config("six"), true)
	poll("two", "a change that keeps time, size and file is not seen")
	write(path, config("six"), false)
	poll("six", "until the next change")

	before, err := os.Stat(path)
	require.NoError(t, err)
	write(path, config("sixty"), true)
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the same file")
	require.True(t, before.ModTime().Equal(after.ModTime()), "the same time")
	require.NotEqual(t, before.Size(), after.Size(), "another size")
	poll("sixty", "a change of size alone is seen")
	alias := filepath.Join(dir, "alias.toml")
	require.NoError(t, os.Link(path, alias))
	write(alias, config("six"), true)
	poll("six", "and back, written through a hard link to the file")
	require.NoError(t, os.Remove(alias))
	reads := count("group descriptions read")

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	poll("six", "a directory at the path keeps the last ones")
	poll("six", "a directory at the path keeps the last ones")
	assert.Equal(t, 1, count("does not resolve"), "logged once")

	require.NoError(t, os.Remove(path))
	write(path, nil, false)
	poll("six", "an empty file keeps the last ones")
	assert.Equal(t, 2, count("does not resolve"))

	require.NoError(t, os.Remove(path))
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	poll("six", "a FIFO is refused unread, and the poll does not wait on it")
	assert.Equal(t, 3, count("does not resolve"))

	require.NoError(t, os.Remove(path))
	write(path, config("seven"), false)
	require.NoError(t, os.Chmod(path, 0o000))
	poll("six", "an unreadable file keeps the last ones")
	poll("six", "an unreadable file keeps the last ones")
	assert.Equal(t, 4, count("does not resolve"), "logged once")
	require.NoError(t, os.Chmod(path, 0o600))

	require.NoError(t, os.Chmod(dir, 0o000))
	poll("six", "a parent that cannot be searched keeps the last ones")
	poll("six", "a parent that cannot be searched keeps the last ones")
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // the test's own temp dir, searchable again
	assert.Equal(t, 1, count("cannot be looked at"), "logged once")
	poll("six", "the file it finds again is the one it could not read: read after its next change")
	stamp = stamp.Add(time.Second)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
	poll("seven", "read once it changes")
	assert.Equal(t, 1, count("cannot be looked at"), "not logged again")
	assert.Equal(t, reads+1, count("group descriptions read"))
	require.NoError(t, os.Chmod(dir, 0o000))
	poll("seven", "a parent that cannot be searched again keeps the last ones")
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // the test's own temp dir, searchable again
	assert.Equal(t, 2, count("cannot be looked at"), "logged again: the file was found in between")

	target := filepath.Join(dir, "real.toml")
	write(target, config("eight"), false)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(target, path))
	poll("eight", "a symlink is followed")

	other := filepath.Join(dir, "other.toml")
	write(other, config("nine"), true)
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(other, link))
	require.NoError(t, os.Rename(link, path))
	poll("nine", "a retargeted symlink is another file")

	require.NoError(t, os.Remove(other))
	poll("", "a dangling symlink is no file: none")
	poll("", "still none")
	assert.Equal(t, 1, count("is gone"), "logged once")
	require.NoError(t, os.Chmod(dir, 0o000))
	poll("", "a parent that cannot be searched, with no file")
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // the test's own temp dir, searchable again
	poll("", "the file is still gone")
	require.NoError(t, os.Chmod(dir, 0o000))
	poll("", "and the parent cannot be searched again")
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // the test's own temp dir, searchable again
	assert.Equal(t, 4, count("cannot be looked at"), "logged again: the file was found gone in between")
	assert.Equal(t, 1, count("is gone"), "still logged once")

	write(other, config("ten"), false)
	poll("ten", "a file that comes back is read")
	assert.NotContains(t, logs.String(), "\x1b")

	none := daemon.NewGroupDescriptions("")
	none.Refresh(log)
	assert.Nil(t, none.Of("app"), "no config dir reads nothing")
}

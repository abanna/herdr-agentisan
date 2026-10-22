//go:build unix

package settings_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestRotationsFilesystemClasses walks the shapes a project's repo and its
// repo file can take on disk under Rotations, as TestLoadFilesystemClasses
// does under Load: a symlink is followed, a dangling one reads as missing,
// and anything Load refuses leaves the repo file unread, without blocking,
// and the project its own table's thresholds.
func TestRotationsFilesystemClasses(t *testing.T) {
	t.Parallel()
	const own = "schema_version = 1\n[rotation]\nthresholds = [20, 30, 40]\n"
	tests := map[string]struct {
		setup func(t *testing.T, repo string) (repoPath string)
		// read is whether the repo file's thresholds apply.
		read    bool
		nonRoot bool
		wantIn  string
	}{
		"a regular file": {setup: func(t *testing.T, repo string) string {
			writeFile(t, repo, settings.RepoFileName, own, "")
			return repo
		}, read: true},
		"a symlink to one": {setup: func(t *testing.T, repo string) string {
			target := filepath.Join(t.TempDir(), "overlay.toml")
			require.NoError(t, os.WriteFile(target, []byte(own), 0o600))
			require.NoError(t, os.Symlink(target, filepath.Join(repo, settings.RepoFileName)))
			return repo
		}, read: true},
		"a dangling symlink, read as missing": {setup: func(t *testing.T, repo string) string {
			require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone.toml"), filepath.Join(repo, settings.RepoFileName)))
			return repo
		}},
		"a directory": {setup: func(t *testing.T, repo string) string {
			require.NoError(t, os.Mkdir(filepath.Join(repo, settings.RepoFileName), 0o700))
			return repo
		}, wantIn: "not a regular file"},
		"a FIFO": {setup: func(t *testing.T, repo string) string {
			require.NoError(t, syscall.Mkfifo(filepath.Join(repo, settings.RepoFileName), 0o600))
			return repo
		}, wantIn: "not a regular file"},
		"unreadable": {setup: func(t *testing.T, repo string) string {
			writeFile(t, repo, settings.RepoFileName, own, "")
			chmod(t, filepath.Join(repo, settings.RepoFileName), 0o000)
			return repo
		}, nonRoot: true, wantIn: "permission denied"},
		"one byte past MaxFileBytes": {setup: func(t *testing.T, repo string) string {
			writeFile(t, repo, settings.RepoFileName, "#"+strings.Repeat("x", settings.MaxFileBytes), "")
			return repo
		}, wantIn: "larger than"},
		"a repo that is a symlink to a directory": {setup: func(t *testing.T, repo string) string {
			writeFile(t, repo, settings.RepoFileName, own, "")
			link := filepath.Join(t.TempDir(), "link")
			require.NoError(t, os.Symlink(repo, link))
			return link
		}, read: true},
		"a repo path with non-ASCII and spaces": {setup: func(t *testing.T, repo string) string {
			inner := filepath.Join(repo, "rëpo dir")
			require.NoError(t, os.Mkdir(inner, 0o700))
			writeFile(t, inner, settings.RepoFileName, own, "")
			return inner
		}, read: true},
		"a repo that is a FIFO": {setup: func(t *testing.T, repo string) string {
			p := filepath.Join(repo, "fifo")
			require.NoError(t, syscall.Mkfifo(p, 0o600))
			return p
		}, wantIn: "not a directory"},
		"a repo whose parent cannot be searched": {setup: func(t *testing.T, repo string) string {
			inner := filepath.Join(repo, "inner")
			require.NoError(t, os.Mkdir(inner, 0o700))
			writeFile(t, inner, settings.RepoFileName, own, "")
			chmod(t, repo, 0o000)
			return inner
		}, nonRoot: true, wantIn: "permission denied"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tc.nonRoot && os.Geteuid() == 0 {
				t.Skip("root reads past permissions")
			}
			repo, dir := t.TempDir(), t.TempDir()
			path := tc.setup(t, repo)
			writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n[projects.app.rotation]\nthresholds = [50, 60, 70]\n", path)

			done := make(chan settings.RotationSettings, 1)
			go func() {
				got, err := settings.Rotations(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
				assert.NoError(t, err)
				done <- got
			}()
			var got settings.RotationSettings
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Rotations blocked on a special file")
			}
			if tc.read {
				assert.Equal(t, []int{20, 30, 40}, got.For("app").For(""))
				assert.Empty(t, got.Unread)
				return
			}
			assert.Equal(t, []int{50, 60, 70}, got.For("app").For(""), "the project's own table")
			if tc.wantIn == "" {
				assert.Empty(t, got.Unread)
				return
			}
			require.Contains(t, got.Unread, "app")
			assert.Contains(t, got.Unread["app"].Error(), tc.wantIn)
		})
	}
}

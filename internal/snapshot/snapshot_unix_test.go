//go:build unix

package snapshot_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

const doc = `{"v":1,"team":"read","groups":[]}`

// TestFileSourceReads: every spelling of a path that names a regular file
// holding a snapshot is read as that file.
func TestFileSourceReads(t *testing.T) {
	t.Parallel()

	file := func(name string) func(t *testing.T, dir string) string {
		return func(t *testing.T, dir string) string {
			t.Helper()
			path := filepath.Join(dir, name)
			writeFile(t, path, doc)
			return path
		}
	}
	tests := map[string]func(t *testing.T, dir string) string{
		"a regular file": file("snap.json"),
		"a symlink to the file": func(t *testing.T, dir string) string {
			t.Helper()
			target := file("target.json")(t, dir)
			link := filepath.Join(dir, "link.json")
			require.NoError(t, os.Symlink(target, link))
			return link
		},
		"a hard link to the file": func(t *testing.T, dir string) string {
			t.Helper()
			target := file("target.json")(t, dir)
			link := filepath.Join(dir, "hard.json")
			require.NoError(t, os.Link(target, link))
			return link
		},
		"a non-ASCII name":                  file("snäp-日本.json"),
		"a non-UTF-8 name":                  file("snap-\xff\xfe.json"),
		"a truncated multibyte name":        file("snap-\xe2\x82.json"),
		"a decomposed (NFD) name":           file("café.json"),
		"a name with spaces, quotes and $":  file(`my "snap" $HOME;|&.json`),
		"a name with a newline and a tab":   file("snap\n\t.json"),
		"a dash-leading name":               file("--help.json"),
		"a name with a byte-order mark":     file("\xef\xbb\xbfsnap.json"),
		"a name of NAME_MAX bytes":          file(strings.Repeat("n", 250) + ".json"),
		"a secret-looking name":             file(".env"),
		"a platform-reserved name on POSIX": file("CON"),
		"a relative path with ..": func(t *testing.T, dir string) string {
			t.Helper()
			path := file("rel.json")(t, dir)
			wd, err := os.Getwd()
			require.NoError(t, err)
			rel, err := filepath.Rel(wd, path)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(rel, ".."), "%s is not a .. path", rel)
			return rel
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := setup(t, t.TempDir())

			got, err := snapshot.FileSource{Path: path}.Snapshot(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "read", got.Team)
		})
	}
}

// TestFileSourceRefusesWhatIsNotARegularFile: a node that is not a regular
// file is refused as ErrUnavailable without being read, and without
// blocking — a FIFO with no writer would otherwise hang the poll forever.
func TestFileSourceRefusesWhatIsNotARegularFile(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		setup func(t *testing.T, dir string) string
		want  error
	}{
		"a directory": {setup: func(_ *testing.T, dir string) string { return dir }, want: snapshot.ErrUnavailable},
		"a FIFO with no writer": {
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				path := filepath.Join(dir, "fifo")
				require.NoError(t, syscall.Mkfifo(path, 0o600))
				return path
			},
			want: snapshot.ErrUnavailable,
		},
		"a unix socket": {
			setup: func(t *testing.T, _ string) string {
				t.Helper()
				// os.MkdirTemp, not t.TempDir: socket paths are capped at
				// about 104 bytes and t.TempDir embeds the test name.
				dir, err := os.MkdirTemp("", "fs")
				require.NoError(t, err)
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				path := filepath.Join(dir, "s")
				ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
				require.NoError(t, err)
				t.Cleanup(func() { _ = ln.Close() })
				return path
			},
			want: snapshot.ErrUnavailable,
		},
		"a character device":   {setup: func(*testing.T, string) string { return os.DevNull }, want: snapshot.ErrUnavailable},
		"a dangling symlink":   {setup: symlinkTo("nowhere.json"), want: snapshot.ErrUnavailable},
		"a symlink to a dir":   {setup: symlinkTo("."), want: snapshot.ErrUnavailable},
		"an empty path":        {setup: func(*testing.T, string) string { return "" }, want: snapshot.ErrUnavailable},
		"a NUL in the path":    {setup: func(_ *testing.T, dir string) string { return filepath.Join(dir, "a\x00b.json") }, want: snapshot.ErrUnavailable},
		"a name past NAME_MAX": {setup: func(_ *testing.T, dir string) string { return filepath.Join(dir, strings.Repeat("n", 256)) }, want: snapshot.ErrUnavailable},
		"a file that is not readable": {
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				skipAsRoot(t)
				path := filepath.Join(dir, "locked.json")
				require.NoError(t, os.WriteFile(path, []byte(doc), 0o000))
				return path
			},
			want: snapshot.ErrUnavailable,
		},
		"a parent that cannot be searched": {
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				skipAsRoot(t)
				sub := filepath.Join(dir, "sub")
				require.NoError(t, os.Mkdir(sub, 0o700))
				path := filepath.Join(sub, "snap.json")
				writeFile(t, path, doc)
				require.NoError(t, os.Chmod(sub, 0o000))
				t.Cleanup(func() { _ = os.Chmod(sub, 0o700) }) //nolint:gosec // restoring the test's own directory
				return path
			},
			want: snapshot.ErrUnavailable,
		},
		"an empty file": {
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				path := filepath.Join(dir, "empty.json")
				writeFile(t, path, "")
				return path
			},
			want: snapshot.ErrInvalid,
		},
		"a file being rewritten (half a document)": {
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				path := filepath.Join(dir, "half.json")
				writeFile(t, path, doc[:len(doc)/2])
				return path
			},
			want: snapshot.ErrInvalid,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := tc.setup(t, t.TempDir())

			done := make(chan error, 1)
			go func() {
				_, err := snapshot.FileSource{Path: path}.Snapshot(context.Background())
				done <- err
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, tc.want)
			case <-time.After(5 * time.Second):
				t.Fatal("reading the source blocked")
			}
		})
	}
}

func symlinkTo(target string) func(t *testing.T, dir string) string {
	return func(t *testing.T, dir string) string {
		t.Helper()
		link := filepath.Join(dir, "link")
		require.NoError(t, os.Symlink(target, link))
		return link
	}
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
}

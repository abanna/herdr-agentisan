package daemon_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
)

// TestLogWriterCap: daemon.log never grows past its cap. The write that would
// cross it first moves the file to daemon.log.1, replacing any older one, so
// at most two files exist: about twice the cap on disk.
func TestLogWriterCap(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		existing int // bytes already in daemon.log
		rotated  bool
		writes   []int
		wantLog  int
		wantOld  int // bytes in daemon.log.1; -1 means absent
	}{
		"under the cap":                    {writes: []int{40, 50}, wantLog: 90, wantOld: -1},
		"exactly the cap fits":             {writes: []int{60, 40}, wantLog: 100, wantOld: -1},
		"one byte past the cap rotates":    {writes: []int{60, 41}, wantLog: 41, wantOld: 60},
		"rotation replaces daemon.log.1":   {rotated: true, writes: []int{90, 20}, wantLog: 20, wantOld: 90},
		"an existing log counts toward it": {existing: 95, writes: []int{10}, wantLog: 10, wantOld: 95},
		"one write larger than the cap":    {writes: []int{10, 150}, wantLog: 150, wantOld: 10},
		"an empty write":                   {writes: []int{0}, wantLog: 0, wantOld: -1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.log")
			if tc.existing > 0 {
				require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("e", tc.existing)), 0o600))
			}
			if tc.rotated {
				require.NoError(t, os.WriteFile(path+".1", []byte("older"), 0o600))
			}
			w, err := daemon.NewLogWriter(path, 100)
			require.NoError(t, err)
			t.Cleanup(func() { _ = w.Close() })

			for _, n := range tc.writes {
				got, err := w.Write([]byte(strings.Repeat("x", n)))
				require.NoError(t, err)
				assert.Equal(t, n, got)
			}

			st, err := os.Stat(path)
			require.NoError(t, err)
			assert.EqualValues(t, tc.wantLog, st.Size())
			assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
			old, err := os.Stat(path + ".1")
			if tc.wantOld < 0 {
				assert.True(t, os.IsNotExist(err), "no rotation expected")
				return
			}
			require.NoError(t, err)
			assert.EqualValues(t, tc.wantOld, old.Size())
		})
	}
}

// TestLogWriterConcurrentWrites: the daemon logs from several goroutines.
// Every line arrives whole, and with room for exactly one rotation none is
// lost.
func TestLogWriterConcurrentWrites(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.log")
	w, err := daemon.NewLogWriter(path, 3000)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	const n, line = 200, "0123456789abcdefghi\n" // 20 bytes, 4000 in all
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_, err := w.Write([]byte(line))
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	require.NoError(t, w.Close())

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	old, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	assert.Equal(t, n, strings.Count(string(cur), line)+strings.Count(string(old), line), "every line arrives whole")
	assert.Equal(t, n*len(line), len(cur)+len(old), "nothing else is written")
	assert.LessOrEqual(t, len(cur), 3000)
	assert.LessOrEqual(t, len(old), 3000)
}

func TestLogWriterOpenFailures(t *testing.T) {
	t.Parallel()

	tests := map[string]func(t *testing.T) string{
		"directory missing": func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent", "daemon.log") },
		"path is a directory": func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "d")
			require.NoError(t, os.Mkdir(p, 0o700))
			return p
		},
		"directory not writable": func(t *testing.T) string {
			d := t.TempDir()
			require.NoError(t, os.Chmod(d, 0o500))
			t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
			return filepath.Join(d, "daemon.log")
		},
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if name == "directory not writable" && os.Geteuid() == 0 {
				t.Skip("root writes anywhere")
			}
			_, err := daemon.NewLogWriter(path(t), 100)
			require.ErrorIs(t, err, daemon.ErrLog)
		})
	}
}

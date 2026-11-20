//go:build unix

package devctl_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

func TestMergeCoverageProfilesRefusesAProfileItCannotRead(t *testing.T) {
	t.Parallel()

	body := "mode: atomic\na.go:1.1,2.2 2 1\n"
	t.Run("a FIFO, which would block the merge", func(t *testing.T) {
		t.Parallel()

		paths, out := writeProfiles(t, body)
		fifo := filepath.Join(t.TempDir(), "fifo.out")
		require.NoError(t, syscall.Mkfifo(fifo, 0o600))
		_, err := devctl.MergeCoverageProfiles(out, append(paths, fifo))
		require.Error(t, err)
		assert.ErrorContains(t, err, "not a regular file")
		assert.NoFileExists(t, out)
	})
	t.Run("a profile no one may read", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores file permission bits")
		}

		paths, out := writeProfiles(t, body)
		require.NoError(t, os.Chmod(paths[0], 0))
		_, err := devctl.MergeCoverageProfiles(out, paths)
		require.Error(t, err)
		assert.NoFileExists(t, out)
	})
}

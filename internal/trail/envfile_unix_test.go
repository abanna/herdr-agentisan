//go:build unix

package trail_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/trail"
)

// TestReadEnvFileFromAPipe: 1Password mounts the env file as a named pipe
// it writes only while unlocked. A pipe that is answered reads like a file;
// one never answered fails within the timeout, saying why, instead of
// hanging the debugger's query.
func TestReadEnvFileFromAPipe(t *testing.T) {
	t.Parallel()
	pipe := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "local.env")
		require.NoError(t, syscall.Mkfifo(path, 0o600))
		return path
	}

	t.Run("answered", func(t *testing.T) {
		t.Parallel()
		path := pipe(t)
		go func() {
			f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // the test's own pipe
			if err != nil {
				return
			}
			_, _ = f.WriteString("OTHER=x\nELASTIC_URL=http://es:9200\n" + keyEq + probeKey + "\n")
			_ = f.Close()
		}()
		got, err := trail.ReadEnvFile(t.Context(), path, both, 5*time.Second)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{trail.URLVar: "http://es:9200", trail.KeyVar: probeKey}, got)
	})

	t.Run("never answered", func(t *testing.T) {
		t.Parallel()
		path := pipe(t)
		start := time.Now()
		_, err := trail.ReadEnvFile(t.Context(), path, both, 100*time.Millisecond)
		require.ErrorIs(t, err, trail.ErrEnvFile)
		assert.Contains(t, err.Error(), "did not answer within 100ms")
		assert.Contains(t, err.Error(), "1Password")
		assert.Less(t, time.Since(start), 5*time.Second)
		// Release the reader left waiting in open.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { //nolint:gosec // the test's own pipe
			_ = f.Close()
		}
	})
}

//go:build unix

package config_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/config"
)

// TestLoadDotenvFileClasses covers the shapes ./.env can take beyond a plain
// file. A .env is a developer convenience, so every class either loads like a
// regular file or is skipped — none may hang startup or make Load fail.
// Not parallel: Load reads the working directory and process environment.
func TestLoadDotenvFileClasses(t *testing.T) {
	tests := map[string]struct {
		setup     func(t *testing.T, dir string)
		wantLevel string
	}{
		"symlink to a regular file is followed": {
			setup: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "real.env"), []byte("HERDR_AGENTISAN_LOG_LEVEL=debug\n"), 0o600))
				require.NoError(t, os.Symlink("real.env", filepath.Join(dir, ".env")))
			},
			wantLevel: "debug",
		},
		"unreadable file is skipped": {
			setup: func(t *testing.T, dir string) {
				if os.Geteuid() == 0 {
					t.Skip("root ignores file permission bits")
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("HERDR_AGENTISAN_LOG_LEVEL=debug\n"), 0o000))
			},
			wantLevel: "info",
		},
		"FIFO is skipped rather than blocking": {
			setup: func(t *testing.T, dir string) {
				require.NoError(t, syscall.Mkfifo(filepath.Join(dir, ".env"), 0o600))
			},
			wantLevel: "info",
		},
		"dangling symlink is skipped": {
			setup: func(t *testing.T, dir string) {
				require.NoError(t, os.Symlink("missing.env", filepath.Join(dir, ".env")))
			},
			wantLevel: "info",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HERDR_AGENTISAN_LOG_LEVEL", "")
			require.NoError(t, os.Unsetenv("HERDR_AGENTISAN_LOG_LEVEL"))
			dir := t.TempDir()
			tc.setup(t, dir)
			t.Chdir(dir)

			done := make(chan struct{})
			var (
				cfg config.Config
				err error
			)
			go func() {
				defer close(done)
				cfg, err = config.Load()
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Load blocked on .env")
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantLevel, cfg.LogLevel)
		})
	}
}

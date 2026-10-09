//go:build unix

package devcli_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/devcli"
)

// TestStaleExemptionsRootClasses walks the path and content classes the root
// argument can take. StaleExemptions only READS under root, through os.Root,
// so every class either resolves to the two files it needs or fails with an
// error — it must never hang, read outside root, or report a clean result
// from a tree it could not read.
func TestStaleExemptionsRootClasses(t *testing.T) {
	t.Parallel()

	const goodTaskfile = "version: \"3\"\ntasks:\n  vet:\n    cmds:\n      - go vet ./...\n"
	const goodAgents = "| Task | Command |\n|------|---------|\n| Lint | `go vet ./...` |\n"

	tests := map[string]struct {
		setup   func(t *testing.T, root string) string // returns the root to pass
		wantErr string
		check   func(t *testing.T, stale []string)
	}{
		"missing root": {
			setup:   func(_ *testing.T, root string) string { return filepath.Join(root, "absent") },
			wantErr: "open repository root",
		},
		"empty root": {
			setup:   func(_ *testing.T, _ string) string { return "" },
			wantErr: "open repository root",
		},
		"root is not searchable": {
			setup: func(t *testing.T, root string) string {
				if os.Geteuid() == 0 {
					t.Skip("root ignores directory permission bits")
				}
				locked := filepath.Join(root, "locked")
				require.NoError(t, os.Mkdir(locked, 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(locked, "AGENTS.md"), []byte(goodAgents), 0o600))
				require.NoError(t, os.Chmod(locked, 0o000))
				t.Cleanup(func() { _ = os.Chmod(locked, 0o750) }) //nolint:gosec // restore so TempDir can clean up
				return locked
			},
			wantErr: "open repository root",
		},
		"byte-order marks before both files": {
			setup: func(t *testing.T, root string) string {
				bom := "\xef\xbb\xbf"
				require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(bom+goodAgents), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "Taskfile.yml"), []byte(bom+goodTaskfile), 0o600))
				return root
			},
			check: func(t *testing.T, stale []string) {
				t.Helper()
				assert.Contains(t, stale, `nonGateTasks["build:cli"]: Taskfile.yml has no such task`)
				assert.NotContains(t, stale, `nonGateTasks["vet"]: Taskfile.yml has no such task`)
			},
		},
		"root is a regular file": {
			setup: func(t *testing.T, root string) string {
				p := filepath.Join(root, "file")
				require.NoError(t, os.WriteFile(p, nil, 0o600))
				return p
			},
			wantErr: "open repository root",
		},
		"AGENTS.md is a directory": {
			setup: func(t *testing.T, root string) string {
				require.NoError(t, os.Mkdir(filepath.Join(root, "AGENTS.md"), 0o750))
				return root
			},
			wantErr: "not a regular file",
		},
		"AGENTS.md is a FIFO": {
			// Opening a FIFO for reading blocks until a writer appears; the
			// check must refuse it before open rather than hang devctl.
			setup: func(t *testing.T, root string) string {
				require.NoError(t, syscall.Mkfifo(filepath.Join(root, "AGENTS.md"), 0o600))
				return root
			},
			wantErr: "not a regular file",
		},
		"AGENTS.md symlinks outside root": {
			setup: func(t *testing.T, root string) string {
				outside := filepath.Join(t.TempDir(), "AGENTS.md")
				require.NoError(t, os.WriteFile(outside, []byte(goodAgents), 0o600))
				require.NoError(t, os.Symlink(outside, filepath.Join(root, "AGENTS.md")))
				return root
			},
			wantErr: "AGENTS.md",
		},
		"AGENTS.md is a dangling symlink": {
			setup: func(t *testing.T, root string) string {
				require.NoError(t, os.Symlink("nowhere.md", filepath.Join(root, "AGENTS.md")))
				return root
			},
			wantErr: "AGENTS.md",
		},
		"AGENTS.md is unreadable": {
			setup: func(t *testing.T, root string) string {
				if os.Geteuid() == 0 {
					t.Skip("root ignores file permission bits")
				}
				p := filepath.Join(root, "AGENTS.md")
				require.NoError(t, os.WriteFile(p, []byte(goodAgents), 0o000))
				return root
			},
			wantErr: "AGENTS.md",
		},
		"in-root symlink and CRLF table": {
			setup: func(t *testing.T, root string) string {
				crlf := strings.ReplaceAll(goodAgents, "\n", "\r\n")
				require.NoError(t, os.WriteFile(filepath.Join(root, "real.md"), []byte(crlf), 0o600))
				require.NoError(t, os.Symlink("real.md", filepath.Join(root, "AGENTS.md")))
				require.NoError(t, os.WriteFile(filepath.Join(root, "Taskfile.yml"), []byte(goodTaskfile), 0o600))
				return root
			},
			check: func(t *testing.T, stale []string) {
				t.Helper()
				assert.Contains(t, stale, `nonGateTasks["build:cli"]: Taskfile.yml has no such task`)
			},
		},
		"Taskfile with no tasks": {
			setup: func(t *testing.T, root string) string {
				require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(goodAgents), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "Taskfile.yml"), []byte("version: \"3\"\n"), 0o600))
				return root
			},
			check: func(t *testing.T, stale []string) {
				t.Helper()
				assert.Contains(t, stale, `nonGateTasks["default"]: Taskfile.yml has no such task`)
			},
		},
		"Taskfile is not YAML": {
			setup: func(t *testing.T, root string) string {
				require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(goodAgents), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "Taskfile.yml"), []byte("tasks: [unclosed\n"), 0o600))
				return root
			},
			wantErr: "parse Taskfile.yml",
		},
		"AGENTS.md has invalid UTF-8 and a NUL": {
			setup: func(t *testing.T, root string) string {
				bad := "\xff\xfe\x00" + goodAgents
				require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(bad), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "Taskfile.yml"), []byte(goodTaskfile), 0o600))
				return root
			},
			check: func(t *testing.T, stale []string) {
				t.Helper()
				assert.Contains(t, stale, `localOnly["task run:cli"]: AGENTS.md does not document this command`)
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := tc.setup(t, t.TempDir())
			done := make(chan struct{})
			var (
				stale []string
				err   error
			)
			go func() {
				defer close(done)
				stale, err = devcli.StaleExemptions(root)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("StaleExemptions blocked instead of refusing the input")
			}

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, stale)
		})
	}
}

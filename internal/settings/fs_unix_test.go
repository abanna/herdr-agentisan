//go:build unix

package settings_test

import (
	"io/fs"
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

// fsFixture is a config dir whose shared file names project app, and app's
// repo with the Agentisan bundle installed. Rows rearrange it.
type fsFixture struct {
	cfg   string
	repo  string
	fifos []string
}

const fsShared = "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n"

// bundleAt installs the Agentisan bundle marker in repo.
func bundleAt(t *testing.T, repo string) {
	t.Helper()
	marker := filepath.Join(repo, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
}

// replace swaps path for whatever make creates there.
func replace(t *testing.T, path string, make func(string) error) {
	t.Helper()
	require.NoError(t, os.RemoveAll(path))
	require.NoError(t, make(path))
}

func (f *fsFixture) fifo(t *testing.T, path string) {
	t.Helper()
	replace(t, path, func(p string) error { return syscall.Mkfifo(p, 0o600) })
	f.fifos = append(f.fifos, path)
}

// chmod sets mode on path and restores it before the temp dir is removed.
func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
}

// loadWithin runs Load and fails, rather than hangs, if it blocks: opening a
// FIFO for reading waits for a writer forever.
func loadWithin(t *testing.T, opts settings.LoadOptions, fifos []string) (settings.Resolved, error) {
	t.Helper()
	type result struct {
		r   settings.Resolved
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := settings.Load(opts)
		done <- result{r, err}
	}()
	select {
	case res := <-done:
		return res.r, res.err
	case <-time.After(5 * time.Second):
		for _, p := range fifos {
			// Opening the write end releases the blocked reader.
			if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
		}
		t.Fatal("Load blocked on a special file")
		return settings.Resolved{}, nil
	}
}

// TestLoadFilesystemClasses walks the shapes the config dir, the shared
// file, a repo file, the repo and the bundle marker can take on disk. Load
// only ever reads: a special file is refused without blocking, a file past
// MaxFileBytes is refused, a symlink is followed, and a dangling one reads
// as missing. Not parallel: one row changes the working directory.
func TestLoadFilesystemClasses(t *testing.T) {
	tests := map[string]struct {
		setup   func(t *testing.T, f *fsFixture)
		project string // "app" unless set; "-" selects none
		nonRoot bool   // permission rows mean nothing to root
		wantErr error
		check   func(t *testing.T, r settings.Resolved)
	}{
		// The config dir.
		"a config dir that does not exist resolves the defaults": {
			setup:   func(t *testing.T, f *fsFixture) { f.cfg = filepath.Join(t.TempDir(), "missing") },
			project: "-",
			check:   func(t *testing.T, r settings.Resolved) { assert.Empty(t, r.Projects) },
		},
		"a config dir that is a file": {
			setup: func(t *testing.T, f *fsFixture) {
				f.cfg = filepath.Join(f.cfg, settings.FileName)
			},
			project: "-", wantErr: syscall.ENOTDIR,
		},
		"a config dir that cannot be searched": {
			setup:   func(t *testing.T, f *fsFixture) { chmod(t, f.cfg, 0o000) },
			project: "-", nonRoot: true, wantErr: fs.ErrPermission,
		},
		"a config dir that is a symlink": {
			setup: func(t *testing.T, f *fsFixture) {
				link := filepath.Join(t.TempDir(), "link")
				require.NoError(t, os.Symlink(f.cfg, link))
				f.cfg = link
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, "app", r.Project) },
		},
		"a config dir that is a dangling symlink resolves the defaults": {
			setup: func(t *testing.T, f *fsFixture) {
				link := filepath.Join(t.TempDir(), "link")
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), link))
				f.cfg = link
			},
			project: "-",
			check:   func(t *testing.T, r settings.Resolved) { assert.Empty(t, r.Projects) },
		},
		"a config dir that is a FIFO": {
			setup: func(t *testing.T, f *fsFixture) {
				p := filepath.Join(t.TempDir(), "cfg")
				f.fifo(t, p)
				f.cfg = p
			},
			project: "-", wantErr: syscall.ENOTDIR,
		},
		"a config dir whose parent cannot be searched": {
			setup: func(t *testing.T, f *fsFixture) {
				parent := filepath.Join(t.TempDir(), "parent")
				require.NoError(t, os.MkdirAll(filepath.Join(parent, "cfg"), 0o700))
				writeFile(t, filepath.Join(parent, "cfg"), settings.FileName, fsShared, f.repo)
				f.cfg = filepath.Join(parent, "cfg")
				chmod(t, parent, 0o000)
			},
			nonRoot: true, wantErr: fs.ErrPermission,
		},
		"a non-ASCII config dir with spaces": {
			setup: func(t *testing.T, f *fsFixture) {
				dir := filepath.Join(t.TempDir(), "réglages de herdr")
				require.NoError(t, os.Mkdir(dir, 0o700))
				writeFile(t, dir, settings.FileName, fsShared, f.repo)
				f.cfg = dir
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, "app", r.Project) },
		},
		"a config dir name that is not UTF-8": {
			setup: func(t *testing.T, f *fsFixture) {
				dir := filepath.Join(t.TempDir(), "cfg\xff")
				require.NoError(t, os.Mkdir(dir, 0o700))
				writeFile(t, dir, settings.FileName, fsShared, f.repo)
				f.cfg = dir
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, "app", r.Project) },
		},
		"a relative config dir resolves against the working directory": {
			setup: func(t *testing.T, f *fsFixture) {
				t.Chdir(filepath.Dir(f.cfg))
				f.cfg = filepath.Base(f.cfg)
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, "app", r.Project) },
		},
		// The shared file.
		"the shared file is a symlink": {
			setup: func(t *testing.T, f *fsFixture) {
				target := filepath.Join(t.TempDir(), "real.toml")
				require.NoError(t, os.Rename(filepath.Join(f.cfg, settings.FileName), target))
				require.NoError(t, os.Symlink(target, filepath.Join(f.cfg, settings.FileName)))
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, "app", r.Project) },
		},
		"the shared file is a dangling symlink, read as missing": {
			setup: func(t *testing.T, f *fsFixture) {
				replace(t, filepath.Join(f.cfg, settings.FileName), func(p string) error {
					return os.Symlink(filepath.Join(t.TempDir(), "gone.toml"), p)
				})
			},
			project: "-",
			check:   func(t *testing.T, r settings.Resolved) { assert.Empty(t, r.Projects) },
		},
		"the shared file is a directory": {
			setup: func(t *testing.T, f *fsFixture) {
				replace(t, filepath.Join(f.cfg, settings.FileName), func(p string) error { return os.Mkdir(p, 0o700) })
			},
			wantErr: settings.ErrInvalid,
		},
		"the shared file cannot be read": {
			setup:   func(t *testing.T, f *fsFixture) { chmod(t, filepath.Join(f.cfg, settings.FileName), 0o000) },
			nonRoot: true, wantErr: fs.ErrPermission,
		},
		"the shared file is a FIFO": {
			setup:   func(t *testing.T, f *fsFixture) { f.fifo(t, filepath.Join(f.cfg, settings.FileName)) },
			wantErr: settings.ErrInvalid,
		},
		"the shared file links to a device": {
			setup: func(t *testing.T, f *fsFixture) {
				replace(t, filepath.Join(f.cfg, settings.FileName), func(p string) error { return os.Symlink("/dev/null", p) })
			},
			wantErr: settings.ErrInvalid,
		},
		"the shared file at exactly MaxFileBytes loads": {
			setup: func(t *testing.T, f *fsFixture) {
				writeFile(t, f.cfg, settings.FileName, padTo(strings.ReplaceAll(fsShared, "REPO", f.repo), settings.MaxFileBytes), "")
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, "app", r.Project) },
		},
		"the shared file one byte past MaxFileBytes": {
			setup: func(t *testing.T, f *fsFixture) {
				writeFile(t, f.cfg, settings.FileName, padTo(strings.ReplaceAll(fsShared, "REPO", f.repo), settings.MaxFileBytes+1), "")
			},
			wantErr: settings.ErrInvalid,
		},
		// The repo file.
		"the repo file is a symlink": {
			setup: func(t *testing.T, f *fsFixture) {
				target := filepath.Join(t.TempDir(), "overlay.toml")
				require.NoError(t, os.WriteFile(target, []byte("schema_version = 1\ncolor = \"#abcdef\"\n"), 0o600))
				require.NoError(t, os.Symlink(target, filepath.Join(f.repo, settings.RepoFileName)))
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, settings.LayerRepo, r.Sources["color"]) },
		},
		"the repo file is a dangling symlink, read as missing": {
			setup: func(t *testing.T, f *fsFixture) {
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone.toml"), filepath.Join(f.repo, settings.RepoFileName)))
			},
			check: func(t *testing.T, r settings.Resolved) { assert.NotContains(t, r.Sources, "color") },
		},
		"the repo file is a directory": {
			setup: func(t *testing.T, f *fsFixture) {
				require.NoError(t, os.Mkdir(filepath.Join(f.repo, settings.RepoFileName), 0o700))
			},
			wantErr: settings.ErrInvalid,
		},
		"the repo file cannot be read": {
			setup: func(t *testing.T, f *fsFixture) {
				p := filepath.Join(f.repo, settings.RepoFileName)
				require.NoError(t, os.WriteFile(p, []byte("schema_version = 1\n"), 0o600))
				chmod(t, p, 0o000)
			},
			nonRoot: true, wantErr: fs.ErrPermission,
		},
		"the repo file one byte past MaxFileBytes": {
			setup: func(t *testing.T, f *fsFixture) {
				writeFile(t, f.repo, settings.RepoFileName, padTo("schema_version = 1\n", settings.MaxFileBytes+1), "")
			},
			wantErr: settings.ErrInvalid,
		},
		"the repo cannot be searched": {
			setup:   func(t *testing.T, f *fsFixture) { chmod(t, f.repo, 0o000) },
			nonRoot: true, wantErr: fs.ErrPermission,
		},
		"a repo path with non-ASCII and spaces": {
			setup: func(t *testing.T, f *fsFixture) {
				repo := filepath.Join(t.TempDir(), "mon dépôt")
				bundleAt(t, repo)
				writeFile(t, f.cfg, settings.FileName, fsShared, repo)
			},
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, "mon dépôt", filepath.Base(r.Profile.Repo)) },
		},
		"the repo file is a FIFO": {
			setup:   func(t *testing.T, f *fsFixture) { f.fifo(t, filepath.Join(f.repo, settings.RepoFileName)) },
			wantErr: settings.ErrInvalid,
		},
		// The repo.
		"the repo is a symlink to a directory": {
			setup: func(t *testing.T, f *fsFixture) {
				link := filepath.Join(t.TempDir(), "link")
				require.NoError(t, os.Symlink(f.repo, link))
				writeFile(t, f.cfg, settings.FileName, fsShared, link)
				f.repo = link
			},
			check: func(t *testing.T, r settings.Resolved) {
				assert.Equal(t, "link", filepath.Base(r.Profile.Repo), "the repo stays as configured")
			},
		},
		"the repo is a dangling symlink": {
			setup: func(t *testing.T, f *fsFixture) {
				link := filepath.Join(t.TempDir(), "link")
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), link))
				writeFile(t, f.cfg, settings.FileName, fsShared, link)
			},
			wantErr: settings.ErrInvalid,
		},
		"the repo is a FIFO": {
			setup: func(t *testing.T, f *fsFixture) {
				p := filepath.Join(t.TempDir(), "repo")
				f.fifo(t, p)
				writeFile(t, f.cfg, settings.FileName, fsShared, p)
			},
			wantErr: settings.ErrInvalid,
		},
		"the repo's parent cannot be searched": {
			setup: func(t *testing.T, f *fsFixture) {
				parent := filepath.Join(t.TempDir(), "parent")
				repo := filepath.Join(parent, "app")
				bundleAt(t, repo)
				writeFile(t, f.cfg, settings.FileName, fsShared, repo)
				chmod(t, parent, 0o000)
			},
			nonRoot: true, wantErr: settings.ErrInvalid,
		},
		// The bundle marker.
		"the bundle marker is a symlink": {
			setup: func(t *testing.T, f *fsFixture) {
				target := filepath.Join(t.TempDir(), "version.txt")
				require.NoError(t, os.WriteFile(target, []byte("1\n"), 0o600))
				replace(t, filepath.Join(f.repo, filepath.FromSlash(settings.BundleMarker)), func(p string) error { return os.Symlink(target, p) })
			},
			check: func(t *testing.T, r settings.Resolved) { assert.NotNil(t, r.Profile) },
		},
		"the bundle marker is a dangling symlink": {
			setup: func(t *testing.T, f *fsFixture) {
				replace(t, filepath.Join(f.repo, filepath.FromSlash(settings.BundleMarker)), func(p string) error {
					return os.Symlink(filepath.Join(t.TempDir(), "gone"), p)
				})
			},
			wantErr: settings.ErrBypassesAgentisan,
		},
		"the bundle marker is a directory": {
			setup: func(t *testing.T, f *fsFixture) {
				replace(t, filepath.Join(f.repo, filepath.FromSlash(settings.BundleMarker)), func(p string) error { return os.Mkdir(p, 0o700) })
			},
			wantErr: settings.ErrBypassesAgentisan,
		},
		"the bundle marker is a FIFO": {
			setup: func(t *testing.T, f *fsFixture) {
				f.fifo(t, filepath.Join(f.repo, filepath.FromSlash(settings.BundleMarker)))
			},
			wantErr: settings.ErrBypassesAgentisan,
		},
		"the bundle directory cannot be searched": {
			setup:   func(t *testing.T, f *fsFixture) { chmod(t, filepath.Join(f.repo, ".claude"), 0o000) },
			nonRoot: true, wantErr: settings.ErrBypassesAgentisan,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if tc.nonRoot && os.Geteuid() == 0 {
				t.Skip("root ignores permission bits")
			}
			f := &fsFixture{cfg: t.TempDir(), repo: newRepo(t)}
			writeFile(t, f.cfg, settings.FileName, fsShared, f.repo)
			tc.setup(t, f)
			opts := settings.LoadOptions{ConfigDir: f.cfg, Project: "app", LookupEnv: envOf(nil)}
			if tc.project == "-" {
				opts.Project = ""
			}

			got, err := loadWithin(t, opts, f.fifos)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, got)
		})
	}
}

// padTo pads a TOML document with a trailing comment to exactly size bytes.
func padTo(doc string, size int) string {
	const open = "#"
	return doc + open + strings.Repeat("x", size-len(doc)-len(open)-1) + "\n"
}

package settings_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/recall"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestRecallSources: [recall] names where the boss's rules are kept (NERD-5741):
// memory directories, and bullet files each under the prefix its sections' IDs
// carry. Both are the machine's, so only the shared file names them.
func TestRecallSources(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\nshort = \"ap\"\n"
	home := t.TempDir()
	tests := map[string]struct {
		file     string // the shared file after schema_version; "-" for no file
		repoFile string
		project  string
		home     string
		want     recall.Sources
		wantErr  error
		wantIn   []string
	}{
		"no config file: none":     {file: "-"},
		"no [recall] table: none":  {file: app},
		"empty lists: none":        {file: "[recall]\nmemory_dirs = []\n" + app},
		"an absolute directory":    {file: "[recall]\nmemory_dirs = [\"/m/memory\"]\n" + app, want: recall.Sources{MemoryDirs: []string{"/m/memory"}}},
		"~/ expands with HOME":     {file: "[recall]\nmemory_dirs = [\"~/mem\"]\n" + app, home: home, want: recall.Sources{MemoryDirs: []string{filepath.Join(home, "mem")}}},
		"two directories in order": {file: "[recall]\nmemory_dirs = [\"/b\", \"/a\"]\n" + app, want: recall.Sources{MemoryDirs: []string{"/b", "/a"}}},
		"a project selected":       {file: "[recall]\nmemory_dirs = [\"/m\"]\n" + app, project: "app", want: recall.Sources{MemoryDirs: []string{"/m"}}},
		"bullet files, sorted by prefix": {
			file: "[recall.bullet_files]\nstd = \"/s/STANDING.md\"\nrul = \"~/r.md\"\n" + app, home: home,
			want: recall.Sources{Bullets: []recall.Bullets{{Prefix: "rul", Path: filepath.Join(home, "r.md")}, {Prefix: "std", Path: "/s/STANDING.md"}}},
		},
		"~/ with no HOME":          {file: "[recall]\nmemory_dirs = [\"~/m\"]\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"HOME"}},
		"a relative directory":     {file: "[recall]\nmemory_dirs = [\"m\"]\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"recall.memory_dirs", "shared", "absolute"}},
		"an empty entry":           {file: "[recall]\nmemory_dirs = [\"\"]\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"recall.memory_dirs"}},
		"a relative bullet file":   {file: "[recall.bullet_files]\nstd = \"S.md\"\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"recall.bullet_files.std", "absolute"}},
		"a control character":      {file: "[recall.bullet_files]\nstd = \"/a\\nb.md\"\n" + app, wantErr: settings.ErrInvalid},
		"not a list":               {file: "[recall]\nmemory_dirs = \"/m\"\n" + app, wantErr: settings.ErrInvalid},
		"bullet_files not a table": {file: "[recall]\nbullet_files = \"/m\"\n" + app, wantErr: settings.ErrInvalid},
		"an unknown key":           {file: "[recall]\nurl = \"x\"\n" + app, wantErr: settings.ErrUnknownKey, wantIn: []string{"recall.url"}},
		"in a project table":       {file: app + "[projects.app.recall]\nmemory_dirs = [\"/m\"]\n", wantErr: settings.ErrUnknownKey},
		"in a repo file":           {file: app, repoFile: "[recall]\nmemory_dirs = [\"/m\"]\n", project: "app", wantErr: settings.ErrUnknownKey, wantIn: []string{"recall"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			if tc.file != "-" {
				writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.file, repo)
			}
			if tc.repoFile != "" {
				writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+tc.repoFile, repo)
			}
			env := map[string]string{}
			if tc.home != "" {
				env["HOME"] = tc.home
			}

			var err error
			var got recall.Sources
			if tc.project != "" || tc.repoFile != "" {
				// Load is where a project and a repo file meet the key.
				_, err = settings.Load(settings.LoadOptions{ConfigDir: dir, Project: tc.project, LookupEnv: envOf(env)})
				if err == nil {
					got, err = settings.RecallSources(dir, envOf(env))
				}
			} else {
				got, err = settings.RecallSources(dir, envOf(env))
			}
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				for _, s := range tc.wantIn {
					assert.Contains(t, err.Error(), s)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRecallSourcesWithoutAConfigDir: no config dir has no sources, and that
// is not an error here; the command says what is missing.
func TestRecallSourcesWithoutAConfigDir(t *testing.T) {
	t.Parallel()
	got, err := settings.RecallSources("", envOf(nil))
	require.NoError(t, err)
	assert.Equal(t, recall.Sources{}, got)
}

// TestRecallKeysAreResolvedFromTheSharedFile: config resolve shows each key,
// and the layer that set it.
func TestRecallKeysAreResolvedFromTheSharedFile(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[recall]\nmemory_dirs = [\"/m\"]\n[recall.bullet_files]\nstd = \"/s.md\"\n", repo)
	res, err := settings.Load(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
	require.NoError(t, err)
	for path, value := range map[string]any{"recall.memory_dirs": []string{"/m"}, "recall.bullet_files.std": "/s.md"} {
		l, ok := leaf(t, res, path)
		require.True(t, ok, path)
		assert.Equal(t, value, l.Value)
		assert.Equal(t, settings.LayerShared, l.Layer)
	}
}

package settings_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// samplePath is the commented example shipped in docs/.
var samplePath = filepath.Join("..", "..", "docs", "config", "herdr-agentisan.example.toml")

// TestSampleConfigResolves keeps the documented sample loadable: it is
// copied in as the shared file, HOME is a temp dir, and a faked stat says
// both repos exist with the Agentisan bundle installed.
func TestSampleConfigResolves(t *testing.T) {
	t.Parallel()
	sample, err := os.ReadFile(samplePath)
	require.NoError(t, err)
	dir, home := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), sample, 0o600))

	repos := map[string]string{
		"agentisan": filepath.Join(home, "Development", "nerdsrun", "agentisan-skills"),
		"assay":     filepath.Join(home, "Development", "nerdsrun", "assay"),
	}
	tree := fstest.MapFS{}
	for _, repo := range repos {
		tree[strings.TrimPrefix(filepath.ToSlash(filepath.Join(repo, settings.BundleMarker)), "/")] = &fstest.MapFile{Data: []byte("1\n")}
	}
	stat := func(name string) (fs.FileInfo, error) {
		return fs.Stat(tree, strings.TrimPrefix(filepath.ToSlash(name), "/"))
	}
	opts := settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(map[string]string{"HOME": home}), Stat: stat}

	all, err := settings.Load(opts)
	require.NoError(t, err)
	assert.Equal(t, []string{"agentisan", "assay"}, all.Projects)

	for name, repo := range repos {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := opts
			o.Project = name
			got, err := settings.Load(o)
			require.NoError(t, err)
			p := got.Profile
			require.NotNil(t, p)
			assert.Equal(t, repo, p.Repo)
			assert.Equal(t, "nerds-run/"+filepath.Base(repo), p.GitHub)
			assert.NotEmpty(t, p.Color)
			for _, g := range []string{"coders", "precheck", "qa", "codex", "research", "clerk", "debugger"} {
				assert.Containsf(t, p.Groups, g, "group %s", g)
			}
			assert.NotEmpty(t, p.Verify)
			var env, op bool
			for _, in := range p.Integrations {
				if in.Token != nil {
					env = env || in.Token.Env != ""
					op = op || in.Token.Op != ""
				}
			}
			assert.True(t, env, "the sample shows an env reference")
			assert.True(t, op, "the sample shows an op:// reference")
		})
	}
}

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
	assert.Equal(t, "agentisan", all.SpacesProject(), "the sample names the spaces' project")
	assert.Equal(t, "127.0.0.1:4328", all.TelemetryEndpoint(), "the sample names the events' collector")
	assert.Equal(t, []string{"/run/user/1000/agentisan/gate.0.lock"}, all.TestLocks(), "the sample names the test slots' lock")
	locks, err := settings.ReadTestLocks(dir)
	require.NoError(t, err)
	skills := []string{"/tmp/agentisan-suite-1000.lock", "/tmp/agentisan-suite-1000.slot2.lock", "/tmp/agentisan-suite-1000.slot3.lock"}
	assert.Equal(t, skills, locks.Of("agentisan"), "the sample gives a project its own test slots")
	assert.Equal(t, []string{"/run/user/1000/agentisan/gate.0.lock"}, locks.Of("assay"), "a project with none of its own counts the shared ones")
	envFile, err := settings.DebuggerEnvFile(dir, envOf(map[string]string{"HOME": home}))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".config", "herdr-agentisan", "elastic.env"), envFile, "the sample names the debugger's env file")
	assert.False(t, all.Routing().Enabled, "the sample leaves routing off")
	assert.Equal(t, settings.LayerShared, all.Sources["routing.enabled"], "the sample sets routing.enabled itself")
	assert.Equal(t, []settings.Team{
		{Project: "agentisan", Layout: settings.LayoutSpaces, Color: "#7c3aed", Descriptions: map[string]string{
			"precheck": "a pre-Codex adversarial pass on finalized heads",
			"qa":       "runs the pre-PR test gates and owns the test-slot queue",
			"coders":   "write fixes + new code · run their own review loop",
		}},
		{Project: "assay", Short: "as", Layout: settings.LayoutTabs, Color: "#0ea5e9", Descriptions: map[string]string{
			"precheck": "a pre-Codex adversarial pass on finalized heads",
		}},
	}, all.Teams, "the spaces' project and one tabs team, each with its group descriptions")

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
			for _, g := range []string{"coder", "precheck", "qa", "review", "research", "librarian", "debugger"} {
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
			// The workforce keys (A23): a group's own model, review on
			// codex, and a shell prompt to wait for, with no keys typed.
			assert.Equal(t, "sonnet", p.AgentFor("precheck").Model)
			assert.Equal(t, []string{"codex", "--no-daemon", "--sandbox", "read-only"}, p.AgentFor("review").Command)
			assert.Equal(t, settings.Spawn{ShellPrompt: `❯\s*$`, ShellReadyAfter: settings.DefaultShellReadyAfter}, p.Spawn)
			// The rotation thresholds (A33): the project's, and one role's.
			assert.Equal(t, []int{55, 65, 75}, p.Rotation.Thresholds)
			assert.Equal(t, []int{45, 55, 65}, p.Groups["librarian"].Rotation.Thresholds)
		})
	}
	rs, err := settings.Rotations(opts)
	require.NoError(t, err)
	assert.Equal(t, []int{45, 55, 65}, rs.For("assay").For("librarian"), "the daemon reads the sample's role thresholds")
	assert.Equal(t, []int{55, 65, 75}, rs.For("assay").For(""))
}

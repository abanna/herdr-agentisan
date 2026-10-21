package settings_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestDebuggerEnvFile: [debugger] env_file names the env file the debugger
// reads Elasticsearch's URL and read key from (item 12). It is a shared key, as
// [telemetry] is: the shared file sets it, --set overrides it, a project
// table or a repo file cannot. It is an absolute path or one under ~/,
// expanded with HOME when read; anything else is refused at load.
func TestDebuggerEnvFile(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	home := t.TempDir()
	tests := map[string]struct {
		file     string // the shared file after schema_version; "-" for no file
		repoFile string
		project  string
		set      []string
		home     string
		want     string
		wantErr  error
		wantIn   []string
	}{
		"no config file: none":        {file: "-"},
		"no [debugger] table: none":   {file: app},
		"set empty: none":             {file: "[debugger]\nenv_file = \"\"\n" + app},
		"an absolute path":            {file: "[debugger]\nenv_file = \"/etc/elastic.env\"\n" + app, want: "/etc/elastic.env"},
		"a path under ~/":             {file: "[debugger]\nenv_file = \"~/.config/x/local.env\"\n" + app, home: home, want: filepath.Join(home, ".config/x/local.env")},
		"--set beats the shared file": {file: "[debugger]\nenv_file = \"/a.env\"\n" + app, set: []string{"debugger.env_file=/b.env"}, want: "/b.env"},
		"a project selected":          {file: "[debugger]\nenv_file = \"/a.env\"\n" + app, project: "app", want: "/a.env"},
		"~/ with no HOME":             {file: "[debugger]\nenv_file = \"~/x.env\"\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"HOME"}},
		"a relative path":             {file: "[debugger]\nenv_file = \"x/local.env\"\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"debugger.env_file", "shared", "absolute"}},
		"~ alone":                     {file: "[debugger]\nenv_file = \"~/\"\n" + app, wantErr: settings.ErrInvalid},
		"~user":                       {file: "[debugger]\nenv_file = \"~root/x.env\"\n" + app, wantErr: settings.ErrInvalid},
		"a control character":         {file: "[debugger]\nenv_file = \"/a\\nb.env\"\n" + app, wantErr: settings.ErrInvalid},
		"a relative path from --set":  {file: app, set: []string{"debugger.env_file=x.env"}, wantErr: settings.ErrInvalid, wantIn: []string{"--set"}},
		"not a string":                {file: "[debugger]\nenv_file = 1\n" + app, wantErr: settings.ErrInvalid},
		"an unknown key, the URL":     {file: "[debugger]\nurl = \"http://es:9200\"\n" + app, wantErr: settings.ErrUnknownKey, wantIn: []string{"debugger.url"}},
		"in a project table":          {file: app + "[projects.app.debugger]\nenv_file = \"/a.env\"\n", wantErr: settings.ErrUnknownKey},
		"in a repo file":              {file: app, repoFile: "[debugger]\nenv_file = \"/a.env\"\n", project: "app", wantErr: settings.ErrUnknownKey},
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

			if tc.set != nil || tc.project != "" || tc.repoFile != "" {
				// Load is where --set, a project and a repo file meet the key.
				res, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: tc.project, Set: tc.set, LookupEnv: envOf(env)})
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
					for _, s := range tc.wantIn {
						assert.Contains(t, err.Error(), s)
					}
					return
				}
				require.NoError(t, err)
				l, ok := leaf(t, res, "debugger.env_file")
				require.True(t, ok)
				assert.Equal(t, tc.want, l.Value)
				return
			}
			got, err := settings.DebuggerEnvFile(dir, envOf(env))
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

// TestDebuggerEnvFileWithoutAConfigDir: a debugger whose environment names
// no config dir has no env file, and that is not an error here; the events
// command says what is missing.
func TestDebuggerEnvFileWithoutAConfigDir(t *testing.T) {
	t.Parallel()
	got, err := settings.DebuggerEnvFile("", envOf(nil))
	require.NoError(t, err)
	assert.Empty(t, got)
}

package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestSpacesProjectPrecedence: [spaces] project names the project today's ◆
// spaces belong to (A17). It is a shared key: the shared file sets it and
// --set overrides it, and its provenance is reported like any leaf. Unset,
// the sole configured project is its default; with none or several, the
// spaces belong to the unnamed project and no leaf is reported.
func TestSpacesProjectPrecedence(t *testing.T) {
	t.Parallel()

	const two = "[projects.app]\nrepo = \"REPO\"\n[projects.alpha]\nrepo = \"/nonexistent\"\n"
	tests := map[string]struct {
		file    string // the shared file after schema_version; "-" for no file
		project string // the project selected; "" for none
		set     []string
		want    string
		layer   string // "" when no layer reports the key
	}{
		"no config file: the unnamed project":          {file: "-"},
		"no projects: the unnamed project":             {file: "[agent]\nmodel = \"opus\"\n"},
		"one project: it, by default":                  {file: "[projects.app]\nrepo = \"REPO\"\n", want: "app", layer: settings.LayerDefault},
		"one project, selected: it, by default":        {file: "[projects.app]\nrepo = \"REPO\"\n", project: "app", want: "app", layer: settings.LayerDefault},
		"several projects: the unnamed project":        {file: two},
		"set in the shared file":                       {file: "[spaces]\nproject = \"alpha\"\n" + two, want: "alpha", layer: settings.LayerShared},
		"set in the shared file, a project selected":   {file: "[spaces]\nproject = \"alpha\"\n" + two, project: "app", want: "alpha", layer: settings.LayerShared},
		"set as a dotted key":                          {file: "spaces.project = \"app\"\n" + two, want: "app", layer: settings.LayerShared},
		"--set beats the shared file":                  {file: "[spaces]\nproject = \"alpha\"\n" + two, set: []string{"spaces.project=app"}, want: "app", layer: settings.LayerFlag},
		"--set with no key in the file":                {file: two, set: []string{"spaces.project=alpha"}, project: "app", want: "alpha", layer: settings.LayerFlag},
		"--set beats the one-project default":          {file: "[projects.app]\nrepo = \"REPO\"\n", set: []string{`spaces.project="app"`}, want: "app", layer: settings.LayerFlag},
		"an empty [spaces] table: the default applies": {file: "[spaces]\n[projects.app]\nrepo = \"REPO\"\n", want: "app", layer: settings.LayerDefault},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			if tc.file != "-" {
				writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.file, repo)
			}

			got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: tc.project, Set: tc.set, LookupEnv: envOf(nil)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.SpacesProject())
			l, ok := leaf(t, got, "spaces.project")
			if tc.layer == "" {
				assert.False(t, ok, "no layer reports spaces.project")
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.want, l.Value)
			assert.Equal(t, tc.layer, l.Layer)
		})
	}
}

// TestSpacesProjectErrors: spaces.project must name a configured project,
// exactly, and only the shared file and --set may set it: a project or repo
// file choosing which project the spaces belong to would be circular. Each
// error names the key path, its layer and file, and the known projects.
func TestSpacesProjectErrors(t *testing.T) {
	t.Parallel()

	const two = "[projects.app]\nrepo = \"REPO\"\n[projects.alpha]\nrepo = \"/nonexistent\"\n"
	tests := map[string]struct {
		file     string
		repoFile string
		project  string
		set      []string
		wantErr  error
		wantIn   []string
	}{
		"an unknown project": {
			file: "[spaces]\nproject = \"beta\"\n" + two, wantErr: settings.ErrNoProject,
			wantIn: []string{"spaces.project", "shared", settings.FileName, `"beta"`, "alpha, app"},
		},
		"an unknown project with one selected": {
			file: "[spaces]\nproject = \"beta\"\n" + two, project: "app", wantErr: settings.ErrNoProject, wantIn: []string{"spaces.project", `"beta"`},
		},
		"an unknown project from --set": {
			file: two, set: []string{"spaces.project=beta"}, wantErr: settings.ErrNoProject, wantIn: []string{"spaces.project", "--set", "alpha, app"},
		},
		"no projects configured": {
			file: "[spaces]\nproject = \"app\"\n", wantErr: settings.ErrNoProject, wantIn: []string{"spaces.project", "known projects: none"},
		},
		"an empty name":            {file: "[spaces]\nproject = \"\"\n" + two, wantErr: settings.ErrNoProject, wantIn: []string{"spaces.project", `""`}},
		"a case variant":           {file: "[spaces]\nproject = \"App\"\n" + two, wantErr: settings.ErrNoProject, wantIn: []string{`"App"`}},
		"surrounding white space":  {file: "[spaces]\nproject = \" app\"\n" + two, wantErr: settings.ErrNoProject, wantIn: []string{`" app"`}},
		"a non-ASCII name":         {file: "[spaces]\nproject = \"äpp\"\n" + two, wantErr: settings.ErrNoProject, wantIn: []string{`"äpp"`}},
		"a NUL in the name":        {file: "[spaces]\nproject = \"a\\u0000pp\"\n" + two, wantErr: settings.ErrNoProject, wantIn: []string{`"a\u0000pp"`}},
		"a name like a flag":       {file: two, set: []string{"spaces.project=--help"}, wantErr: settings.ErrNoProject, wantIn: []string{`"--help"`}},
		"not a string":             {file: "[spaces]\nproject = 3\n" + two, wantErr: settings.ErrInvalid, wantIn: []string{"spaces.project", "want a string"}},
		"spaces is not a table":    {file: "spaces = \"app\"\n" + two, wantErr: settings.ErrInvalid, wantIn: []string{"spaces", "want a table"}},
		"an unknown key in spaces": {file: "[spaces]\nlayout = \"tabs\"\n" + two, wantErr: settings.ErrUnknownKey, wantIn: []string{"spaces.layout"}},
		"in a project table": {
			file: "[projects.app]\nrepo = \"REPO\"\n[projects.app.spaces]\nproject = \"app\"\n", wantErr: settings.ErrUnknownKey,
			wantIn: []string{"projects.app.spaces"},
		},
		"in a repo file": {
			file: "[projects.app]\nrepo = \"REPO\"\n", repoFile: "[spaces]\nproject = \"app\"\n", project: "app",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"spaces", settings.RepoFileName},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.file, repo)
			if tc.repoFile != "" {
				writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+tc.repoFile, repo)
			}

			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: tc.project, Set: tc.set, LookupEnv: envOf(nil)})
			require.ErrorIs(t, err, tc.wantErr)
			for _, s := range tc.wantIn {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
}

// TestSpacesProjectForTheDaemon: the daemon reads the project once at start,
// from HERDR_PLUGIN_CONFIG_DIR. It must run with no configuration at all,
// which is the user's case today: no directory, or no file in it, is the
// unnamed project, not an error. A file that does not resolve is an error the
// caller decides about.
func TestSpacesProjectForTheDaemon(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		file    string // "" for no file; "nodir" for no config dir
		want    string
		wantErr error
	}{
		"no config dir":          {file: "nodir"},
		"no file":                {},
		"the key":                {file: "[spaces]\nproject = \"app\"\n[projects.app]\nrepo = \"/nonexistent\"\n[projects.b]\nrepo = \"/x\"\n", want: "app"},
		"the sole project":       {file: "[projects.app]\nrepo = \"/nonexistent\"\n", want: "app"},
		"an unknown project":     {file: "[spaces]\nproject = \"x\"\n", wantErr: settings.ErrNoProject},
		"an unrelated bad value": {file: "color = \"red\"\n", wantErr: settings.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.file == "nodir" {
				dir = ""
			} else if tc.file != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+tc.file), 0o600))
			}

			got, err := settings.SpacesProject(dir)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, tc.want, got)
		})
	}
}

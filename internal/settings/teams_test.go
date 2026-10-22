package settings_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestTeams: every configured project has a team (A19). The [spaces] project
// is a spaces team, explicitly or as the sole project's default; every other
// project is a tabs team, which needs a short code. A sole project that sets
// layout = "tabs" is no default for the spaces, which then belong to the
// unnamed project. Each team's colour comes from the defaults, the shared
// tables, its own table and --set, never its repo file; the same teams come
// back whether or not a project is selected.
func TestTeams(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	tests := map[string]struct {
		file       string // the shared file after schema_version
		repoFile   string
		set        []string
		want       []settings.Team
		wantSpaces string
	}{
		"no projects": {},
		"the sole project is the spaces'": {
			file: app, want: []settings.Team{{Project: "app", Layout: settings.LayoutSpaces}}, wantSpaces: "app",
		},
		"the sole project, a tabs team": {
			file: app + "short = \"ap\"\nlayout = \"tabs\"\n", want: []settings.Team{{Project: "app", Short: "ap", Layout: settings.LayoutTabs}},
		},
		"the spaces' project and a tabs team": {
			file:       "[spaces]\nproject = \"b\"\n" + app + "short = \"ap\"\ncolor = \"#123456\"\n[projects.b]\nrepo = \"/x\"\n",
			want:       []settings.Team{{Project: "app", Short: "ap", Layout: settings.LayoutTabs, Color: "#123456"}, {Project: "b", Layout: settings.LayoutSpaces}},
			wantSpaces: "b",
		},
		"two tabs teams": {
			file: app + "short = \"ap\"\n[projects.b]\nrepo = \"/x\"\nshort = \"b\"\nlayout = \"tabs\"\n",
			want: []settings.Team{{Project: "app", Short: "ap", Layout: settings.LayoutTabs}, {Project: "b", Short: "b", Layout: settings.LayoutTabs}},
		},
		"the spaces' project may set a short code and its layout": {
			file: app + "short = \"ap\"\nlayout = \"spaces\"\n", want: []settings.Team{{Project: "app", Short: "ap", Layout: settings.LayoutSpaces}}, wantSpaces: "app",
		},
		"the longest short code": {
			file: app + "short = \"abcdefgh\"\nlayout = \"tabs\"\n", want: []settings.Team{{Project: "app", Short: "abcdefgh", Layout: settings.LayoutTabs}},
		},
		"a shared colour, overridden by --set": {
			file: "color = \"#111111\"\n" + app, set: []string{`color="#222222"`},
			want: []settings.Team{{Project: "app", Layout: settings.LayoutSpaces, Color: "#222222"}}, wantSpaces: "app",
		},
		"a repo file's colour is not the team's": {
			file: app + "color = \"#111111\"\n", repoFile: "color = \"#333333\"\n",
			want: []settings.Team{{Project: "app", Layout: settings.LayoutSpaces, Color: "#111111"}}, wantSpaces: "app",
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

			all, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Set: tc.set, LookupEnv: envOf(nil)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, all.Teams)
			assert.Equal(t, tc.wantSpaces, all.SpacesProject())
			if len(tc.want) == 0 {
				return
			}
			one, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", Set: tc.set, LookupEnv: envOf(nil)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, one.Teams, "the same teams with a project selected")
			if tc.set == nil {
				daemon, err := settings.Teams(dir)
				require.NoError(t, err)
				assert.Equal(t, tc.want, daemon, "what the daemon reads at start")
			}
		})
	}
}

// TestTeamsErrors: layout is "tabs" or "spaces", and "spaces" only for the
// [spaces] project, which is never "tabs". A tabs team needs a short code: a
// lower-case letter, then up to 7 lower-case letters or digits, unique across
// projects. Only a project's own table sets short and layout; boss is the
// boss's tab, never a group. Each error names the key path and, for a set
// value, its layer and file; a selected project does not skip the others.
func TestTeamsErrors(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	const b = "[projects.b]\nrepo = \"/x\"\nshort = \"b\"\n"
	tests := map[string]struct {
		file     string
		repoFile string
		project  string
		set      []string
		wantErr  error
		wantIn   []string
	}{
		"an unknown layout":           {file: app + "layout = \"grid\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.layout", "project", settings.FileName, `"grid"`}},
		"a layout in upper case":      {file: app + "layout = \"Tabs\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.layout"}},
		"a layout that is no string":  {file: app + "layout = 3\n", wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.layout", "want a string"}},
		"the spaces' project as tabs": {file: "[spaces]\nproject = \"app\"\n" + app + "layout = \"tabs\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.layout", "always"}},
		"spaces on another project":   {file: "[spaces]\nproject = \"b\"\n" + app + "layout = \"spaces\"\nshort = \"ap\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.layout", `spaces.project = "app"`}},
		"spaces with two projects":    {file: app + "short = \"ap\"\nlayout = \"spaces\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.layout"}},
		"a tabs team with no short":   {file: app + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short", "short code"}},
		"the sole tabs team, no short": {
			file: app + "layout = \"tabs\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"},
		},
		"an empty short":            {file: app + "short = \"\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"}},
		"a short in upper case":     {file: app + "short = \"Ha\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short", `"Ha"`}},
		"a short starting a digit":  {file: app + "short = \"1a\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"}},
		"a short of nine":           {file: app + "short = \"abcdefghi\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"}},
		"a short with a dash":       {file: app + "short = \"h-a\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"}},
		"a short with a space":      {file: app + "short = \"ha \"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"}},
		"a non-ASCII short":         {file: app + "short = \"hä\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"}},
		"a NUL in the short":        {file: app + "short = \"h\\u0000\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short"}},
		"a short that is no string": {file: app + "short = 7\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.short", "want a string"}},
		"a short taken twice":       {file: app + "short = \"b\"\n" + b, wantErr: settings.ErrInvalid, wantIn: []string{"projects.b.short", "project app's"}},
		"a short taken, another project selected": {
			file: app + "short = \"ap\"\n" + b + "[projects.c]\nrepo = \"/y\"\nshort = \"b\"\n", project: "app",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.c.short"},
		},
		"short in the shared tables": {file: "short = \"ha\"\n" + app, wantErr: settings.ErrUnknownKey, wantIn: []string{"short", "shared"}},
		"layout in the shared tables": {
			file: "layout = \"tabs\"\n" + app, wantErr: settings.ErrUnknownKey, wantIn: []string{"layout", "shared"},
		},
		"short from --set":  {file: app, set: []string{"short=ha"}, wantErr: settings.ErrUnknownKey, wantIn: []string{"short", "--set"}},
		"layout from --set": {file: app, set: []string{"layout=tabs"}, wantErr: settings.ErrUnknownKey, wantIn: []string{"layout", "--set"}},
		"short in a repo file": {
			file: app, repoFile: "short = \"ha\"\n", project: "app", wantErr: settings.ErrUnknownKey, wantIn: []string{"short", settings.RepoFileName},
		},
		"boss as a shared group": {
			file: "[groups.boss]\nworkers = 1\nslots = 1\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"groups.boss", "boss's tab"},
		},
		"boss as a project's group": {
			file: app + "[projects.app.groups.boss]\nworkers = 0\nslots = 1\n", wantErr: settings.ErrInvalid, wantIn: []string{"groups.boss"},
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

// TestGroupNamesFitAWorkerID: a group's name is at most 20 characters, from
// any layer, so a worker's id, <short>-<group>-<n>, fits herdr's 32-character
// agent names with any short code (8 at most) and n up to 99 (A53). The error
// names the group with the lowest layer and file that set it, even an empty
// table; a project and an integration name are not bounded.
func TestGroupNamesFitAWorkerID(t *testing.T) {
	t.Parallel()
	const app = "[projects.app]\nrepo = \"REPO\"\n"
	at20, at21, long := strings.Repeat("g", 20), strings.Repeat("g", 21), strings.Repeat("g", 300)
	counts := "workers = 1\nslots = 1\n"
	tests := map[string]struct {
		file, repoFile string
		set            []string
		wantIn         string // "" for none: it resolves
	}{
		"20 characters":           {file: app + "[projects.app.groups." + at20 + "]\n" + counts},
		"21 from the shared file": {file: "[groups." + at21 + "]\n" + counts + app, wantIn: "groups." + at21 + " (shared, "},
		"21 from a project":       {file: app + "[projects.app.groups." + at21 + "]\n" + counts, wantIn: "groups." + at21 + " (project, "},
		"300 from a repo file":    {file: app, repoFile: "[groups." + long + "]\n" + counts, wantIn: "groups." + long + " (repo, "},
		"21 from --set": {
			file: app, set: []string{"groups." + at21 + ".workers=1", "groups." + at21 + ".slots=1"}, wantIn: "groups." + at21 + " (flag, ",
		},
		"21 that only describes": {file: app + "[projects.app.groups." + at21 + "]\ndescription = \"x\"\n", wantIn: "groups." + at21 + " (project, "},
		"21 as an empty table":   {file: "[groups." + at21 + "]\n" + app, wantIn: "groups." + at21 + " (shared, "},
		// The lowest layer that has the group names it.
		"21 from two layers": {
			file: app + "[projects.app.groups." + at21 + "]\nslots = 1\n", repoFile: "[groups." + at21 + "]\nworkers = 1\n", wantIn: "groups." + at21 + " (project, ",
		},
		"a long project name": {file: "[projects." + long + "]\nrepo = \"REPO\"\n"},
		"a long integration name": {
			file: app + "[integrations." + long + "]\nurl = \"https://example.com\"\n",
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
			project := "app"
			if name == "a long project name" {
				project = long
			}

			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project, Set: tc.set, LookupEnv: envOf(nil)})
			if tc.wantIn == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.Contains(t, err.Error(), "at most 20 characters")
		})
	}
}

// TestTeamsForTheDaemon: the daemon runs with no configuration at all, so no
// directory, or no file in it, is no team; a file that does not resolve is
// an error the caller decides about.
func TestTeamsForTheDaemon(t *testing.T) {
	t.Parallel()

	got, err := settings.Teams("")
	require.NoError(t, err)
	assert.Empty(t, got, "no config dir")

	got, err = settings.Teams(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, got, "no file")

	dir := t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.a]\nrepo = \"/x\"\n[projects.b]\nrepo = \"/y\"\n", "")
	_, err = settings.Teams(dir)
	require.ErrorIs(t, err, settings.ErrInvalid, "two tabs teams without short codes")
	assert.Contains(t, err.Error(), "projects.a.short")
}

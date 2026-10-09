package settings_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// plainSecret stands in for a secret pasted into config. No error may ever
// contain it.
const plainSecret = "hunter2"

// envOf is a lookup over a fixed map, so no test reads the real environment.
func envOf(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

// newRepo returns a temp repo with the Agentisan bundle installed.
func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	marker := filepath.Join(repo, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1.0.0\n"), 0o600))
	return repo
}

// writeFile writes content to dir/name, replacing every REPO with repo.
func writeFile(t *testing.T, dir, name, content, repo string) {
	t.Helper()
	content = strings.ReplaceAll(content, "REPO", repo)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

// leaf finds one resolved leaf by path.
func leaf(t *testing.T, r settings.Resolved, path string) (settings.Leaf, bool) {
	t.Helper()
	for _, l := range r.Leaves() {
		if l.Path == path {
			return l, true
		}
	}
	return settings.Leaf{}, false
}

type want struct {
	value any
	layer string
}

// TestLoadLayerPrecedence has one row per layer showing which one wins, and
// one per merge rule: tables deep-merge, scalars, lists and secret refs
// replace whole. Every row also checks provenance.
func TestLoadLayerPrecedence(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		shared  string // top-level tables of the shared file
		project string // body of [projects.app], after its repo
		repo    string // body of the repo file; empty means no repo file
		set     []string
		want    map[string]want
	}{
		"built-in defaults apply when no layer sets a value": {
			want: map[string]want{
				"rotation.thresholds": {[]int{55, 65, 75}, settings.LayerDefault},
				"agent.command":       {[]string{"claude"}, settings.LayerDefault},
			},
		},
		"shared beats default": {
			shared: "[rotation]\nthresholds = [50, 60, 70]\n",
			want: map[string]want{
				"rotation.thresholds": {[]int{50, 60, 70}, settings.LayerShared},
			},
		},
		"thresholds at both bounds are accepted": {
			shared: "[rotation]\nthresholds = [1, 50, 100]\n",
			want: map[string]want{
				"rotation.thresholds": {[]int{1, 50, 100}, settings.LayerShared},
			},
		},
		"the group bounds are accepted: slots equal to workers, workers 0": {
			shared: "[groups.qa]\nworkers = 2\nslots = 2\n[groups.idle]\nworkers = 0\nslots = 0\n",
			want: map[string]want{
				"groups.qa.slots":     {2, settings.LayerShared},
				"groups.idle.workers": {0, settings.LayerShared},
				"groups.idle.slots":   {0, settings.LayerShared},
			},
		},
		"a colour in upper case is accepted": {
			project: "color = \"#ABCDEF\"\n",
			want: map[string]want{
				"color": {"#ABCDEF", settings.LayerProject},
			},
		},
		"project beats shared": {
			shared:  "color = \"#111111\"\n",
			project: "color = \"#222222\"\n",
			want: map[string]want{
				"color": {"#222222", settings.LayerProject},
			},
		},
		"repo file beats project": {
			project: "color = \"#222222\"\n",
			repo:    "color = \"#333333\"\n",
			want: map[string]want{
				"color": {"#333333", settings.LayerRepo},
			},
		},
		"flag beats repo file, and an unquoted value is a string": {
			repo: "color = \"#333333\"\n",
			set:  []string{"color=#444444"},
			want: map[string]want{
				"color": {"#444444", settings.LayerFlag},
			},
		},
		"the last --set for a path wins": {
			set: []string{`agent.model="sonnet"`, "agent.model=opus"},
			want: map[string]want{
				"agent.model": {"opus", settings.LayerFlag},
			},
		},
		"tables deep-merge: each key keeps the layer that set it": {
			shared:  "[agent]\nmodel = \"opus\"\n",
			project: "[projects.app.agent]\ncommand = [\"codex\"]\n",
			want: map[string]want{
				"agent.model":   {"opus", settings.LayerShared},
				"agent.command": {[]string{"codex"}, settings.LayerProject},
			},
		},
		"a group deep-merges key by key across four layers": {
			shared:  "[groups.coders]\nworkers = 1\nslots = 4\n",
			project: "[projects.app.groups.coders]\nworkers = 3\n",
			repo:    "[groups.qa]\nworkers = 1\nslots = 1\n",
			set:     []string{"groups.qa.slots=2"},
			want: map[string]want{
				"groups.coders.workers": {3, settings.LayerProject},
				"groups.coders.slots":   {4, settings.LayerShared},
				"groups.qa.workers":     {1, settings.LayerRepo},
				"groups.qa.slots":       {2, settings.LayerFlag},
			},
		},
		"a list replaces whole, it never appends": {
			shared:  "verify = [[\"task\", \"lint\"], [\"task\", \"test\"]]\n",
			project: "verify = [[\"task\", \"check\"]]\n",
			want: map[string]want{
				"verify": {[][]string{{"task", "check"}}, settings.LayerProject},
			},
		},
		"a list from --set replaces whole": {
			repo: "[rotation]\nthresholds = [40, 50, 60]\n",
			set:  []string{"rotation.thresholds=[50,60,70]"},
			want: map[string]want{
				"rotation.thresholds": {[]int{50, 60, 70}, settings.LayerFlag},
			},
		},
		"a secret ref replaces whole, its keys never merge": {
			shared: "[integrations.github]\ntoken = { env = \"SHARED_TOKEN\" }\nurl = \"https://github.com\"\n",
			project: "[projects.app.integrations.github]\n" +
				"token = { op = \"op://Vault/Item/field\" }\n",
			want: map[string]want{
				"integrations.github.token": {settings.SecretRef{Op: "op://Vault/Item/field"}, settings.LayerProject},
				"integrations.github.url":   {"https://github.com", settings.LayerShared},
			},
		},
		"a secret ref from --set is a reference": {
			set: []string{`integrations.github.token={ env = "FLAG_TOKEN" }`},
			want: map[string]want{
				"integrations.github.token": {settings.SecretRef{Env: "FLAG_TOKEN"}, settings.LayerFlag},
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName,
				"schema_version = 1\n"+tc.shared+"[projects.app]\nrepo = \"REPO\"\n"+tc.project, repo)
			if tc.repo != "" {
				writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+tc.repo, repo)
			}

			got, err := settings.Load(settings.LoadOptions{
				ConfigDir: dir, Project: "app", Set: tc.set, LookupEnv: envOf(nil),
			})
			require.NoError(t, err)
			for path, w := range tc.want {
				l, ok := leaf(t, got, path)
				require.Truef(t, ok, "no leaf %s in %v", path, got.Sources)
				assert.Equalf(t, w.value, l.Value, "value of %s", path)
				assert.Equalf(t, w.layer, l.Layer, "layer of %s", path)
				assert.Equalf(t, w.layer, got.Sources[path], "Sources[%s]", path)
			}
			assert.Equal(t, settings.LayerProject, got.Sources["repo"])
		})
	}
}

// TestLoadBuildsTheProfile: every field of the resolved profile is filled
// from the merged layers, and ~ in repo is expanded with the injected HOME.
func TestLoadBuildsTheProfile(t *testing.T) {
	t.Parallel()
	home, dir := t.TempDir(), t.TempDir()
	repo := filepath.Join(home, "src", "app")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".claude"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".claude", "version.txt"), []byte("1\n"), 0o600))
	writeFile(t, dir, settings.FileName, `schema_version = 1

[agent]
model = "opus"

[groups.coders]
workers = 2
slots = 4

[projects.app]
repo = "~/src/app"
color = "#7C3AED"
github = "nerds-run/app"
verify = [["task", "check"], ["go", "test", "./..."]]

[projects.app.agent]
command = ["/usr/local/bin/claude", "--setting-sources", "user,project,local"]

[projects.app.groups.qa]
workers = 0
slots = 1

[projects.app.integrations.github]
token = { env = "GITHUB_TOKEN" }

[projects.app.integrations.linear]
url = "https://linear.app/nerds-run"
`, "")

	got, err := settings.Load(settings.LoadOptions{
		ConfigDir: dir, Project: "app", LookupEnv: envOf(map[string]string{"HOME": home}),
	})
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "app", got.Project)
	assert.Equal(t, []string{"app"}, got.Projects)
	assert.Equal(t, settings.Profile{
		Name:   "app",
		Repo:   repo,
		Color:  "#7C3AED",
		GitHub: "nerds-run/app",
		Agent: settings.Agent{
			Command: []string{"/usr/local/bin/claude", "--setting-sources", "user,project,local"},
			Model:   "opus",
		},
		Groups: map[string]settings.Group{
			"coders": {Workers: 2, Slots: 4},
			"qa":     {Workers: 0, Slots: 1},
		},
		Verify: [][]string{{"task", "check"}, {"go", "test", "./..."}},
		Integrations: map[string]settings.Integration{
			"github": {Token: &settings.SecretRef{Env: "GITHUB_TOKEN"}},
			"linear": {URL: "https://linear.app/nerds-run"},
		},
		Rotation: settings.Rotation{Thresholds: []int{55, 65, 75}},
	}, *got.Profile)
	l, ok := leaf(t, got, "repo")
	require.True(t, ok)
	assert.Equal(t, repo, l.Value, "Values carries the expanded repo")
}

// TestLoadWithoutAProject resolves defaults, the shared tables and --set
// only, and lists the projects.
func TestLoadWithoutAProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, settings.FileName, `schema_version = 1

[agent]
model = "opus"

[projects.zeta]
repo = "/nonexistent/zeta"
color = "#000000"

[projects.alpha]
repo = "/nonexistent/alpha"
`, "")

	got, err := settings.Load(settings.LoadOptions{
		ConfigDir: dir, Set: []string{"rotation.thresholds=[10,20,30]"}, LookupEnv: envOf(nil),
	})
	require.NoError(t, err)
	assert.Empty(t, got.Project)
	assert.Nil(t, got.Profile)
	assert.Equal(t, []string{"alpha", "zeta"}, got.Projects)
	assert.Equal(t, map[string]string{
		"agent.command":       settings.LayerDefault,
		"agent.model":         settings.LayerShared,
		"rotation.thresholds": settings.LayerFlag,
	}, got.Sources, "no project's values leak into a no-project resolve")
	var paths []string
	for _, l := range got.Leaves() {
		paths = append(paths, l.Path)
	}
	assert.Equal(t, []string{"agent.command", "agent.model", "rotation.thresholds"}, paths, "leaves are sorted")
}

// TestLoadMissingFilesAreNotErrors: with no shared file only the defaults
// apply, and a project with no repo file resolves without one.
func TestLoadMissingFilesAreNotErrors(t *testing.T) {
	t.Parallel()

	got, err := settings.Load(settings.LoadOptions{ConfigDir: t.TempDir(), LookupEnv: envOf(nil)})
	require.NoError(t, err)
	assert.NotNil(t, got.Projects, "an empty list, not null, so --json prints []")
	assert.Empty(t, got.Projects)
	assert.Equal(t, map[string]string{
		"agent.command":       settings.LayerDefault,
		"rotation.thresholds": settings.LayerDefault,
	}, got.Sources)

	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n", repo)
	got, err = settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.NoError(t, err)
	assert.Equal(t, repo, got.Profile.Repo)
}

// TestLoadChecksEveryProfileWithoutAProject: the field rules and the D12
// command checks apply to the shared tables and to every project even when
// no project is selected, so `config resolve` alone lints the whole file.
// Only the repo checks, which touch the filesystem, wait for a selection.
func TestLoadChecksEveryProfileWithoutAProject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		shared  string
		set     []string
		wantErr error
		wantIn  string
	}{
		"bad shared colour":       {shared: "color = \"red\"\n", wantErr: settings.ErrInvalid, wantIn: "color"},
		"shared agent not claude": {shared: "[agent]\ncommand = [\"aider\"]\n", wantErr: settings.ErrBypassesAgentisan, wantIn: "agent.command"},
		"bad shared thresholds":   {shared: "[rotation]\nthresholds = [1, 2]\n", wantErr: settings.ErrInvalid, wantIn: "rotation.thresholds"},
		"unknown key, project":    {shared: "[projects.app]\nbogus = 1\n", wantErr: settings.ErrUnknownKey, wantIn: "projects.app.bogus"},
		"project agent not claude": {
			shared:  "[projects.app]\nrepo = \"/nonexistent\"\n[projects.app.agent]\ncommand = [\"aider\"]\n",
			wantErr: settings.ErrBypassesAgentisan, wantIn: "projects.app.agent.command",
		},
		"project slots below shared workers": {
			shared:  "[groups.coders]\nworkers = 3\nslots = 4\n[projects.app]\n[projects.app.groups.coders]\nslots = 2\n",
			wantErr: settings.ErrInvalid, wantIn: "projects.app.groups.coders.slots",
		},
		"--set breaks a project": {
			shared: "[projects.app]\n", set: []string{"color=red"},
			wantErr: settings.ErrInvalid, wantIn: "--set",
		},
		"an empty projects table": {shared: "[projects]\n"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.shared, "")
			got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Set: tc.set, LookupEnv: envOf(nil)})
			if tc.wantErr == nil {
				require.NoError(t, err)
				assert.Empty(t, got.Projects)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.Contains(t, err.Error(), tc.wantIn)
		})
	}
}

// TestLoadErrors has a row for every sentinel and every field rule. Each
// message must name the file or layer and the key path, and none may contain
// a secret.
func TestLoadErrors(t *testing.T) {
	t.Parallel()

	const app = "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n"
	tests := map[string]struct {
		shared    string // full shared file; empty means no file
		emptyFile bool   // write the shared file with zero bytes
		repoFile  string // full repo file; empty means no file
		emptyRepo bool   // write the repo file with zero bytes
		project   string // defaults to app
		set       []string
		env       map[string]string
		noBundle  bool
		noDir     bool
		wantErr   error
		wantIn    []string
		wantNotIn []string
	}{
		"no config dir": {
			noDir: true, wantErr: settings.ErrNoConfigDir,
		},
		"an empty shared file": {
			emptyFile: true, wantErr: settings.ErrUnsupportedSchema, wantIn: []string{settings.FileName, "missing"},
		},
		"an empty repo file": {
			shared: app, emptyRepo: true, wantErr: settings.ErrUnsupportedSchema, wantIn: []string{settings.RepoFileName},
		},
		"a project name with a NUL": {
			shared:  app + "[projects.\"a\\u0000b\"]\nrepo = \"/x\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.a"},
		},
		"a key with a NUL": {
			shared:  app + "\"col\\u0000or\" = 1\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"projects.app.col"},
		},
		"a group name with non-ASCII": {
			shared:  app + "[projects.app.groups.\"qä\"]\nworkers = 1\nslots = 1\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"groups.qä"},
		},
		"a group name like an option": {
			shared:  app + "[projects.app.groups.\"-x\"]\nworkers = 1\nslots = 1\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"groups.-x"},
		},
		"a project name that is not UTF-8": {
			shared: app, project: "app\xff", wantErr: settings.ErrNoProject,
		},
		"selecting a name with a space": {
			shared: app, project: "a b", wantErr: settings.ErrNoProject, wantIn: []string{"a b"},
		},
		"--set key in another case": {
			shared: app, set: []string{"Agent.model=opus"},
			wantErr: settings.ErrUnknownKey, wantIn: []string{"Agent", "--set"},
		},
		"repo empty": {
			shared:  "schema_version = 1\n[projects.app]\nrepo = \"\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"repo", "absolute"},
		},
		"four thresholds": {
			shared:  app + "[projects.app.rotation]\nthresholds = [40, 50, 60, 70]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"rotation.thresholds"},
		},
		"workers past the int64 range": {
			shared:  app + "[projects.app.groups.coders]\nworkers = 9223372036854775808\nslots = 1\n",
			wantErr: settings.ErrInvalid, wantIn: []string{settings.FileName, "line 5"},
		},
		"a key in another case": {
			shared:  app + "Color = \"#123456\"\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"projects.app.Color"},
		},
		"a project name with non-ASCII": {
			shared:  app + "[projects.\"café\"]\nrepo = \"/x\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.café"},
		},
		"a project name with a space": {
			shared:  app + "[projects.\"a b\"]\nrepo = \"/x\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.a b"},
		},
		"a project name like an option": {
			shared:  app + "[projects.\"-x\"]\nrepo = \"/x\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.-x"},
		},
		"a group name with a dot": {
			shared:  app + "[projects.app.groups.\"a.b\"]\nworkers = 1\nslots = 1\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"groups.a.b"},
		},
		"an integration name in another case": {
			shared:  app + "[projects.app.integrations.GitHub]\nurl = \"https://github.com\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"integrations.GitHub"},
		},
		"a project named like a flag": {
			shared: app, project: "--help", wantErr: settings.ErrNoProject, wantIn: []string{"--help"},
		},
		"a project name in another case": {
			shared: app, project: "APP", wantErr: settings.ErrNoProject, wantIn: []string{"APP"},
		},
		"a non-ASCII project name": {
			shared: app, project: "äpp", wantErr: settings.ErrNoProject, wantIn: []string{"äpp"},
		},
		"repo relative with ..": {
			shared:  "schema_version = 1\n[projects.app]\nrepo = \"../app\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"repo", "absolute"},
		},
		"shared schema_version missing": {
			shared:  "[projects.app]\nrepo = \"REPO\"\n",
			wantErr: settings.ErrUnsupportedSchema, wantIn: []string{settings.FileName, "schema_version"},
		},
		"shared schema_version 2": {
			shared:  "schema_version = 2\n[projects.app]\nrepo = \"REPO\"\nfuture_key = 1\n",
			wantErr: settings.ErrUnsupportedSchema, wantIn: []string{settings.FileName, "2"},
		},
		"shared schema_version a string": {
			shared:  "schema_version = \"1\"\n[projects.app]\nrepo = \"REPO\"\n",
			wantErr: settings.ErrUnsupportedSchema, wantIn: []string{settings.FileName},
		},
		"repo file schema_version missing": {
			shared: app, repoFile: "color = \"#123456\"\n",
			wantErr: settings.ErrUnsupportedSchema, wantIn: []string{settings.RepoFileName},
		},
		"repo file schema_version 2": {
			shared: app, repoFile: "schema_version = 2\n",
			wantErr: settings.ErrUnsupportedSchema, wantIn: []string{settings.RepoFileName},
		},
		"unknown key in the shared tables": {
			shared:  "schema_version = 1\n[agent]\nmodle = \"opus\"\n[projects.app]\nrepo = \"REPO\"\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"agent.modle", settings.LayerShared, settings.FileName},
		},
		"unknown key in the selected project": {
			shared:  app + "[projects.app.agent]\nmodle = \"opus\"\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"projects.app.agent.modle", settings.LayerProject, settings.FileName},
		},
		"unknown key in another project": {
			shared:  app + "[projects.other]\nrepo = \"/x\"\nbogus = 1\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"projects.other.bogus"},
		},
		"a project cannot set its own name": {
			shared:  app + "name = \"app\"\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"projects.app.name"},
		},
		"unknown key in the repo file": {
			shared: app, repoFile: "schema_version = 1\n[agent]\nmodle = \"opus\"\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"agent.modle", settings.LayerRepo, settings.RepoFileName},
		},
		"the repo file cannot move the repo": {
			shared: app, repoFile: "schema_version = 1\nrepo = \"/elsewhere\"\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"repo", settings.RepoFileName},
		},
		"unknown key in --set": {
			shared: app, set: []string{"agent.modle=opus"},
			wantErr: settings.ErrUnknownKey, wantIn: []string{"agent.modle", "--set"},
		},
		"a token given as a plain string": {
			shared:  app + "[projects.app.integrations.github]\ntoken = \"" + plainSecret + "\"\n",
			wantErr: settings.ErrSecretValue, wantIn: []string{"projects.app.integrations.github.token", settings.FileName},
		},
		"a token in the shared tables given as a plain string": {
			shared:  "schema_version = 1\n[integrations.github]\ntoken = \"" + plainSecret + "\"\n",
			project: "-", wantErr: settings.ErrSecretValue, wantIn: []string{"integrations.github.token", settings.LayerShared},
		},
		"an api_key is a secret, not an unknown key": {
			shared:  app + "[projects.app.integrations.linear]\napi_key = \"" + plainSecret + "\"\n",
			wantErr: settings.ErrSecretValue, wantIn: []string{"projects.app.integrations.linear.api_key"},
		},
		"a password given as a number": {
			shared:  app + "[projects.app.integrations.db]\npassword = 1234\n",
			wantErr: settings.ErrSecretValue, wantIn: []string{"projects.app.integrations.db.password"},
		},
		"a secret nested under an unknown key": {
			shared:  app + "[projects.app.integrations.db.extra]\nsecret = \"" + plainSecret + "\"\n",
			wantErr: settings.ErrSecretValue, wantIn: []string{"projects.app.integrations.db.extra.secret"},
		},
		"a secret inside an array of tables": {
			shared:  app + "[[projects.app.integrations.db.hosts]]\npassword = \"" + plainSecret + "\"\n",
			wantErr: settings.ErrSecretValue, wantIn: []string{"projects.app.integrations.db.hosts.password"},
		},
		"a token in the repo file given as a plain string": {
			shared: app, repoFile: "schema_version = 1\n[integrations.github]\ntoken = \"" + plainSecret + "\"\n",
			wantErr: settings.ErrSecretValue, wantIn: []string{"integrations.github.token", settings.RepoFileName},
		},
		"a token given in --set as a plain string": {
			shared: app, set: []string{"integrations.github.token=" + plainSecret},
			wantErr: settings.ErrSecretValue, wantIn: []string{"integrations.github.token", "--set"},
		},
		"an unquoted token is a syntax error that does not echo it": {
			shared:  app + "[projects.app.integrations.github]\ntoken = " + plainSecret + "\n",
			wantErr: settings.ErrInvalid, wantIn: []string{settings.FileName, "line 5"},
		},
		"a valid reference under a name the schema does not know": {
			shared:  app + "[projects.app.integrations.linear]\napi_key = { env = \"LINEAR_API_KEY\" }\n",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"projects.app.integrations.linear.api_key"},
		},
		"colour is not #rrggbb": {
			shared:  app + "color = \"red\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"color", settings.LayerProject, settings.FileName},
		},
		"colour of the wrong type": {
			shared:  app + "color = 5\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.color", "string"},
		},
		"colour from --set names the flag": {
			shared: app, set: []string{"color=blue"},
			wantErr: settings.ErrInvalid, wantIn: []string{"color", "--set"},
		},
		"github is not owner/name": {
			shared:  app + "github = \"just-a-name\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"github"},
		},
		"two thresholds": {
			shared:  app + "[projects.app.rotation]\nthresholds = [50, 60]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"rotation.thresholds"},
		},
		"thresholds not ascending": {
			shared:  app + "[projects.app.rotation]\nthresholds = [60, 50, 70]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"rotation.thresholds"},
		},
		"thresholds repeat": {
			shared:  app + "[projects.app.rotation]\nthresholds = [50, 50, 70]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"rotation.thresholds"},
		},
		"threshold below 1": {
			shared:  app + "[projects.app.rotation]\nthresholds = [0, 50, 70]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"rotation.thresholds"},
		},
		"threshold above 100": {
			shared:  app + "[projects.app.rotation]\nthresholds = [50, 60, 101]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"rotation.thresholds"},
		},
		"threshold of the wrong type": {
			shared:  app + "[projects.app.rotation]\nthresholds = [50, 60, \"70\"]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"rotation.thresholds"},
		},
		"slots below workers": {
			shared:  app + "[projects.app.groups.coders]\nworkers = 3\nslots = 2\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"groups.coders.slots", settings.LayerProject},
		},
		"negative workers": {
			shared:  app + "[projects.app.groups.coders]\nworkers = -1\nslots = 2\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"groups.coders.workers"},
		},
		"slots not set in any layer": {
			shared:  app + "[projects.app.groups.coders]\nworkers = 1\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"groups.coders.slots"},
		},
		"group name breaks the alphabet": {
			shared:  app + "[projects.app.groups.Coders]\nworkers = 1\nslots = 1\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.groups.Coders"},
		},
		"project name breaks the alphabet": {
			shared:  app + "[projects.Bad_Name]\nrepo = \"/x\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.Bad_Name"},
		},
		"projects is not a table": {
			shared:  "schema_version = 1\nprojects = 3\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects"},
		},
		"a project that is not a table": {
			shared:  "schema_version = 1\n[projects]\napp = 3\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.app", "table"},
		},
		"a verify command is empty": {
			shared:  app + "verify = [[\"task\", \"check\"], []]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"verify"},
		},
		"a verify command is not an argv list": {
			shared:  app + "verify = [\"task check\"]\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.verify", "item 1"},
		},
		"repo is a file": {
			shared:  "schema_version = 1\n[projects.app]\nrepo = \"REPO/.claude/version.txt\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"repo", "not a directory"},
		},
		"repo not set": {
			shared:  "schema_version = 1\n[projects.app]\ncolor = \"#123456\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"repo"},
		},
		"repo relative": {
			shared:  "schema_version = 1\n[projects.app]\nrepo = \"src/app\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"repo", "absolute"},
		},
		"repo does not exist": {
			shared:  "schema_version = 1\n[projects.app]\nrepo = \"REPO/missing\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"repo"},
		},
		"repo under ~ with HOME unset": {
			shared:  "schema_version = 1\n[projects.app]\nrepo = \"~/src/app\"\n",
			wantErr: settings.ErrInvalid, wantIn: []string{"repo", "HOME"},
		},
		"toml syntax error names the file and line": {
			shared:  "schema_version = 1\n[projects.app\n",
			wantErr: settings.ErrInvalid, wantIn: []string{settings.FileName, "line 2"},
		},
		"repo file syntax error": {
			shared: app, repoFile: "schema_version = 1\ncolor = \n",
			wantErr: settings.ErrInvalid, wantIn: []string{settings.RepoFileName},
		},
		"unknown project": {
			shared: app, project: "nope",
			wantErr: settings.ErrNoProject, wantIn: []string{"nope", "app"},
		},
		"unknown project and no shared file": {
			project: "nope", wantErr: settings.ErrNoProject, wantIn: []string{"nope"},
		},
		"agent is not claude or codex": {
			shared:  app + "[projects.app.agent]\ncommand = [\"aider\"]\n",
			wantErr: settings.ErrBypassesAgentisan, wantIn: []string{"agent.command", settings.LayerProject},
		},
		"agent command emptied": {
			shared:  app + "[projects.app.agent]\ncommand = []\n",
			wantErr: settings.ErrBypassesAgentisan, wantIn: []string{"agent.command"},
		},
		"agent --bare from --set": {
			shared: app, set: []string{`agent.command=["claude","--bare"]`},
			wantErr: settings.ErrBypassesAgentisan, wantIn: []string{"agent.command", "--set"},
		},
		"no Agentisan bundle in the repo": {
			shared: app, noBundle: true,
			wantErr: settings.ErrBypassesAgentisan, wantIn: []string{settings.BundleMarker},
		},
		"--set without =": {
			shared: app, set: []string{"color"},
			wantErr: settings.ErrInvalid, wantIn: []string{"--set"},
		},
		"--set with an empty key segment": {
			shared: app, set: []string{"agent..model=opus"},
			wantErr: settings.ErrInvalid, wantIn: []string{"agent..model"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			if tc.noBundle {
				repo = t.TempDir()
			}
			if tc.shared != "" || tc.emptyFile {
				writeFile(t, dir, settings.FileName, tc.shared, repo)
			}
			if tc.repoFile != "" || tc.emptyRepo {
				writeFile(t, repo, settings.RepoFileName, tc.repoFile, repo)
			}
			opts := settings.LoadOptions{ConfigDir: dir, Project: "app", Set: tc.set, LookupEnv: envOf(tc.env)}
			switch tc.project {
			case "":
			case "-":
				opts.Project = ""
			default:
				opts.Project = tc.project
			}
			if tc.noDir {
				opts.ConfigDir = ""
			}

			_, err := settings.Load(opts)
			require.ErrorIs(t, err, tc.wantErr)
			for _, s := range tc.wantIn {
				assert.Contains(t, err.Error(), s)
			}
			assert.NotContains(t, err.Error(), plainSecret, "an error must never echo a secret")
		})
	}
}

func TestExpandHome(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		path    string
		env     map[string]string
		want    string
		wantErr error
	}{
		"bare ~":                  {path: "~", env: map[string]string{"HOME": "/home/u"}, want: "/home/u"},
		"~/ prefix":               {path: "~/src/app", env: map[string]string{"HOME": "/home/u"}, want: "/home/u/src/app"},
		"HOME with a trailing /":  {path: "~/src", env: map[string]string{"HOME": "/home/u/"}, want: "/home/u/src"},
		"absolute path untouched": {path: "/srv/app", want: "/srv/app"},
		"HOME with non-ASCII":     {path: "~/src", env: map[string]string{"HOME": "/home/zoë"}, want: "/home/zoë/src"},
		"HOME with a space":       {path: "~/src", env: map[string]string{"HOME": "/home/my user"}, want: "/home/my user/src"},
		"relative path untouched": {path: "src/app", want: "src/app"},
		"empty path untouched":    {path: "", want: ""},
		"~user is not expanded":   {path: "~bob/app", env: map[string]string{"HOME": "/home/u"}, want: "~bob/app"},
		"HOME unset":              {path: "~/app", wantErr: settings.ErrInvalid},
		"HOME empty":              {path: "~/app", env: map[string]string{"HOME": ""}, wantErr: settings.ErrInvalid},
		"HOME relative":           {path: "~/app", env: map[string]string{"HOME": "home"}, wantErr: settings.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := settings.ExpandHome(tc.path, envOf(tc.env))
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseSet(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		arg       string
		wantPath  string
		wantValue any
		wantErr   error
	}{
		"integer":                      {arg: "groups.coders.workers=3", wantPath: "groups.coders.workers", wantValue: int64(3)},
		"quoted string":                {arg: `agent.model="opus"`, wantPath: "agent.model", wantValue: "opus"},
		"bare word is a string":        {arg: "agent.model=opus", wantPath: "agent.model", wantValue: "opus"},
		"# is not a comment":           {arg: "color=#123456", wantPath: "color", wantValue: "#123456"},
		"array without spaces":         {arg: "rotation.thresholds=[50,60,70]", wantPath: "rotation.thresholds", wantValue: []any{int64(50), int64(60), int64(70)}},
		"inline table":                 {arg: `integrations.x.token={ env = "X" }`, wantPath: "integrations.x.token", wantValue: map[string]any{"env": "X"}},
		"splits on the first =":        {arg: "agent.model=a=b", wantPath: "agent.model", wantValue: "a=b"},
		"empty value":                  {arg: "agent.model=", wantPath: "agent.model", wantValue: ""},
		"a second key is a string":     {arg: "agent.model=1\ncolor = 2", wantPath: "agent.model", wantValue: "1\ncolor = 2"},
		"spaces around the path":       {arg: " agent.model =opus", wantPath: "agent.model", wantValue: "opus"},
		"a path like an option":        {arg: "-x=1", wantPath: "-x", wantValue: int64(1)},
		"a NUL in a plain value":       {arg: "agent.model=a\x00b", wantPath: "agent.model", wantValue: "a\x00b"},
		"a NUL escape":                 {arg: `agent.model="a\u0000b"`, wantPath: "agent.model", wantValue: "a\x00b"},
		"a value that is not UTF-8":    {arg: "agent.model=x\xffy", wantErr: settings.ErrInvalid},
		"a value ending mid-character": {arg: "agent.model=caf\xc3", wantErr: settings.ErrInvalid},
		"a non-ASCII path segment":     {arg: "agent.modèl=x", wantErr: settings.ErrInvalid},
		"no =":                         {arg: "agent.model", wantErr: settings.ErrInvalid},
		"empty path":                   {arg: "=opus", wantErr: settings.ErrInvalid},
		"empty segment":                {arg: "agent..model=opus", wantErr: settings.ErrInvalid},
		"leading dot":                  {arg: ".model=opus", wantErr: settings.ErrInvalid},
		"segment outside bare keys":    {arg: "agent.mo del=opus", wantErr: settings.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, value, err := settings.ParseSet(tc.arg)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, path)
			assert.Equal(t, tc.wantValue, value)
		})
	}
}

func TestFormatValue(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value any
		want  string
	}{
		"string":                    {value: "opus", want: `"opus"`},
		"quotes, backslash":         {value: `a "q" \ b`, want: `"a \"q\" \\ b"`},
		"whitespace escapes":        {value: "t\tn\nr\r", want: `"t\tn\nr\r"`},
		"control character":         {value: "a\x01b", want: `"a\u0001b"`},
		"unicode kept":              {value: "café", want: `"café"`},
		"NUL":                       {value: "a\x00b", want: `"a\u0000b"`},
		"DEL":                       {value: "\x7f", want: `"\u007F"`},
		"invalid UTF-8":             {value: "a\xffb", want: "\"a\uFFFDb\""},
		"a truncated sequence":      {value: "caf\xc3", want: "\"caf\uFFFD\""},
		"a byte-order mark is kept": {value: "\ufeffx", want: "\"\ufeffx\""},
		"empty string":              {value: "", want: `""`},
		"empty table":               {value: map[string]any{}, want: "{}"},
		"key that isn't bare":       {value: map[string]any{"a b": 1}, want: `{ "a b" = 1 }`},
		"int":                       {value: 3, want: "3"},
		"int64":                     {value: int64(-4), want: "-4"},
		"bool":                      {value: true, want: "true"},
		"ints":                      {value: []int{55, 65, 75}, want: "[55, 65, 75]"},
		"strings":                   {value: []string{"claude", "--model"}, want: `["claude", "--model"]`},
		"empty list":                {value: []string{}, want: "[]"},
		"argv lists":                {value: [][]string{{"task", "check"}, {"go"}}, want: `[["task", "check"], ["go"]]`},
		"env ref":                   {value: settings.SecretRef{Env: "GITHUB_TOKEN"}, want: `{ env = "GITHUB_TOKEN" }`},
		"op ref":                    {value: settings.SecretRef{Op: "op://V/I/f"}, want: `{ op = "op://V/I/f" }`},
		"table, sorted":             {value: map[string]any{"b": 1, "a": "x"}, want: `{ a = "x", b = 1 }`},
		"any list":                  {value: []any{int64(1), "x"}, want: `[1, "x"]`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, settings.FormatValue(tc.value))
		})
	}
}

// utf16le encodes ASCII text as UTF-16LE, the way some editors save it.
func utf16le(s string) string {
	var b strings.Builder
	for i := range len(s) {
		b.WriteByte(s[i])
		b.WriteByte(0)
	}
	return b.String()
}

// TestLoadEncodingClasses: both files must be UTF-8 TOML. Anything else is
// ErrInvalid naming the file; line framing TOML allows (CRLF, no final
// newline) loads; a NUL a TOML escape lets through still meets the field
// rules.
func TestLoadEncodingClasses(t *testing.T) {
	t.Parallel()

	const app = "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n"
	tests := map[string]struct {
		shared   string
		repoFile string
		wantErr  error
		wantIn   string
		check    func(t *testing.T, r settings.Resolved)
	}{
		"invalid UTF-8 in a string":           {shared: app + "color = \"x\xffy\"\n", wantErr: settings.ErrInvalid, wantIn: settings.FileName},
		"invalid UTF-8 in a comment":          {shared: app + "# x\xff\n", wantErr: settings.ErrInvalid, wantIn: settings.FileName},
		"a Latin-1 byte in a UTF-8 file":      {shared: app + "[projects.app.agent]\nmodel = \"caf\xe9\"\n", wantErr: settings.ErrInvalid},
		"a Latin-1 byte in a quoted key":      {shared: app + "[projects.\"caf\xe9\"]\n", wantErr: settings.ErrInvalid},
		"a truncated sequence at end of file": {shared: app + "# caf\xc3", wantErr: settings.ErrInvalid},
		"a byte-order mark":                   {shared: "\xef\xbb\xbf" + app, wantErr: settings.ErrInvalid, wantIn: "line 1"},
		"UTF-16 encoded":                      {shared: utf16le(app), wantErr: settings.ErrInvalid},
		"a lone surrogate escape":             {shared: app + "[projects.app.agent]\nmodel = \"\\uD800\"\n", wantErr: settings.ErrInvalid},
		"a raw NUL":                           {shared: app + "color = \"#12\x003456\"\n", wantErr: settings.ErrInvalid},
		"a NUL escape meets the colour rule":  {shared: app + "color = \"#12\\u00003456\"\n", wantErr: settings.ErrInvalid, wantIn: "color"},
		"a NUL escape in the repo path":       {shared: "schema_version = 1\n[projects.app]\nrepo = \"REPO\\u0000x\"\n", wantErr: settings.ErrInvalid, wantIn: "repo"},
		"a lone CR":                           {shared: strings.ReplaceAll(app, "\n", "\r"), wantErr: settings.ErrInvalid},
		"CRLF line endings": {
			shared: strings.ReplaceAll(app, "\n", "\r\n") + "color = \"#123456\"\r\n",
			check:  func(t *testing.T, r settings.Resolved) { assert.Equal(t, "#123456", r.Profile.Color) },
		},
		"no final newline": {
			shared: app + "color = \"#123456\"",
			check:  func(t *testing.T, r settings.Resolved) { assert.Equal(t, "#123456", r.Profile.Color) },
		},
		"non-ASCII values pass through": {
			shared: app + "[projects.app.agent]\nmodel = \"opus-café\"\n",
			check:  func(t *testing.T, r settings.Resolved) { assert.Equal(t, "opus-café", r.Profile.Agent.Model) },
		},
		"invalid UTF-8 in the repo file": {
			shared: app, repoFile: "schema_version = 1\ncolor = \"x\xff\"\n", wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName,
		},
		"repo file: a truncated sequence":    {shared: app, repoFile: "schema_version = 1\n# caf\xc3", wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName},
		"repo file: a byte-order mark":       {shared: app, repoFile: "\xef\xbb\xbfschema_version = 1\n", wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName},
		"repo file: UTF-16 encoded":          {shared: app, repoFile: utf16le("schema_version = 1\n"), wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName},
		"repo file: a lone surrogate escape": {shared: app, repoFile: "schema_version = 1\n[agent]\nmodel = \"\\uD800\"\n", wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName},
		"repo file: a Latin-1 byte":          {shared: app, repoFile: "schema_version = 1\n[agent]\nmodel = \"caf\xe9\"\n", wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName},
		"repo file: a raw NUL":               {shared: app, repoFile: "schema_version = 1\ncolor = \"#12\x003456\"\n", wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName},
		"repo file: a lone CR":               {shared: app, repoFile: "schema_version = 1\rcolor = \"#abcdef\"\r", wantErr: settings.ErrInvalid, wantIn: settings.RepoFileName},
		"repo file: no final newline": {
			shared: app, repoFile: "schema_version = 1\ncolor = \"#abcdef\"",
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, settings.LayerRepo, r.Sources["color"]) },
		},
		"a CRLF repo file": {
			shared: app, repoFile: "schema_version = 1\r\ncolor = \"#abcdef\"\r\n",
			check: func(t *testing.T, r settings.Resolved) { assert.Equal(t, settings.LayerRepo, r.Sources["color"]) },
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, tc.shared, repo)
			if tc.repoFile != "" {
				writeFile(t, repo, settings.RepoFileName, tc.repoFile, repo)
			}
			got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.wantIn)
				return
			}
			require.NoError(t, err)
			tc.check(t, got)
		})
	}
}

// TestLoadNestingDepth: depth costs time in proportion to the file's size,
// never its square, up to the deepest file MaxFileBytes allows; arrays and
// inline tables stop at the decoder's limit of 10000 levels.
func TestLoadNestingDepth(t *testing.T) {
	t.Parallel()

	const app = "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n"
	const table = "[projects.app.integrations.x"
	capDepth := (settings.MaxFileBytes - len(app) - len(table) - len("]\nb = 1\n") - 200) / len(".a")
	tests := map[string]struct {
		shared   string
		repoFile string
		wantErr  error
		wantIn   string
	}{
		"a repo file nested 100000 tables deep": {
			shared: app, repoFile: "schema_version = 1\n[integrations.x" + strings.Repeat(".a", 100000) + "]\nb = 1\n",
			wantErr: settings.ErrUnknownKey, wantIn: "integrations.x.a (repo",
		},
		"tables nested as deep as MaxFileBytes allows": {
			shared:  app + table + strings.Repeat(".a", capDepth) + "]\nb = 1\n",
			wantErr: settings.ErrUnknownKey, wantIn: "projects.app.integrations.x.a ",
		},
		"a secret 1000 tables deep": {
			shared:  app + table + strings.Repeat(".a", 1000) + "]\npassword = \"" + plainSecret + "\"\n",
			wantErr: settings.ErrSecretValue, wantIn: ".a.password",
		},
		"arrays nested to the decoder's limit": {
			shared:  app + "verify = " + strings.Repeat("[", 10000) + strings.Repeat("]", 10000) + "\n",
			wantErr: settings.ErrInvalid, wantIn: "projects.app.verify",
		},
		"arrays nested one past the decoder's limit": {
			shared:  app + "verify = " + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + "\n",
			wantErr: settings.ErrInvalid, wantIn: "nested",
		},
		"inline tables nested past the decoder's limit": {
			shared:  app + "[projects.app.integrations.x]\ntoken = " + strings.Repeat("{a=", 10001) + "1" + strings.Repeat("}", 10001) + "\n",
			wantErr: settings.ErrInvalid, wantIn: "nested",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, tc.shared, repo)
			if tc.repoFile != "" {
				writeFile(t, repo, settings.RepoFileName, tc.repoFile, repo)
			}
			done := make(chan error, 1)
			go func() {
				_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
				done <- err
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.wantIn)
				assert.NotContains(t, err.Error(), plainSecret)
			case <-time.After(20 * time.Second):
				t.Fatal("Load's cost grows faster than the file")
			}
		})
	}
}

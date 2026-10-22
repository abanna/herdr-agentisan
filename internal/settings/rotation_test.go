package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestRotationsReadEveryLayer: a project's thresholds and each group's merge
// from the defaults, the shared tables, the project's own table and its repo
// file (A46), the boss and a group that sets none have the project's, and a
// team the file does not name has the shared ones.
func TestRotationsReadEveryLayer(t *testing.T) {
	t.Parallel()
	repo, otherRepo, dir := newRepo(t), newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, `schema_version = 1
[rotation]
thresholds = [50, 60, 70]
[groups.coder]
workers = 1
slots = 2
rotation = { thresholds = [45, 55, 65] }
[projects.app]
repo = "REPO"
short = "a"
[projects.app.groups.qa]
workers = 1
slots = 1
[projects.other]
repo = "`+otherRepo+`"
short = "o"
[projects.other.rotation]
thresholds = [30, 40, 50]
`, repo)
	writeFile(t, repo, settings.RepoFileName, `schema_version = 1
[groups.qa.rotation]
thresholds = [70, 80, 90]
`, repo)

	got, err := settings.Rotations(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
	require.NoError(t, err)
	assert.Empty(t, got.Unread)
	app := got.For("app")
	assert.Equal(t, []int{70, 80, 90}, app.For("qa"), "the repo file's, for its group")
	assert.Equal(t, []int{45, 55, 65}, app.For("coder"), "the shared group's")
	assert.Equal(t, []int{50, 60, 70}, app.For(""), "the boss has the project's")
	assert.Equal(t, []int{50, 60, 70}, app.For("research"), "a group that sets none has the project's")
	other := got.For("other")
	assert.Equal(t, []int{30, 40, 50}, other.For(""), "the project's own table")
	assert.Equal(t, []int{30, 40, 50}, other.For("qa"), "another project's repo file is not this one's")
	assert.Equal(t, []int{50, 60, 70}, got.For("unnamed").For(""), "a team the file does not name has the shared tables'")
	assert.Equal(t, []int{45, 55, 65}, got.For("unnamed").For("coder"))
	assert.Equal(t, []int{50, 60, 70}, app.For("Coder"), "a name is its exact bytes: Coder is not coder")
	assert.Equal(t, []int{50, 60, 70}, app.For("cödërs ◆"), "a name no layer sets, however spelled, has the project's")
}

// TestRotationsExpandTheRepo: a repo under ~ is found through HOME, as Load
// finds it, and with no HOME, or a relative repo, its repo file is unread.
func TestRotationsExpandTheRepo(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	repo := filepath.Join(home, "r")
	require.NoError(t, os.Mkdir(repo, 0o700))
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n[rotation]\nthresholds = [20, 30, 40]\n", "")
	for name, tc := range map[string]struct {
		repo   string
		env    map[string]string
		want   []int
		wantIn string
	}{
		"under HOME":    {repo: "~/r", env: map[string]string{"HOME": home}, want: []int{20, 30, 40}},
		"no HOME":       {repo: "~/r", want: []int{55, 65, 75}, wantIn: "HOME"},
		"an empty HOME": {repo: "~/r", env: map[string]string{"HOME": ""}, want: []int{55, 65, 75}, wantIn: "HOME"},
		"relative":      {repo: "r", env: map[string]string{"HOME": home}, want: []int{55, 65, 75}, wantIn: "not an absolute path"},
	} {
		dir := t.TempDir()
		writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \""+tc.repo+"\"\n", "")
		got, err := settings.Rotations(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(tc.env)})
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got.For("app").For(""), name)
		if tc.wantIn == "" {
			assert.Empty(t, got.Unread, name)
			continue
		}
		require.Contains(t, got.Unread, "app", name)
		assert.Contains(t, got.Unread["app"].Error(), tc.wantIn, name)
	}
}

// TestRotationsWithoutAFile: no directory, or no file in it, is the defaults
// for every team.
func TestRotationsWithoutAFile(t *testing.T) {
	t.Parallel()
	for name, dir := range map[string]string{"no directory": "", "no file": t.TempDir()} {
		got, err := settings.Rotations(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
		require.NoError(t, err, name)
		assert.Equal(t, []int{55, 65, 75}, got.For("app").For("coder"), name)
		assert.Empty(t, got.Unread, name)
	}
}

// TestRotationsNeedNoBundle: only the repo is resolved, so a repo without
// the Agentisan bundle, which every workforce op refuses (D12), still gives
// its thresholds.
func TestRotationsNeedNoBundle(t *testing.T) {
	t.Parallel()
	repo, dir := t.TempDir(), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n", repo)
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n[rotation]\nthresholds = [20, 30, 40]\n", repo)

	got, err := settings.Rotations(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
	require.NoError(t, err)
	assert.Empty(t, got.Unread)
	assert.Equal(t, []int{20, 30, 40}, got.For("app").For(""))
}

// TestRotationsKeepTheUsersLayers: a repo file that cannot be read costs that
// project its repo file's thresholds alone, never its own table's, and says
// why in Unread; the other projects are read as ever.
func TestRotationsKeepTheUsersLayers(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		// repoFile is the repo file's content; "" writes none.
		repoFile string
		// noRepo points the project at a repo that does not exist.
		noRepo bool
		wantIn string
	}{
		"no repo":                 {noRepo: true, wantIn: "repo"},
		"an empty file":           {repoFile: " ", wantIn: "schema_version is missing"},
		"a key a repo cannot set": {repoFile: "schema_version = 1\nverify = [[\"x\"]]\n", wantIn: "verify"},
		"a file that does not parse": {
			repoFile: "schema_version = 1\n[rotation\n", wantIn: settings.RepoFileName,
		},
		"thresholds that do not check": {
			repoFile: "schema_version = 1\n[rotation]\nthresholds = [90, 80, 70]\n", wantIn: "rotation.thresholds",
		},
		"a group the merge cannot make": {
			repoFile: "schema_version = 1\n[groups.ghost.rotation]\nthresholds = [10, 20, 30]\n", wantIn: "groups.ghost.workers",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, good, dir := t.TempDir(), newRepo(t), t.TempDir()
			if tc.noRepo {
				repo = filepath.Join(repo, "missing")
			}
			if tc.repoFile != "" {
				require.NoError(t, os.WriteFile(filepath.Join(repo, settings.RepoFileName), []byte(tc.repoFile), 0o600))
			}
			writeFile(t, good, settings.RepoFileName, "schema_version = 1\n[rotation]\nthresholds = [20, 30, 40]\n", good)
			writeFile(t, dir, settings.FileName, `schema_version = 1
[projects.app]
repo = "REPO"
short = "a"
[projects.app.rotation]
thresholds = [35, 45, 85]
[projects.fine]
repo = "`+good+`"
short = "f"
`, repo)

			got, err := settings.Rotations(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
			require.NoError(t, err)
			require.Contains(t, got.Unread, "app")
			assert.Contains(t, got.Unread["app"].Error(), tc.wantIn)
			assert.Equal(t, []int{35, 45, 85}, got.For("app").For(""), "the project's own table still counts")
			assert.NotContains(t, got.Unread, "fine")
			assert.Equal(t, []int{20, 30, 40}, got.For("fine").For(""))
		})
	}
}

// TestRotationsRefuseABrokenFile: a shared file that does not resolve is an
// error, as for every setting the daemon reads at start.
func TestRotationsRefuseABrokenFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[rotation]\nthresholds = [50, 60]\n", "")

	_, err := settings.Rotations(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "rotation.thresholds")
}

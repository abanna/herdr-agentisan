package settings_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

const retiredLayoutHelp = "the ◆ spaces are retired (NERD-5670). Remove this entry. Every team is a tabs team"

func TestSpacesTableIsRetired(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		entry string
		set   []string
	}{
		"empty table":      {entry: "[spaces]\n"},
		"project in table": {entry: "[spaces]\nproject = \"app\"\n"},
		"dotted key":       {entry: "spaces.project = \"app\"\n"},
		"project override": {set: []string{"spaces.project=app"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, repo := t.TempDir(), newRepo(t)
			writeFile(t, dir, settings.FileName,
				"schema_version = 1\n"+tc.entry+"[projects.app]\nrepo = \"REPO\"\nshort = \"ap\"\n", repo)
			for _, project := range []string{"", "app"} {
				_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project, Set: tc.set})
				require.ErrorIs(t, err, settings.ErrInvalid)
				assert.NotErrorIs(t, err, settings.ErrUnknownKey)
				assert.Contains(t, err.Error(), "spaces (")
				assert.Contains(t, err.Error(), retiredLayoutHelp)
			}
			if tc.set == nil {
				teams, err := settings.Teams(dir)
				require.ErrorIs(t, err, settings.ErrInvalid)
				assert.Contains(t, err.Error(), retiredLayoutHelp)
				assert.Empty(t, teams)
			}
		})
	}
}

func TestSpacesLayoutIsRetired(t *testing.T) {
	t.Parallel()

	for name, repoField := range map[string]string{
		"valid repo":   "repo = \"REPO\"\n",
		"missing repo": "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, repo := t.TempDir(), newRepo(t)
			writeFile(t, dir, settings.FileName,
				"schema_version = 1\n[projects.app]\nshort = \"ap\"\nlayout = \"spaces\"\n"+repoField, repo)
			for _, project := range []string{"", "app"} {
				_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project})
				require.ErrorIs(t, err, settings.ErrInvalid)
				assert.Contains(t, err.Error(), "projects.app.layout")
				assert.Contains(t, err.Error(), retiredLayoutHelp)
			}
			teams, err := settings.Teams(dir)
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Contains(t, err.Error(), retiredLayoutHelp)
			assert.Empty(t, teams)
		})
	}
}

func TestEveryProjectNeedsAShortCode(t *testing.T) {
	t.Parallel()

	for name, extra := range map[string]string{
		"sole project": "",
		"two projects": "[projects.other]\nrepo = \"/x\"\nshort = \"ot\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, repo := t.TempDir(), newRepo(t)
			writeFile(t, dir, settings.FileName,
				"schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n"+extra, repo)
			for _, project := range []string{"", "app"} {
				_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project})
				require.ErrorIs(t, err, settings.ErrInvalid)
				assert.Contains(t, err.Error(), "projects.app.short")
				assert.Contains(t, err.Error(), "a tabs project needs a short code")
			}
			teams, err := settings.Teams(dir)
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Contains(t, err.Error(), "a tabs project needs a short code")
			assert.Empty(t, teams)
		})
	}
}

func TestASoleProjectIsATabsTeam(t *testing.T) {
	t.Parallel()

	dir, repo := t.TempDir(), newRepo(t)
	writeFile(t, dir, settings.FileName,
		"schema_version = 1\n[projects.app]\nrepo = \"REPO\"\nshort = \"ap\"\n", repo)
	want := []settings.Team{{Project: "app", Short: "ap", Layout: settings.LayoutTabs}}
	for _, project := range []string{"", "app"} {
		got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project})
		require.NoError(t, err)
		assert.Equal(t, want, got.Teams)
		assert.NotContains(t, got.Values, "spaces")
	}
	teams, err := settings.Teams(dir)
	require.NoError(t, err)
	assert.Equal(t, want, teams)
}

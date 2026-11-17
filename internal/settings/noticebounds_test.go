package settings_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestGitHubLengthBound: github is at most 245 bytes, so that repo#pr fits a
// GitHub notice's 256-byte subject with any PR number up to 2^31-1
// (NERD-5632). The longest is kept and one byte more is ErrTooLong with
// ErrInvalid, from every layer that can set github, with or without a
// selected project, and the message names the layer and the limit. GitHub's
// own longest, a 39-character owner and a 100-character name, is far under.
func TestGitHubLengthBound(t *testing.T) {
	t.Parallel()

	const app = "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n"
	githubs := strings.Repeat("o", 39) + "/" + strings.Repeat("n", 100)
	longest := "o/" + strings.Repeat("n", 243)
	over := longest + "n"
	require.Len(t, longest, 245)
	require.Equal(t, 245, settings.MaxGitHubRepo)

	// layers puts repo in each layer that can set github.
	layers := map[string]func(repo string) (shared, repoFile string, set []string, layer string){
		"the shared file": func(repo string) (string, string, []string, string) {
			return "schema_version = 1\ngithub = \"" + repo + "\"\n[projects.app]\nrepo = \"REPO\"\n", "", nil, settings.LayerShared
		},
		"the project's table": func(repo string) (string, string, []string, string) {
			return app + "github = \"" + repo + "\"\n", "", nil, settings.LayerProject
		},
		"the repo file": func(repo string) (string, string, []string, string) {
			return app, "schema_version = 1\ngithub = \"" + repo + "\"\n", nil, settings.RepoFileName
		},
		"--set": func(repo string) (string, string, []string, string) {
			return app, "", []string{"github=\"" + repo + "\""}, "--set"
		},
	}
	for name, at := range layers {
		for _, project := range []string{"app", ""} {
			if project == "" && name == "the repo file" {
				continue // no project, no repo file
			}
			t.Run(name+" "+project, func(t *testing.T) {
				t.Parallel()
				load := func(value string) (settings.Resolved, error) {
					shared, repoFile, set, _ := at(value)
					repo, dir := newRepo(t), t.TempDir()
					writeFile(t, dir, settings.FileName, shared, repo)
					if repoFile != "" {
						writeFile(t, repo, settings.RepoFileName, repoFile, repo)
					}
					return settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project, Set: set, LookupEnv: envOf(nil)})
				}
				for _, ok := range []string{"a/b", githubs, longest} {
					r, err := load(ok)
					require.NoError(t, err, "%d bytes", len(ok))
					if project != "" {
						assert.Equal(t, ok, r.Profile.GitHub)
					}
				}
				_, err := load(over)
				require.ErrorIs(t, err, settings.ErrInvalid)
				require.ErrorIs(t, err, settings.ErrTooLong)
				_, _, _, layer := at(over)
				assert.Contains(t, err.Error(), layer)
				assert.Contains(t, err.Error(), "github is at most 245 bytes, got 246")
			})
		}
	}
}

// TestGitHubPatternIsNotTooLong: a github value that is not owner/name is
// ErrInvalid alone, whatever its length, and the pattern is checked first:
// ErrTooLong is only for an owner/name too long. Each value is TOML source.
func TestGitHubPatternIsNotTooLong(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"", "just-a-name", strings.Repeat("n", 300), "-o/r", "o/r n", "o/r\\u0000",
		"o/" + strings.Repeat("é", 200), // 402 bytes, 202 characters
	} {
		repo, dir := newRepo(t), t.TempDir()
		writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\ngithub = \""+value+"\"\n", repo)
		_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
		require.ErrorIs(t, err, settings.ErrInvalid)
		assert.NotErrorIs(t, err, settings.ErrTooLong)
		assert.Contains(t, err.Error(), "is not owner/name")
	}
}

// TestProjectNameLengthBound: a project's name is at most 256 bytes, a
// GitHub notice's project field (NERD-5632). The longest loads, selected or
// not, and one byte more is ErrTooLong with ErrInvalid, naming the limit,
// whichever project is selected, since the shared file is read whole.
func TestProjectNameLengthBound(t *testing.T) {
	t.Parallel()
	longest := strings.Repeat("p", 256)
	over := longest + "p"
	require.Equal(t, 256, settings.MaxProjectName)

	for _, project := range []string{longest, ""} {
		repo, dir := newRepo(t), t.TempDir()
		writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects."+longest+"]\nrepo = \"REPO\"\ngithub = \"o/r\"\n", repo)
		r, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project, LookupEnv: envOf(nil)})
		require.NoError(t, err)
		if project != "" {
			assert.Equal(t, longest, r.Profile.Name)
		}
		headers, err := settings.Headers(dir)
		require.NoError(t, err)
		assert.Equal(t, []settings.Header{{Project: longest, GitHub: "o/r"}}, headers)
	}

	for _, project := range []string{over, "app", ""} {
		repo, dir := newRepo(t), t.TempDir()
		writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n[projects."+over+"]\nrepo = \"REPO\"\n", repo)
		_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project, LookupEnv: envOf(nil)})
		require.ErrorIs(t, err, settings.ErrInvalid)
		require.ErrorIs(t, err, settings.ErrTooLong)
		assert.Contains(t, err.Error(), "a project name is at most 256 bytes, got 257")
		assert.Contains(t, err.Error(), settings.FileName)
		_, err = settings.Headers(dir)
		require.ErrorIs(t, err, settings.ErrTooLong)
	}
}

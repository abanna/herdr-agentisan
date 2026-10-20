package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestHeadersForTheDaemon: the daemon collects a header for every project
// with a GitHub repo (ADR-001 A29), read once at start from the shared file
// alone. The repo merges as the profile's does, so a shared github is every
// project's unless its own table says otherwise; no repo or bundle check
// runs and no environment is read. A file that does not resolve is its
// error.
func TestHeadersForTheDaemon(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		file    string // "" for no file; "nodir" for no config dir
		want    []settings.Header
		wantErr error
	}{
		"no config dir":      {file: "nodir"},
		"no file":            {},
		"no projects":        {file: "github = \"acme/app\"\n"},
		"no project has one": {file: "[projects.app]\nrepo = \"/x\"\n"},
		"by project name, those with one": {
			file: "[projects.zed]\nrepo = \"/z\"\nshort = \"z\"\ngithub = \"acme/zed\"\n[projects.mid]\nrepo = \"/m\"\nshort = \"m\"\n[projects.app]\nrepo = \"~/Development/app\"\nshort = \"a\"\ngithub = \"acme/app.go\"\n",
			want: []settings.Header{{Project: "app", GitHub: "acme/app.go"}, {Project: "zed", GitHub: "acme/zed"}},
		},
		"the shared repo is inherited": {
			file: "github = \"acme/mono\"\n[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n[projects.web]\nrepo = \"/y\"\nshort = \"w\"\ngithub = \"acme/web\"\n",
			want: []settings.Header{{Project: "app", GitHub: "acme/mono"}, {Project: "web", GitHub: "acme/web"}},
		},
		"not owner/name":        {file: "[projects.app]\nrepo = \"/x\"\ngithub = \"app\"\n", wantErr: settings.ErrInvalid},
		"a URL":                 {file: "[projects.app]\nrepo = \"/x\"\ngithub = \"https://github.com/acme/app\"\n", wantErr: settings.ErrInvalid},
		"an empty repo":         {file: "[projects.app]\nrepo = \"/x\"\ngithub = \"\"\n", wantErr: settings.ErrInvalid},
		"not a string":          {file: "[projects.app]\nrepo = \"/x\"\ngithub = 7\n", wantErr: settings.ErrInvalid},
		"an unknown [spaces]":   {file: "[spaces]\nproject = \"nope\"\n[projects.app]\nrepo = \"/x\"\ngithub = \"acme/app\"\n", wantErr: settings.ErrNoProject},
		"a newer schema":        {file: "schema_version = 2\n", wantErr: settings.ErrUnsupportedSchema},
		"a file that is broken": {file: "[projects.app\n", wantErr: settings.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			switch tc.file {
			case "nodir":
				dir = ""
			case "":
			default:
				body := "schema_version = 1\n" + tc.file
				if tc.file == "schema_version = 2\n" {
					body = tc.file
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(body), 0o600))
			}
			got, err := settings.Headers(dir)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestHeadersIgnoreTheRepoFile: a github set only in the project's repo
// file is not the daemon's, as for the colour: reading it would mean
// resolving the repo, which the daemon does not do at start.
func TestHeadersIgnoreTheRepoFile(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\ngithub = \"acme/fromrepo\"\n", repo)
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\nshort = \"a\"\n[projects.web]\nrepo = \"REPO\"\nshort = \"w\"\ngithub = \"acme/web\"\n", repo)

	got, err := settings.Headers(dir)
	require.NoError(t, err)
	assert.Equal(t, []settings.Header{{Project: "web", GitHub: "acme/web"}}, got)
}

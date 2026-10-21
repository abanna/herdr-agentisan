package settings_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// locksList is a TOML list of n absolute lock files.
func locksList(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("%q", fmt.Sprintf("/run/agentisan/gate.%d.lock", i))
	}
	return "[" + strings.Join(items, ", ") + "]"
}

// TestTestLocksPrecedence: [tests] locks names the lock files behind the
// header's test slots (A31). The shared file sets it, a project's own table
// replaces it for that project (A40), --set overrides both, and its
// provenance is reported like any leaf. Each entry comes back cleaned; unset
// or empty is none.
func TestTestLocksPrecedence(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	const two = "[tests]\nlocks = [\"/run/user/1000/agentisan/gate.0.lock\", \"/run/user/1000/agentisan/gate.1.lock\"]\n"
	const own = "[projects.app.tests]\nlocks = [\"/tmp//own.lock\"]\n"
	gates := []string{"/run/user/1000/agentisan/gate.0.lock", "/run/user/1000/agentisan/gate.1.lock"}
	tests := map[string]struct {
		file    string // the shared file after schema_version; "-" for no file
		project string
		set     []string
		want    []string
		layer   string // "" when no layer reports the key
	}{
		"no config file: none":             {file: "-"},
		"no [tests] table: none":           {file: app},
		"an empty [tests] table: none":     {file: "[tests]\n" + app},
		"an empty list: none":              {file: "[tests]\nlocks = []\n" + app, layer: settings.LayerShared},
		"set in the shared file":           {file: two + app, want: gates, layer: settings.LayerShared},
		"set, a project selected":          {file: two + app, project: "app", want: gates, layer: settings.LayerShared},
		"set as a dotted key":              {file: "tests.locks = [\"/tmp/a.lock\"]\n" + app, want: []string{"/tmp/a.lock"}, layer: settings.LayerShared},
		"each entry cleaned":               {file: "[tests]\nlocks = [\"/run//agentisan/./gate.0.lock\", \"/run/agentisan/x/../gate.1.lock\"]\n" + app, want: []string{"/run/agentisan/gate.0.lock", "/run/agentisan/gate.1.lock"}, layer: settings.LayerShared},
		"a path with spaces and non-ASCII": {file: "[tests]\nlocks = [\"/tmp/my slots/gäte.lock\"]\n" + app, want: []string{"/tmp/my slots/gäte.lock"}, layer: settings.LayerShared},
		"the most it allows":               {file: "[tests]\nlocks = " + locksList(settings.MaxTestLocks) + "\n" + app, want: settingsLocks(settings.MaxTestLocks), layer: settings.LayerShared},
		"--set beats the shared file":      {file: two + app, set: []string{`tests.locks=["/tmp/x.lock"]`}, want: []string{"/tmp/x.lock"}, layer: settings.LayerFlag},
		"--set with no key in the file":    {file: app, set: []string{`tests.locks=["/tmp/x.lock"]`}, want: []string{"/tmp/x.lock"}, layer: settings.LayerFlag},
		"--set empty turns them off":       {file: two + app, set: []string{`tests.locks=[]`}, layer: settings.LayerFlag},
		"a project's own, selected":        {file: two + app + own, project: "app", want: []string{"/tmp/own.lock"}, layer: settings.LayerProject},
		"a project's own, none selected":   {file: two + app + own, want: gates, layer: settings.LayerShared},
		"a project's own empty list":       {file: two + app + "[projects.app.tests]\nlocks = []\n", project: "app", layer: settings.LayerProject},
		"--set beats a project's own":      {file: two + app + own, project: "app", set: []string{`tests.locks=["/tmp/x.lock"]`}, want: []string{"/tmp/x.lock"}, layer: settings.LayerFlag},
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
			assert.Equal(t, tc.want, got.TestLocks())
			l, ok := leaf(t, got, "tests.locks")
			if tc.layer == "" {
				assert.False(t, ok, "no layer reports tests.locks")
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.layer, l.Layer)
		})
	}
}

// settingsLocks is what locksList(n) resolves to.
func settingsLocks(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("/run/agentisan/gate.%d.lock", i)
	}
	return out
}

// TestTestLocksErrors: every entry is an absolute path, named once, at most
// MaxTestLocks of them: the daemon reads them wherever it started and
// expands no ~. Anything else is refused at load, naming the key, its layer
// and file, in a project's table as in the shared file. A repo file may not
// set them: the lock files are the machine's, not a repo's.
func TestTestLocksErrors(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	locks := func(list string) string { return "[tests]\nlocks = " + list + "\n" + app }
	tests := map[string]struct {
		file     string
		repoFile string
		project  string
		set      []string
		wantErr  error
		wantIn   []string
	}{
		"a relative path":           {file: locks(`["gate.0.lock"]`), wantErr: settings.ErrInvalid, wantIn: []string{"tests.locks", "shared", settings.FileName, "entry 1", "not an absolute path"}},
		"a dot-relative path":       {file: locks(`["/tmp/a.lock", "./gate.lock"]`), wantErr: settings.ErrInvalid, wantIn: []string{"entry 2", "not an absolute path"}},
		"a home-relative path":      {file: locks(`["~/gate.lock"]`), wantErr: settings.ErrInvalid, wantIn: []string{"not an absolute path", "~ is not expanded"}},
		"an empty entry":            {file: locks(`[""]`), wantErr: settings.ErrInvalid, wantIn: []string{"entry 1 is not a path"}},
		"a NUL in an entry":         {file: locks(`["/tmp/a\u0000.lock"]`), wantErr: settings.ErrInvalid, wantIn: []string{"entry 1 is not a path"}},
		"the same path twice":       {file: locks(`["/tmp/a.lock", "/tmp/a.lock"]`), wantErr: settings.ErrInvalid, wantIn: []string{"entry 2", "already listed"}},
		"the same path spelt twice": {file: locks(`["/tmp/x//a.lock", "/tmp/x/./a.lock"]`), wantErr: settings.ErrInvalid, wantIn: []string{"entry 2", "already listed"}},
		"one past the most":         {file: locks(locksList(settings.MaxTestLocks + 1)), wantErr: settings.ErrInvalid, wantIn: []string{"33 lock files", "at most 32"}},
		"a relative path from --set": {
			file: app, set: []string{`tests.locks=["gate.lock"]`}, wantErr: settings.ErrInvalid, wantIn: []string{"tests.locks", "--set"},
		},
		"not a list":           {file: "[tests]\nlocks = \"/tmp/a.lock\"\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"tests.locks", "a list of strings"}},
		"a number in the list": {file: locks(`["/tmp/a.lock", 2]`), wantErr: settings.ErrInvalid, wantIn: []string{"tests.locks", "item 2"}},
		"tests is not a table": {file: "tests = [\"/tmp/a.lock\"]\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"tests", "want a table"}},
		"an unknown key":       {file: "[tests]\nslots = 2\n" + app, wantErr: settings.ErrUnknownKey, wantIn: []string{"tests.slots"}},
		"a relative path in a project's table": {
			file: app + "[projects.app.tests]\nlocks = [\"gate.lock\"]\n", wantErr: settings.ErrInvalid,
			wantIn: []string{"projects.app.tests.locks", "project", "entry 1", "not an absolute path"},
		},
		"a project's table, its project selected": {
			file: app + "[projects.app.tests]\nlocks = [\"/tmp/a.lock\", \"/tmp/a.lock\"]\n", project: "app", wantErr: settings.ErrInvalid,
			wantIn: []string{"projects.app.tests.locks", "entry 2", "already listed"},
		},
		"an unknown key in a project's table": {
			file: app + "[projects.app.tests]\nslots = 2\n", wantErr: settings.ErrUnknownKey,
			wantIn: []string{"projects.app.tests.slots"},
		},
		"in a repo file": {
			file: app, repoFile: "[tests]\nlocks = [\"/tmp/a.lock\"]\n", project: "app",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"tests", settings.RepoFileName},
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

// TestReadTestLocks: the daemon reads every project's lock files once, from
// the shared file alone (A40). A project's own list replaces the shared one,
// an empty one included; a project with none of its own, and a project the
// file does not configure, counts the shared one. No config dir or no file is
// none; a file that does not resolve is its error, and no slots are reported.
func TestReadTestLocks(t *testing.T) {
	t.Parallel()
	const shared = "[tests]\nlocks = [\"/run/a//gate.0.lock\", \"/run/a/gate.1.lock\"]\n"
	gates := []string{"/run/a/gate.0.lock", "/run/a/gate.1.lock"}
	tests := map[string]struct {
		file     string // "" for no file; "nodir" for no config dir
		want     settings.TestLocks
		project  string // a project to look up, with Of
		wantOf   []string
		wantErr  error
		wantText string
	}{
		"no config dir":  {file: "nodir", project: "app"},
		"no file":        {want: settings.TestLocks{Projects: map[string][]string{}}, project: "app"},
		"no [tests]":     {file: "[projects.app]\nrepo = \"/x\"\n", want: settings.TestLocks{Projects: map[string][]string{"app": nil}}, project: "app"},
		"two lock files": {file: shared, want: settings.TestLocks{Shared: gates, Projects: map[string][]string{}}, project: "app", wantOf: gates},
		"a project with none of its own": {
			file: shared + "[projects.app]\nrepo = \"/x\"\n", want: settings.TestLocks{Shared: gates, Projects: map[string][]string{"app": gates}},
			project: "app", wantOf: gates,
		},
		"a project's own": {
			file:    shared + "[projects.app]\nrepo = \"/x\"\nshort = \"ap\"\n[projects.app.tests]\nlocks = [\"/tmp/skills/./a.lock\"]\n[projects.web]\nrepo = \"/y\"\nshort = \"we\"\n",
			want:    settings.TestLocks{Shared: gates, Projects: map[string][]string{"app": {"/tmp/skills/a.lock"}, "web": gates}},
			project: "app", wantOf: []string{"/tmp/skills/a.lock"},
		},
		"a project's own empty list": {
			file:    shared + "[projects.app]\nrepo = \"/x\"\n[projects.app.tests]\nlocks = []\n",
			want:    settings.TestLocks{Shared: gates, Projects: map[string][]string{"app": nil}},
			project: "app",
		},
		"a project's own, with no shared list": {
			file:    "[projects.app]\nrepo = \"/x\"\n[projects.app.tests]\nlocks = [\"/tmp/a.lock\"]\n",
			want:    settings.TestLocks{Projects: map[string][]string{"app": {"/tmp/a.lock"}}},
			project: "app", wantOf: []string{"/tmp/a.lock"},
		},
		"the unnamed project": {
			file: shared + "[projects.app]\nrepo = \"/x\"\n[projects.app.tests]\nlocks = []\n", want: settings.TestLocks{Shared: gates, Projects: map[string][]string{"app": nil}},
			wantOf: gates,
		},
		"a project the file does not configure": {
			file: shared + "[projects.app]\nrepo = \"/x\"\n", want: settings.TestLocks{Shared: gates, Projects: map[string][]string{"app": gates}},
			project: "other", wantOf: gates,
		},
		"a relative one":           {file: "[tests]\nlocks = [\"gate.lock\"]\n", wantErr: settings.ErrInvalid},
		"a project's relative one": {file: "[projects.app]\nrepo = \"/x\"\n[projects.app.tests]\nlocks = [\"gate.lock\"]\n", wantErr: settings.ErrInvalid, wantText: "projects.app.tests.locks"},
		"a newer schema":           {file: "schema_version = 2\n", wantErr: settings.ErrUnsupportedSchema},
		"a file that breaks":       {file: "[tests\n", wantErr: settings.ErrInvalid},
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
			got, err := settings.ReadTestLocks(dir)
			require.ErrorIs(t, err, tc.wantErr)
			if tc.wantText != "" {
				assert.Contains(t, err.Error(), tc.wantText)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantOf, got.Of(tc.project))
		})
	}
}

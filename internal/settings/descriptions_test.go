package settings_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestGroupDescriptions is A36's configuration: a group's description comes
// from the layers D9 orders, with its provenance, and reaches the profile
// and the daemon's teams; "" from a higher layer clears a lower one's; a
// group that sets only a description starts no worker.
func TestGroupDescriptions(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	const coder = "[groups.coder]\nworkers = 1\nslots = 2\n"
	tests := map[string]struct {
		file  string // the shared file after schema_version
		set   []string
		group string
		want  settings.Group
		layer string // groups.<group>.description's; "" when unset
		teams map[string]string
	}{
		"no description": {file: coder + app, group: "coder", want: settings.Group{Workers: 1, Slots: 2}},
		"a shared one, for every project": {
			file: coder + "description = \"writes code\"\n" + app, group: "coder",
			want: settings.Group{Workers: 1, Slots: 2, Description: "writes code"}, layer: settings.LayerShared,
			teams: map[string]string{"coder": "writes code"},
		},
		"the project's own wins": {
			file: coder + "description = \"writes code\"\n" + app + "[projects.app.groups.coder]\ndescription = \"fixes bugs\"\n", group: "coder",
			want: settings.Group{Workers: 1, Slots: 2, Description: "fixes bugs"}, layer: settings.LayerProject,
			teams: map[string]string{"coder": "fixes bugs"},
		},
		"--set wins": {
			file: coder + "description = \"writes code\"\n" + app, set: []string{`groups.coder.description="from a flag"`}, group: "coder",
			want: settings.Group{Workers: 1, Slots: 2, Description: "from a flag"}, layer: settings.LayerFlag,
			teams: map[string]string{"coder": "from a flag"},
		},
		`--set "" clears a shared one`: {
			file: coder + "description = \"writes code\"\n" + app, set: []string{`groups.coder.description=""`}, group: "coder",
			want: settings.Group{Workers: 1, Slots: 2}, layer: settings.LayerFlag,
		},
		"a group that only describes, in the project's table": {
			file: app + "[projects.app.groups.coders]\ndescription = \"write fixes and new code\"\n", group: "coders",
			want: settings.Group{Description: "write fixes and new code"}, layer: settings.LayerProject,
			teams: map[string]string{"coders": "write fixes and new code"},
		},
		"a group that only describes, shared": {
			file: "[groups.qa]\ndescription = \"runs the gates\"\n" + app, group: "qa",
			want: settings.Group{Description: "runs the gates"}, layer: settings.LayerShared,
			teams: map[string]string{"qa": "runs the gates"},
		},
		"a group that only describes, with an empty description": {
			file: app + "[projects.app.groups.coders]\ndescription = \"\"\n", group: "coders",
			want: settings.Group{}, layer: settings.LayerProject,
		},
		"a shared worker group the project describes": {
			file: coder + app + "[projects.app.groups.coder]\ndescription = \"writes code\"\n", group: "coder",
			want: settings.Group{Workers: 1, Slots: 2, Description: "writes code"}, layer: settings.LayerProject,
			teams: map[string]string{"coder": "writes code"},
		},
		"exactly 80 characters": {
			file: app + "[projects.app.groups.qa]\ndescription = \"" + strings.Repeat("a", 80) + "\"\n", group: "qa",
			want: settings.Group{Description: strings.Repeat("a", 80)}, layer: settings.LayerProject,
			teams: map[string]string{"qa": strings.Repeat("a", 80)},
		},
		"80 characters that are not ASCII": {
			file: app + "[projects.app.groups.qa]\ndescription = \"" + strings.Repeat("é", 80) + "\"\n", group: "qa",
			want: settings.Group{Description: strings.Repeat("é", 80)}, layer: settings.LayerProject,
			teams: map[string]string{"qa": strings.Repeat("é", 80)},
		},
		"punctuation, symbols and spaces": {
			file: app + "[projects.app.groups.qa]\ndescription = \" gates + slots · re-run → PR #41 ✓ \"\n", group: "qa",
			want: settings.Group{Description: " gates + slots · re-run → PR #41 ✓ "}, layer: settings.LayerProject,
			teams: map[string]string{"qa": " gates + slots · re-run → PR #41 ✓ "},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.file, repo)

			r, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", Set: tc.set, LookupEnv: envOf(nil)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, r.Profile.Groups[tc.group])
			l, ok := leaf(t, r, "groups."+tc.group+".description")
			assert.Equal(t, tc.layer != "", ok, "a description leaf")
			if ok {
				assert.Equal(t, tc.layer, l.Layer)
				assert.Equal(t, tc.want.Description, l.Value)
			}
			require.Len(t, r.Teams, 1)
			assert.Equal(t, tc.teams, r.Teams[0].Descriptions)
			if tc.set == nil {
				teams, err := settings.Teams(dir)
				require.NoError(t, err)
				require.Len(t, teams, 1)
				assert.Equal(t, tc.teams, teams[0].Descriptions, "what the daemon reads")
			}
		})
	}
}

// TestGroupDescriptionErrors: a description is one line every client draws
// as written, at most 80 characters; a group that sets workers or slots
// still needs both; boss is never a group; and a repo file cannot set one,
// since the daemon reads no repo file. Each error names the key path and its
// layer, never the value, which may carry terminal escapes.
func TestGroupDescriptionErrors(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	described := func(value string) string {
		return app + "[projects.app.groups.qa]\ndescription = \"" + value + "\"\n"
	}
	tests := map[string]struct {
		file     string // the shared file after schema_version
		repoFile string // the repo file after schema_version
		set      []string
		wantIn   []string
		value    string // a part of the value the error must not hold
		inRepo   bool   // only the repo file is wrong, which the daemon never reads
		// noProject loads with no project selected, which lints every
		// project's table, as the daemon's read does.
		noProject bool
	}{
		"81 characters": {
			file:   described(strings.Repeat("a", 81)),
			wantIn: []string{"projects.app.groups.qa.description (project", "81 characters, more than 80"}, value: strings.Repeat("a", 81),
		},
		"81 characters that are not ASCII": {
			file: described(strings.Repeat("é", 81)), wantIn: []string{"81 characters, more than 80"}, value: "éé",
		},
		"a tab":             {file: described(`a\tb`), wantIn: []string{"U+0009 at byte 1"}},
		"a newline":         {file: described(`a\nb`), wantIn: []string{"U+000A at byte 1"}},
		"a carriage return": {file: described(`a\rb`), wantIn: []string{"U+000D at byte 1"}},
		"a NUL":             {file: described(`a\u0000b`), wantIn: []string{"U+0000 at byte 1"}},
		"a terminal escape": {file: described(`x\u001b[2Jy`), wantIn: []string{"U+001B at byte 1"}, value: "[2J"},
		"a zero-width space": {
			file: described(`a\u200bb`), wantIn: []string{"U+200B at byte 1"},
		},
		"a bidi override":   {file: described(`\u202eabc`), wantIn: []string{"U+202E at byte 0"}, value: "abc"},
		"a no-break space":  {file: described(`a\u00a0b`), wantIn: []string{"U+00A0 at byte 1"}},
		"a line separator":  {file: described(`a\u2028b`), wantIn: []string{"U+2028 at byte 1"}},
		"a private-use one": {file: described(`a\ue000b`), wantIn: []string{"U+E000 at byte 1"}},
		"a byte-order mark": {file: described(`\ufeffabc`), wantIn: []string{"U+FEFF at byte 0"}},
		"an empty group table": {
			file: app + "[projects.app.groups.qa]\n", wantIn: []string{"groups.qa.workers (not set in any layer)", "every group needs workers"},
		},
		"not a string": {
			file: app + "[projects.app.groups.qa]\ndescription = 5\n", wantIn: []string{"groups.qa.description"},
		},
		"workers without slots": {
			file:   app + "[projects.app.groups.qa]\nworkers = 1\ndescription = \"x\"\n",
			wantIn: []string{"groups.qa.slots (not set in any layer)", "every group needs slots"},
		},
		"slots without workers": {
			file:   app + "[projects.app.groups.qa]\nslots = 1\ndescription = \"x\"\n",
			wantIn: []string{"groups.qa.workers (not set in any layer)", "every group needs workers"},
		},
		"the boss described": {
			file: app + "[projects.app.groups.boss]\ndescription = \"x\"\n", wantIn: []string{"boss's tab"},
		},
		"from --set, too long": {
			file: app, set: []string{`groups.qa.description="` + strings.Repeat("b", 81) + `"`},
			wantIn: []string{"groups.qa.description (flag", "81 characters"}, value: strings.Repeat("b", 81),
		},
		"in another project": {
			file:   "[spaces]\nproject = \"app\"\n" + app + "[projects.other]\nrepo = \"/x\"\nshort = \"ot\"\n[projects.other.groups.qa]\ndescription = \"a\\u0007b\"\n",
			wantIn: []string{"projects.other.groups.qa.description", "U+0007"}, noProject: true,
		},
		"in a repo file": {
			file: app, repoFile: "[groups.qa]\ndescription = \"from the repo\"\n", inRepo: true,
			wantIn: []string{"groups.qa.description (repo, ", settings.RepoFileName, "set it in " + settings.FileName}, value: "from the repo",
		},
		"in a repo file, empty": {
			file: app, repoFile: "[groups.qa]\nworkers = 1\nslots = 1\ndescription = \"\"\n", inRepo: true,
			wantIn: []string{"groups.qa.description (repo, "},
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
			if tc.noProject {
				project = ""
			}
			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: project, Set: tc.set, LookupEnv: envOf(nil)})
			require.ErrorIs(t, err, settings.ErrInvalid)
			for _, s := range tc.wantIn {
				assert.Contains(t, err.Error(), s)
			}
			if tc.value != "" {
				assert.NotContains(t, err.Error(), tc.value, "the error never holds the value")
			}
			assert.NotContains(t, err.Error(), "\x1b")
			if tc.set != nil {
				return
			}
			_, err = settings.Teams(dir)
			if tc.inRepo {
				assert.NoError(t, err, "the daemon reads no repo file")
				return
			}
			assert.ErrorIs(t, err, settings.ErrInvalid, "the daemon refuses it too")
		})
	}
}

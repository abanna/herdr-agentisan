package settings_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestRepoFileRefusesUserOnlyKeys: a repo file that sets what runs or is
// typed in the user's terminals, or where a secret is read and sent, is
// refused, whatever the value and however the key is spelt (ADR-001 A46).
// Each error names the key path, the repo layer and its file, and where to
// set it instead, never the value; with several such keys, the first in path
// order, at every load.
func TestRepoFileRefusesUserOnlyKeys(t *testing.T) {
	t.Parallel()

	const group = "[groups.qa]\nworkers = 1\nslots = 1\n"
	tests := map[string]struct {
		repoFile string // the repo file after schema_version
		path     string // the key path the error names
		value    string // a part of the value the error must not hold
	}{
		"agent.command":                {repoFile: "[agent]\ncommand = [\"claude\", \"--from-the-repo\"]\n", path: "agent.command", value: "from-the-repo"},
		"agent.command, a dotted key":  {repoFile: "agent.command = [\"claude\", \"--from-the-repo\"]\n", path: "agent.command", value: "from-the-repo"},
		"agent.command, inline":        {repoFile: "agent = { command = [\"claude\", \"--from-the-repo\"] }\n", path: "agent.command", value: "from-the-repo"},
		"agent.command, empty":         {repoFile: "[agent]\ncommand = []\n", path: "agent.command"},
		"agent.command beside a model": {repoFile: "[agent]\nmodel = \"opus\"\ncommand = [\"claude\"]\n", path: "agent.command"},
		"a group's command": {
			repoFile: group + "[groups.qa.agent]\ncommand = [\"codex\", \"--from-the-repo\"]\n", path: "groups.qa.agent.command", value: "from-the-repo",
		},
		"a group's command, inline": {
			repoFile: group + "agent = { command = [\"codex\", \"--from-the-repo\"] }\n", path: "groups.qa.agent.command", value: "from-the-repo",
		},
		"a group's command, a dotted key": {
			repoFile: "groups.qa.agent.command = [\"codex\"]\n", path: "groups.qa.agent.command",
		},
		"a group's command, empty": {repoFile: group + "agent = { command = [] }\n", path: "groups.qa.agent.command"},
		"a group's command beside its model": {
			repoFile: group + "agent = { model = \"o3\", command = [\"codex\"] }\n", path: "groups.qa.agent.command",
		},
		"spawn.shell_prompt":             {repoFile: "[spawn]\nshell_prompt = \"from-the-repo\"\n", path: "spawn.shell_prompt", value: "from-the-repo"},
		"spawn.shell_prompt, empty":      {repoFile: "[spawn]\nshell_prompt = \"\"\n", path: "spawn.shell_prompt"},
		"spawn.shell_ready_keys":         {repoFile: "[spawn]\nshell_ready_keys = [\"y\", \"enter\"]\n", path: "spawn.shell_ready_keys", value: "enter"},
		"spawn.shell_ready_keys, empty":  {repoFile: "[spawn]\nshell_ready_keys = []\n", path: "spawn.shell_ready_keys"},
		"spawn.shell_ready_keys, dotted": {repoFile: "spawn.shell_ready_keys = [\"enter\"]\n", path: "spawn.shell_ready_keys"},
		"spawn.shell_ready_after_s, 0":   {repoFile: "[spawn]\nshell_ready_after_s = 0\n", path: "spawn.shell_ready_after_s"},
		"verify": {
			repoFile: "verify = [[\"task\", \"from-the-repo\"]]\n", path: "verify", value: "from-the-repo",
		},
		"verify, empty": {repoFile: "verify = []\n", path: "verify"},
		"an integration's url": {
			repoFile: "[integrations.github]\nurl = \"https://from-the-repo.example\"\n", path: "integrations.github.url", value: "from-the-repo",
		},
		"an integration's token from the environment": {
			repoFile: "[integrations.github]\ntoken = { env = \"FROM_THIS_FILE\" }\n", path: "integrations.github.token", value: "FROM_THIS_FILE",
		},
		"an integration's token from 1Password": {
			repoFile: "[integrations.linear]\ntoken = { op = \"op://Private/from-the-repo/credential\" }\n", path: "integrations.linear.token", value: "from-the-repo",
		},
		"the first key in path order is the one named": {
			repoFile: "verify = [[\"task\"]]\n[spawn]\nshell_ready_keys = [\"enter\"]\nshell_prompt = \"x\"\n[agent]\ncommand = [\"claude\"]\n",
			path:     "agent.command",
		},
		"the first spawn key in path order": {
			repoFile: "[spawn]\nshell_ready_keys = [\"enter\"]\nshell_ready_after_s = 1\nshell_prompt = \"x\"\n", path: "spawn.shell_prompt",
		},
		"the first group in name order": {
			repoFile: "[groups.qa.agent]\ncommand = [\"codex\"]\n[groups.coder.agent]\ncommand = [\"claude\"]\n", path: "groups.coder.agent.command",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n", repo)
			writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+tc.repoFile, repo)

			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
			require.ErrorIs(t, err, settings.ErrInvalid)
			// Go's map order changes from one walk to the next, so with
			// several keys set only a sorted walk names the same one every
			// time.
			for range 20 {
				_, again := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
				require.EqualError(t, again, err.Error(), "every load names the same key")
			}
			assert.Contains(t, err.Error(), tc.path+" (repo, ")
			assert.Contains(t, err.Error(), settings.RepoFileName)
			assert.Contains(t, err.Error(), "set it in "+settings.FileName+" or with --set")
			if tc.value != "" {
				assert.NotContains(t, err.Error(), tc.value, "the error never holds the value")
			}
		})
	}
}

// TestRepoFileKeepsTheSafeKeys: a repo file still sets its colour, GitHub
// repo, group counts, models and rotation, and an empty table of a refused
// key sets nothing, so it passes.
func TestRepoFileKeepsTheSafeKeys(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n", repo)
	writeFile(t, repo, settings.RepoFileName, `schema_version = 1
color = "#123456"
github = "acme/app"
[agent]
model = "opus"
[groups.qa]
workers = 1
slots = 2
agent = { model = "sonnet" }
[rotation]
thresholds = [50, 60, 70]
[spawn]
[integrations.github]
[groups.coder]
workers = 1
slots = 1
[groups.coder.agent]
`, repo)

	got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.NoError(t, err)
	for path, w := range map[string]want{
		"color":                 {"#123456", settings.LayerRepo},
		"github":                {"acme/app", settings.LayerRepo},
		"agent.model":           {"opus", settings.LayerRepo},
		"groups.qa.workers":     {1, settings.LayerRepo},
		"groups.qa.slots":       {2, settings.LayerRepo},
		"groups.qa.agent.model": {"sonnet", settings.LayerRepo},
		"rotation.thresholds":   {[]int{50, 60, 70}, settings.LayerRepo},
		"agent.command":         {[]string{"claude"}, settings.LayerDefault},
	} {
		l, ok := leaf(t, got, path)
		require.Truef(t, ok, "no leaf %s in %v", path, got.Sources)
		assert.Equalf(t, w.value, l.Value, "value of %s", path)
		assert.Equalf(t, w.layer, l.Layer, "layer of %s", path)
	}
	assert.Equal(t, "sonnet", got.Profile.AgentFor("qa").Model)
	assert.Empty(t, got.Profile.Spawn.ShellReadyKeys)
	assert.Empty(t, got.Profile.Verify)
}

// TestUserOnlyKeysFromTheUsersLayers: what a repo file cannot set still
// resolves from the user's own layers, the shared file, its project table and
// --set, with a repo file beside them that sets only safe keys.
func TestUserOnlyKeysFromTheUsersLayers(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, `schema_version = 1
verify = [["task", "check"]]
[agent]
command = ["claude", "--verbose"]
[spawn]
shell_prompt = "\\$ $"
[integrations.github]
url = "https://github.com"
token = { env = "GH_TOKEN" }
[projects.app]
repo = "REPO"
[projects.app.groups.qa]
workers = 1
slots = 1
agent = { command = ["codex", "--search"] }
`, repo)
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\ncolor = \"#123456\"\n", repo)

	got, err := settings.Load(settings.LoadOptions{
		ConfigDir: dir, Project: "app", LookupEnv: envOf(nil),
		Set: []string{`spawn.shell_ready_keys=["enter"]`, "spawn.shell_ready_after_s=5"},
	})
	require.NoError(t, err)
	for path, w := range map[string]want{
		"agent.command":             {[]string{"claude", "--verbose"}, settings.LayerShared},
		"verify":                    {[][]string{{"task", "check"}}, settings.LayerShared},
		"spawn.shell_prompt":        {`\$ $`, settings.LayerShared},
		"integrations.github.url":   {"https://github.com", settings.LayerShared},
		"integrations.github.token": {settings.SecretRef{Env: "GH_TOKEN"}, settings.LayerShared},
		"groups.qa.agent.command":   {[]string{"codex", "--search"}, settings.LayerProject},
		"spawn.shell_ready_keys":    {[]string{"enter"}, settings.LayerFlag},
		"spawn.shell_ready_after_s": {5, settings.LayerFlag},
		"color":                     {"#123456", settings.LayerRepo},
	} {
		l, ok := leaf(t, got, path)
		require.Truef(t, ok, "no leaf %s in %v", path, got.Sources)
		assert.Equalf(t, w.value, l.Value, "value of %s", path)
		assert.Equalf(t, w.layer, l.Layer, "layer of %s", path)
	}
}

// TestRepoFileNamesCannotSplitAPath: a group or integration name with a dot
// is refused before the user-only check walks the file, so no quoted name
// can stretch a key path past that check's patterns.
func TestRepoFileNamesCannotSplitAPath(t *testing.T) {
	t.Parallel()
	for name, repoFile := range map[string]string{
		"an integration": "[integrations.\"github.x\"]\nurl = \"https://from-the-repo.example\"\n",
		"a group":        "[groups.\"qa.x\".agent]\ncommand = [\"codex\"]\n",
	} {
		repo, dir := newRepo(t), t.TempDir()
		writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n", repo)
		writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+repoFile, repo)

		_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
		require.ErrorIs(t, err, settings.ErrInvalid, name)
		assert.Contains(t, err.Error(), "a name must match", name)
	}
}

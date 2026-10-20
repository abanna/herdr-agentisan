package settings_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestAgentFor: a type launches its group's own command and model when the
// group sets a command, never the top-level model that belongs to the
// top-level command; review launches codex unless its group says otherwise;
// every other type launches the top-level command, with its group's model or
// the top-level one.
func TestAgentFor(t *testing.T) {
	t.Parallel()
	top := settings.Agent{Command: []string{"claude", "--setting-sources", "user,project"}, Model: "opus"}
	tests := map[string]struct {
		groups map[string]settings.Group
		typ    string
		want   settings.Agent
	}{
		"a type with no group of its own":            {typ: "coder", want: top},
		"a group without an agent":                   {groups: map[string]settings.Group{"coder": {Workers: 1, Slots: 1}}, typ: "coder", want: top},
		"a group model only":                         {groups: map[string]settings.Group{"qa": {Agent: settings.Agent{Model: "sonnet"}}}, typ: "qa", want: settings.Agent{Command: top.Command, Model: "sonnet"}},
		"a group command drops the top-level model":  {groups: map[string]settings.Group{"research": {Agent: settings.Agent{Command: []string{"codex"}}}}, typ: "research", want: settings.Agent{Command: []string{"codex"}}},
		"a group command and model":                  {groups: map[string]settings.Group{"research": {Agent: settings.Agent{Command: []string{"codex"}, Model: "gpt-5"}}}, typ: "research", want: settings.Agent{Command: []string{"codex"}, Model: "gpt-5"}},
		"review is codex by default, off the daemon": {typ: "review", want: settings.Agent{Command: []string{"codex", "--no-daemon"}}},
		"review keeps its group's model":             {groups: map[string]settings.Group{"review": {Agent: settings.Agent{Model: "gpt-5"}}}, typ: "review", want: settings.Agent{Command: []string{"codex", "--no-daemon"}, Model: "gpt-5"}},
		"review set back to claude":                  {groups: map[string]settings.Group{"review": {Agent: settings.Agent{Command: []string{"claude"}}}}, typ: "review", want: settings.Agent{Command: []string{"claude"}}},
		"the match on review is exact":               {typ: "Review", want: top},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := settings.Profile{Agent: top, Groups: tc.groups}
			got := p.AgentFor(tc.typ)
			assert.Equal(t, tc.want, got)

			got.Command[0] = "mutated"
			assert.NotEqual(t, "mutated", p.AgentFor(tc.typ).Command[0], "the profile's command is not shared")
		})
	}
}

// TestLoadReadsTheWorkforceKeys: a group's agent and [spawn] reach the
// profile, from any layer; a tabs project's short code and layout do too.
func TestLoadReadsTheWorkforceKeys(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, `schema_version = 1
[spawn]
shell_ready_keys = ["enter"]

[projects.app]
repo = "REPO"
short = "app"
layout = "tabs"

[projects.app.spawn]
shell_prompt = '❯\s*$'
shell_ready_after_s = 10

[projects.app.groups.review]
workers = 1
slots = 2
agent = { model = "gpt-5" }

[projects.app.groups.research]
workers = 0
slots = 1
agent = { command = ["codex", "--search"] }
`, repo)
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n[groups.qa]\nworkers = 1\nslots = 1\nagent = { model = \"sonnet\" }\n", repo)

	got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.NoError(t, err)
	p := got.Profile
	assert.Equal(t, []settings.Team{{Project: "app", Short: "app", Layout: settings.LayoutTabs}}, got.Teams)
	assert.Equal(t, settings.Spawn{ShellPrompt: `❯\s*$`, ShellReadyKeys: []string{"enter"}, ShellReadyAfter: 10 * time.Second}, p.Spawn)
	assert.Equal(t, settings.Agent{Command: []string{"codex", "--no-daemon"}, Model: "gpt-5"}, p.AgentFor("review"))
	assert.Equal(t, settings.Agent{Command: []string{"codex", "--search"}}, p.AgentFor("research"))
	assert.Equal(t, settings.Agent{Command: []string{"claude"}, Model: "sonnet"}, p.AgentFor("qa"), "the repo file sets a group's agent too")
	assert.Equal(t, settings.Agent{Command: []string{"claude"}}, p.AgentFor("coder"))
}

// TestLoadRefusesWorkforceKeys: a group's command bypassing Agentisan, any
// command carrying a flag the workforce sets, and a [spawn] value that cannot
// work are each refused with the key that holds them.
func TestLoadRefusesWorkforceKeys(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		body    string
		wantErr error
		wantAt  string
	}{
		"a group command that is not claude or codex": {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"aider\"] }\n", wantErr: settings.ErrBypassesAgentisan, wantAt: "groups.qa.agent.command"},
		"a group command with --bare":                 {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\", \"--bare\"] }\n", wantErr: settings.ErrBypassesAgentisan, wantAt: "groups.qa.agent.command"},
		"an empty group command":                      {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [] }\n", wantErr: settings.ErrBypassesAgentisan, wantAt: "groups.qa.agent.command"},
		"a group command with --model":                {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\", \"--model\", \"opus\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command"},
		"a top-level command with -m":                 {body: "[agent]\ncommand = [\"codex\", \"-m\", \"o3\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"a top-level command with --name=":            {body: "[agent]\ncommand = [\"claude\", \"--name=boss\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"a top-level command with -n":                 {body: "[agent]\ncommand = [\"claude\", \"-n\", \"x\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"a short flag's value attached":               {body: "[agent]\ncommand = [\"codex\", \"-mo3\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"a one-byte value attached":                   {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"codex\", \"-n1\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command"},
		// The check knows no flag's arity, so it refuses a launch flag's name
		// even where it is another flag's value.
		"a launch flag's name as a value":      {body: "[agent]\ncommand = [\"claude\", \"--append-system-prompt\", \"--model\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"a group agent key it does not know":   {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { kind = \"codex\" }\n", wantErr: settings.ErrUnknownKey},
		"an empty shell prompt":                {body: "[spawn]\nshell_prompt = \"\"\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_prompt"},
		"a shell prompt that does not compile": {body: "[spawn]\nshell_prompt = \"(\"\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_prompt"},
		"an empty key name":                    {body: "[spawn]\nshell_ready_keys = [\"enter\", \" \"]\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_ready_keys"},
		"a negative wait":                      {body: "[spawn]\nshell_ready_after_s = -1\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_ready_after_s"},
		"a wait over a minute":                 {body: "[spawn]\nshell_ready_after_s = 61\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_ready_after_s"},
		"a wait that is no number":             {body: "[spawn]\nshell_ready_after_s = \"3\"\n", wantErr: settings.ErrInvalid},
		"keys that are no list":                {body: "[spawn]\nshell_ready_keys = \"enter\"\n", wantErr: settings.ErrInvalid},
		"a [spawn] key it does not know":       {body: "[spawn]\nshell_ready_after = 3\n", wantErr: settings.ErrUnknownKey},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.body+"[projects.app]\nrepo = \"REPO\"\n", repo)

			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, LookupEnv: envOf(nil)})
			require.ErrorIs(t, err, tc.wantErr)
			if tc.wantAt != "" {
				assert.Contains(t, err.Error(), tc.wantAt)
			}
		})
	}
}

// TestLoadAcceptsTheWorkforceEdges: the bounds of the wait, a shell prompt
// of any regular expression, flags that only look like the launch flags and
// a path to codex all resolve.
func TestLoadAcceptsTheWorkforceEdges(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"no wait at all":               "[spawn]\nshell_ready_after_s = 0\n",
		"slots at the bound":           "[groups.qa]\nworkers = 64\nslots = 64\n",
		"a minute's wait":              "[spawn]\nshell_ready_after_s = 60\n",
		"no keys":                      "[spawn]\nshell_ready_keys = []\n",
		"a non-ASCII prompt":           "[spawn]\nshell_prompt = '➜\\s+\\S+'\n",
		"a look-alike of a flag":       "[agent]\ncommand = [\"claude\", \"--models\", \"--model-x\", \"-x\", \"-é\", \"-\"]\n",
		"codex as a group's own agent": "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"/opt/bin/codex\"] }\n",
		// argv[0] is the program, which D12 checks by its base name.
		"a program path that starts like a flag": "[agent]\ncommand = [\"-m/claude\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+body+"[projects.app]\nrepo = \"REPO\"\n", repo)

			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
			require.NoError(t, err)
		})
	}
}

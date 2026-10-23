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
// the debugger launches claude, never the top-level command or one of its
// group's, whose arguments the workforce builds (A32); every other type
// launches the top-level command, with its group's model or the top-level
// one.
func TestAgentFor(t *testing.T) {
	t.Parallel()
	top := settings.Agent{Command: []string{"claude", "--setting-sources", "user,project"}, Model: "opus"}
	tests := map[string]struct {
		groups map[string]settings.Group
		// top replaces the top-level agent.
		top  *settings.Agent
		typ  string
		want settings.Agent
	}{
		"a type with no group of its own":            {typ: "coder", want: top},
		"a group without an agent":                   {groups: map[string]settings.Group{"coder": {Workers: 1, Slots: 1}}, typ: "coder", want: top},
		"a group model only":                         {groups: map[string]settings.Group{"qa": {Agent: settings.Agent{Model: "sonnet"}}}, typ: "qa", want: settings.Agent{Command: top.Command, Model: "sonnet"}},
		"a group command drops the top-level model":  {groups: map[string]settings.Group{"research": {Agent: settings.Agent{Command: []string{"codex"}}}}, typ: "research", want: settings.Agent{Command: []string{"codex"}}},
		"a group command and model":                  {groups: map[string]settings.Group{"research": {Agent: settings.Agent{Command: []string{"codex"}, Model: "gpt-5"}}}, typ: "research", want: settings.Agent{Command: []string{"codex"}, Model: "gpt-5"}},
		"review is codex by default, off the daemon": {typ: "review", want: settings.Agent{Command: []string{"codex", "--no-daemon", "--sandbox", "read-only"}}},
		"review keeps its group's model":             {groups: map[string]settings.Group{"review": {Agent: settings.Agent{Model: "gpt-5"}}}, typ: "review", want: settings.Agent{Command: []string{"codex", "--no-daemon", "--sandbox", "read-only"}, Model: "gpt-5"}},
		"review set back to claude":                  {groups: map[string]settings.Group{"review": {Agent: settings.Agent{Command: []string{"claude"}}}}, typ: "review", want: settings.Agent{Command: []string{"claude"}}},
		"the match on review is exact":               {typ: "Review", want: top},
		"the debugger is claude, with the top model": {typ: "debugger", want: settings.Agent{Command: []string{"claude"}, Model: "opus"}},
		"the debugger keeps its group's model":       {groups: map[string]settings.Group{"debugger": {Agent: settings.Agent{Model: "sonnet"}}}, typ: "debugger", want: settings.Agent{Command: []string{"claude"}, Model: "sonnet"}},
		"a codex top level's model is not claude's":  {top: &settings.Agent{Command: []string{"codex"}, Model: "gpt-5"}, typ: "debugger", want: settings.Agent{Command: []string{"claude"}}},
		"claude by path at the top level":            {top: &settings.Agent{Command: []string{"/usr/local/bin/claude"}, Model: "opus"}, typ: "debugger", want: settings.Agent{Command: []string{"claude"}, Model: "opus"}},
		"no top-level command":                       {top: &settings.Agent{Model: "opus"}, typ: "debugger", want: settings.Agent{Command: []string{"claude"}}},
		"a debugger group's command never applies":   {groups: map[string]settings.Group{"debugger": {Agent: settings.Agent{Command: []string{"codex"}}}}, typ: "debugger", want: settings.Agent{Command: []string{"claude"}, Model: "opus"}},
		"the match on debugger is exact":             {typ: "Debugger", want: top},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := settings.Profile{Agent: top, Groups: tc.groups}
			if tc.top != nil {
				p.Agent = *tc.top
			}
			got := p.AgentFor(tc.typ)
			assert.Equal(t, tc.want, got)

			got.Command[0] = "mutated"
			assert.NotEqual(t, "mutated", p.AgentFor(tc.typ).Command[0], "the profile's command is not shared")
		})
	}
}

// TestLoadReadsTheWorkforceKeys: a group's agent and [spawn] reach the
// profile, from any layer, --set included; a tabs project's short code and
// layout do too.
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

	got, err := settings.Load(settings.LoadOptions{
		ConfigDir: dir, Project: "app", LookupEnv: envOf(nil),
		Set: []string{"spawn.shell_ready_after_s=20", "groups.research.agent.model=o3"},
	})
	require.NoError(t, err)
	p := got.Profile
	assert.Equal(t, []settings.Team{{Project: "app", Short: "app", Layout: settings.LayoutTabs}}, got.Teams)
	assert.Equal(t, settings.Spawn{ShellPrompt: `❯\s*$`, ShellReadyKeys: []string{"enter"}, ShellReadyAfter: 20 * time.Second}, p.Spawn, "--set over the project's wait")
	assert.Equal(t, settings.Agent{Command: []string{"codex", "--no-daemon", "--sandbox", "read-only"}, Model: "gpt-5"}, p.AgentFor("review"))
	assert.Equal(t, settings.Agent{Command: []string{"codex", "--search"}, Model: "o3"}, p.AgentFor("research"), "--set adds a model to the project's command")
	assert.Equal(t, settings.Agent{Command: []string{"claude"}, Model: "sonnet"}, p.AgentFor("qa"), "the repo file sets a group's agent too")
	assert.Equal(t, settings.Agent{Command: []string{"claude"}}, p.AgentFor("coder"))
}

// TestLoadedProfileSharesNoSlice: the profile's shell_ready_keys and a
// group's command are its own copies, so a caller that changes them leaves
// the resolved tree, which config resolve prints, as it was.
func TestLoadedProfileSharesNoSlice(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, `schema_version = 1
[spawn]
shell_ready_keys = ["enter"]
[projects.app]
repo = "REPO"
[projects.app.groups.research]
workers = 0
slots = 1
agent = { command = ["codex", "--search"] }
`, repo)
	got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.NoError(t, err)

	got.Profile.Spawn.ShellReadyKeys[0] = "mutated"
	got.Profile.Groups["research"].Agent.Command[0] = "mutated"
	keys, ok := leaf(t, got, "spawn.shell_ready_keys")
	require.True(t, ok)
	assert.Equal(t, []string{"enter"}, keys.Value, "[spawn]'s keys are not shared")
	command, ok := leaf(t, got, "groups.research.agent.command")
	require.True(t, ok)
	assert.Equal(t, []string{"codex", "--search"}, command.Value, "a group's command is not shared")
}

// earlierGroups sort before qa: coder with its own command, which passes,
// then librarian with a model and no command.
const earlierGroups = "[groups.coder]\nworkers = 1\nslots = 1\nagent = { command = [\"claude\"] }\n" +
	"[groups.librarian]\nworkers = 0\nslots = 1\nagent = { model = \"sonnet\" }\n"

// TestLoadRefusesWorkforceKeys: a group's command bypassing Agentisan, any
// command carrying a flag the workforce sets, a codex command running its
// hooks without their trust, routing on or off (the user's Q9), and a [spawn]
// value that cannot work are each refused with the key that holds them and its
// layer, and a launch flag's refusal never shows the flag's value.
func TestLoadRefusesWorkforceKeys(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		body    string
		wantErr error
		wantAt  string
		// wantNot is part of a value the error must never show: a launch
		// flag's refusal names the flag only.
		wantNot string
		// wantIn is the refusal's own text, where a row pins it.
		wantIn string
	}{
		"a group command that is not claude or codex": {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"aider\"] }\n", wantErr: settings.ErrBypassesAgentisan, wantAt: "groups.qa.agent.command"},
		"a group command with --bare":                 {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\", \"--bare\"] }\n", wantErr: settings.ErrBypassesAgentisan, wantAt: "groups.qa.agent.command"},
		"an empty group command":                      {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [] }\n", wantErr: settings.ErrBypassesAgentisan, wantAt: "groups.qa.agent.command"},
		"a group command with --model":                {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\", \"--model\", \"opus\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command"},
		"a top-level command with -m":                 {body: "[agent]\ncommand = [\"codex\", \"-m\", \"o3\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"a top-level command with --name=":            {body: "[agent]\ncommand = [\"claude\", \"--name=hunter2\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantNot: "hunter2"},
		"a top-level command with -n":                 {body: "[agent]\ncommand = [\"claude\", \"-n\", \"x\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"a short flag's value attached":               {body: "[agent]\ncommand = [\"codex\", \"-mo3secret\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantNot: "o3secret"},
		"a one-byte value attached":                   {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"codex\", \"-n1\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command"},
		// Every group is checked, not only the first: the bad command sorts
		// after a group with a command that passes and one with none.
		"a bad command in a later group": {body: earlierGroups + "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"aider\"] }\n", wantErr: settings.ErrBypassesAgentisan, wantAt: "groups.qa.agent.command"},
		"a launch flag in a later group": {body: earlierGroups + "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\", \"--model=opus-secret\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command", wantNot: "opus-secret"},
		// The check knows no flag's arity, so it refuses a launch flag's name
		// even where it is another flag's value.
		"a launch flag's name as a value": {body: "[agent]\ncommand = [\"claude\", \"--append-system-prompt\", \"--model\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		// Routing's effort and Codex's model settings are the workforce's
		// too (A50), in every form clap takes them.
		"a top-level command with --effort":    {body: "[agent]\ncommand = [\"claude\", \"--effort\", \"high\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"--effort with its value joined":       {body: "[agent]\ncommand = [\"claude\", \"--effort=xhigh-secret\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantNot: "xhigh-secret"},
		"codex's -c model":                     {body: "[agent]\ncommand = [\"codex\", \"-c\", \"model=o3-secret\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantNot: "o3-secret"},
		"codex's -c model_reasoning_effort":    {body: "[agent]\ncommand = [\"codex\", \"-c\", 'model_reasoning_effort=\"high\"']\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"codex's --config model":               {body: "[agent]\ncommand = [\"codex\", \"--config\", \"model=o3\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"codex's --config= joined":             {body: "[agent]\ncommand = [\"codex\", \"--config=model=o3\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"codex's -c= joined":                   {body: "[agent]\ncommand = [\"codex\", \"-c=model_reasoning_effort=low\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"codex's -c attached":                  {body: "[agent]\ncommand = [\"codex\", \"-cmodel=o3\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"codex's -c with a quoted, spaced key": {body: "[agent]\ncommand = [\"codex\", \"-c\", ' \"model\" = o3']\n", wantErr: settings.ErrInvalid, wantAt: "agent.command"},
		"codex by path in a group, with -c":    {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"/opt/bin/codex\", \"--no-daemon\", \"-c\", \"model=o3\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command"},
		"a group agent key it does not know":   {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { kind = \"codex\" }\n", wantErr: settings.ErrUnknownKey},
		// Hook trust stays a one-time human step (A43): a codex command never
		// runs its hooks without it, by the flag or the key, routing on or off.
		"codex's hook-trust bypass":               {body: "[agent]\ncommand = [\"codex\", \"--dangerously-bypass-hook-trust\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantIn: bypassRefusal},
		"the hook-trust bypass in a group":        {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"/opt/bin/codex\", \"--no-daemon\", \"--dangerously-bypass-hook-trust\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command", wantIn: bypassRefusal},
		"the hook-trust bypass with a value":      {body: "[agent]\ncommand = [\"codex\", \"--dangerously-bypass-hook-trust=hunter2\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantNot: "hunter2", wantIn: bypassRefusal},
		"the hook-trust bypass with routing on":   {body: "[agent]\ncommand = [\"codex\", \"--search\", \"--dangerously-bypass-hook-trust\"]\n[routing]\nenabled = true\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantIn: bypassRefusal},
		"the hook-trust bypass as a value":        {body: "[agent]\ncommand = [\"codex\", \"--add-dir\", \"--dangerously-bypass-hook-trust\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantIn: bypassRefusal},
		"codex's -c bypass_hook_trust":            {body: "[agent]\ncommand = [\"codex\", \"-c\", \"bypass_hook_trust=true\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantIn: bypassKeyRefusal},
		"--config= bypass_hook_trust joined":      {body: "[agent]\ncommand = [\"codex\", \"--config=bypass_hook_trust=true\"]\n", wantErr: settings.ErrInvalid, wantAt: "agent.command", wantIn: bypassKeyRefusal},
		"a group's -c bypass_hook_trust attached": {body: "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"codex\", \"-cbypass_hook_trust=true\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.qa.agent.command", wantIn: bypassKeyRefusal},
		// The debugger runs the plugin's launch (A32): its group sets no
		// command, not even claude alone.
		"a debugger group's command":           {body: "[groups.debugger]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\"] }\n", wantErr: settings.ErrInvalid, wantAt: "groups.debugger.agent.command"},
		"an empty shell prompt":                {body: "[spawn]\nshell_prompt = \"\"\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_prompt"},
		"a shell prompt that does not compile": {body: "[spawn]\nshell_prompt = \"(\"\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_prompt"},
		"an empty key name":                    {body: "[spawn]\nshell_ready_keys = [\"enter\", \" \"]\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_ready_keys"},
		"a key name that is a tab":             {body: "[spawn]\nshell_ready_keys = [\"\\t\"]\n", wantErr: settings.ErrInvalid, wantAt: "spawn.shell_ready_keys"},
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
				assert.Contains(t, err.Error(), tc.wantAt+" ("+settings.LayerShared+", ", "the error names the key and its layer")
			}
			if tc.wantNot != "" {
				assert.NotContains(t, err.Error(), tc.wantNot)
			}
			if tc.wantIn != "" {
				assert.Contains(t, err.Error(), tc.wantIn)
			}
		})
	}
}

// bypassRefusal and bypassKeyRefusal are the hook-trust bypass's refusals,
// by the flag and by the -c key.
const (
	bypassRefusal    = "codex's --dangerously-bypass-hook-trust runs enabled hooks without the trust a person gives Codex once (ADR-001 A43): remove it"
	bypassKeyRefusal = "codex's bypass_hook_trust key runs enabled hooks without the trust a person gives Codex once (ADR-001 A43): remove it"
)

// TestLoadAcceptsTheWorkforceEdges: the bounds of the wait, a shell prompt
// of any regular expression, flags that only look like the launch flags or
// codex's hook-trust bypass, codex's flags on a claude command and a path to
// codex all resolve.
func TestLoadAcceptsTheWorkforceEdges(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"no wait at all":         "[spawn]\nshell_ready_after_s = 0\n",
		"slots at the bound":     "[groups.qa]\nworkers = 64\nslots = 64\n",
		"a minute's wait":        "[spawn]\nshell_ready_after_s = 60\n",
		"no keys":                "[spawn]\nshell_ready_keys = []\n",
		"a non-ASCII prompt":     "[spawn]\nshell_prompt = '➜\\s+\\S+'\n",
		"a look-alike of a flag": "[agent]\ncommand = [\"claude\", \"--models\", \"--model-x\", \"-x\", \"-é\", \"-\"]\n",
		// Neither agent reads an abbreviated long flag as the whole one:
		// claude's parser matches long names exactly, and codex refuses
		// --mod as an unexpected argument (A53).
		"an abbreviation of --model":  "[agent]\ncommand = [\"claude\", \"--mod\", \"o3\"]\n",
		"a look-alike of --effort":    "[agent]\ncommand = [\"claude\", \"--efforts\", \"--effort-x\"]\n",
		"another of codex's settings": "[agent]\ncommand = [\"codex\", \"-c\", \"model_provider=oss\", \"--config=profiles.x.model=y\", \"-cmodel_verbosity=low\"]\n",
		"codex's -c with no value":    "[agent]\ncommand = [\"codex\", \"-c\"]\n",
		// The hook-trust refusal is codex's only, and matches the flag and the
		// key whole.
		"claude with codex's hook-trust bypass": "[agent]\ncommand = [\"claude\", \"--dangerously-bypass-hook-trust\", \"-c\", \"bypass_hook_trust=true\"]\n",
		"a look-alike of the hook-trust bypass": "[agent]\ncommand = [\"codex\", \"--dangerously-bypass-hook-trusts\", \"--dangerously-bypass-hook\", \"--Dangerously-Bypass-Hook-Trust\", \"-c\", \"bypass_hook_trust_x=1\"]\n",
		// claude's -c is --continue, which takes no value.
		"claude's own -c": "[agent]\ncommand = [\"claude\", \"-c\", \"model=x\"]\n",
		// A command named by a path resolves, and spawn refuses it before it
		// opens anything, since herdr runs its own executable from the
		// shell's PATH (A23).
		"codex as a group's own agent": "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"/opt/bin/codex\"] }\n",
		// argv[0] is the program, which D12 checks by its base name; spawn
		// refuses it as a path too.
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

// TestLoadRefusesADebuggerCommandFromAnyLayer: a project's table or --set
// cannot give the debugger a command either, while a model alone resolves.
func TestLoadRefusesADebuggerCommandFromAnyLayer(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n[projects.app.groups.debugger]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\"] }\n", repo)
	_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "groups.debugger.agent.command")

	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\n[projects.app.groups.debugger]\nworkers = 0\nslots = 1\nagent = { model = \"sonnet\" }\n", repo)
	res, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.NoError(t, err)
	assert.Equal(t, settings.Agent{Command: []string{"claude"}, Model: "sonnet"}, res.Profile.AgentFor("debugger"))

	_, err = settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil), Set: []string{`groups.debugger.agent.command=["claude"]`}})
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "groups.debugger.agent.command")
}

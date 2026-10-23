package settings_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestDefaultProvider: a type's default provider is claude for the
// debugger; else its group's provider; else its group's own command's; else
// codex for review; else the top-level command's, "" with none (A62).
func TestDefaultProvider(t *testing.T) {
	t.Parallel()
	claude := settings.Agent{Command: []string{"claude"}}
	tests := map[string]struct {
		top    settings.Agent
		groups map[string]settings.Group
		typ    string
		want   string
	}{
		"the top-level command's":             {top: claude, typ: "coder", want: "claude"},
		"a codex top level":                   {top: settings.Agent{Command: []string{"codex", "--search"}}, typ: "coder", want: "codex"},
		"by its base name":                    {top: settings.Agent{Command: []string{"/usr/bin/codex"}}, typ: "coder", want: "codex"},
		"no top-level command":                {typ: "coder", want: ""},
		"the group's own command":             {top: claude, groups: map[string]settings.Group{"qa": {Agent: settings.Agent{Command: []string{"codex"}}}}, typ: "qa", want: "codex"},
		"the group's provider":                {top: claude, groups: map[string]settings.Group{"coder": {Provider: "codex"}}, typ: "coder", want: "codex"},
		"the provider over the group command": {top: claude, groups: map[string]settings.Group{"qa": {Provider: "claude", Agent: settings.Agent{Command: []string{"codex"}}}}, typ: "qa", want: "claude"},
		"review is codex":                     {top: claude, typ: "review", want: "codex"},
		"review's group says otherwise":       {top: claude, groups: map[string]settings.Group{"review": {Provider: "claude"}}, typ: "review", want: "claude"},
		"the debugger is claude":              {top: settings.Agent{Command: []string{"codex"}}, groups: map[string]settings.Group{"debugger": {Provider: "codex"}}, typ: "debugger", want: "claude"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := settings.Profile{Agent: tc.top, Groups: tc.groups}
			assert.Equal(t, tc.want, p.DefaultProvider(tc.typ))
		})
	}
}

// TestAgentForProviders: under a provider, a type runs its group's command
// for it; else its group's own command when it is the provider's; else, for
// review under codex, the built-in read-only codex, which outranks a
// top-level [agent.codex]; else the top-level command for the provider;
// else the top-level command when it is the provider's; else the provider
// bare. Its model is its group's for the provider, else its group's when the
// provider is the type's default, else, only with a top-level command, the
// top level's for the provider (A62).
func TestAgentForProviders(t *testing.T) {
	t.Parallel()
	top := settings.Agent{Command: []string{"claude", "--setting-sources", "user,project"}, Model: "opus"}
	tops := map[string]settings.Agent{
		"codex":  {Command: []string{"codex", "--search"}, Model: "gpt-6.1-sol"},
		"claude": {Command: []string{"claude", "--verbose"}, Model: "sonnet"},
	}
	review := []string{"codex", "--no-daemon", "--sandbox", "read-only"}
	tests := map[string]struct {
		providers map[string]settings.Agent
		groups    map[string]settings.Group
		typ, prov string
		want      settings.Agent
	}{
		"codex with no codex table is codex bare":      {typ: "coder", prov: "codex", want: settings.Agent{Command: []string{"codex"}}},
		"claude is the top-level command, its model":   {typ: "coder", prov: "claude", want: top},
		"the top-level codex table":                    {providers: tops, typ: "coder", prov: "codex", want: tops["codex"]},
		"the top-level claude table outranks agent":    {providers: tops, typ: "coder", prov: "claude", want: tops["claude"]},
		"a top-level table's command with agent.model": {providers: map[string]settings.Agent{"claude": {Command: []string{"claude", "--verbose"}}}, typ: "coder", prov: "claude", want: settings.Agent{Command: []string{"claude", "--verbose"}, Model: "opus"}},
		"the group's table for the provider": {
			providers: tops, groups: map[string]settings.Group{"coder": {Providers: map[string]settings.Agent{"codex": {Command: []string{"codex", "-c", "x=1"}, Model: "gpt-5.5"}}}},
			typ: "coder", prov: "codex", want: settings.Agent{Command: []string{"codex", "-c", "x=1"}, Model: "gpt-5.5"},
		},
		"the group's table model with a top command": {
			providers: tops, groups: map[string]settings.Group{"coder": {Providers: map[string]settings.Agent{"codex": {Model: "gpt-5.5"}}}},
			typ: "coder", prov: "codex", want: settings.Agent{Command: tops["codex"].Command, Model: "gpt-5.5"},
		},
		"the group's own command when it is the provider's": {
			providers: tops, groups: map[string]settings.Group{"qa": {Agent: settings.Agent{Command: []string{"codex", "--oss"}}}},
			typ: "qa", prov: "codex", want: settings.Agent{Command: []string{"codex", "--oss"}},
		},
		"not when it is the other provider's": {
			groups: map[string]settings.Group{"qa": {Agent: settings.Agent{Command: []string{"codex"}, Model: "gpt-5"}}},
			typ:    "qa", prov: "claude", want: top,
		},
		"the group's model goes with its default provider": {
			groups: map[string]settings.Group{"coder": {Provider: "codex", Agent: settings.Agent{Model: "gpt-5"}}},
			typ:    "coder", prov: "codex", want: settings.Agent{Command: []string{"codex"}, Model: "gpt-5"},
		},
		"and not with the other": {
			groups: map[string]settings.Group{"coder": {Provider: "codex", Agent: settings.Agent{Model: "gpt-5"}}},
			typ:    "coder", prov: "claude", want: top,
		},
		"the top-level model is the top command's provider's": {
			typ: "coder", prov: "codex", want: settings.Agent{Command: []string{"codex"}},
		},
		"review's built-in outranks the top-level codex table": {
			providers: tops, typ: "review", prov: "codex", want: settings.Agent{Command: review},
		},
		"review's group table outranks its built-in": {
			providers: tops, groups: map[string]settings.Group{"review": {Providers: map[string]settings.Agent{"codex": {Command: []string{"codex", "-s", "read-only"}, Model: "gpt-5"}}}},
			typ: "review", prov: "codex", want: settings.Agent{Command: []string{"codex", "-s", "read-only"}, Model: "gpt-5"},
		},
		"review's built-in takes its group's codex model": {
			providers: tops, groups: map[string]settings.Group{"review": {Providers: map[string]settings.Agent{"codex": {Model: "gpt-5"}}}},
			typ: "review", prov: "codex", want: settings.Agent{Command: review, Model: "gpt-5"},
		},
		"the debugger takes its group's claude model first": {
			providers: tops, groups: map[string]settings.Group{"debugger": {Agent: settings.Agent{Model: "haiku"}, Providers: map[string]settings.Agent{"claude": {Model: "fable"}}}},
			typ: "debugger", prov: "claude", want: settings.Agent{Command: []string{"claude"}, Model: "fable"},
		},
		"then its group's model": {
			providers: tops, groups: map[string]settings.Group{"debugger": {Agent: settings.Agent{Model: "haiku"}}},
			typ: "debugger", prov: "claude", want: settings.Agent{Command: []string{"claude"}, Model: "haiku"},
		},
		"then the top-level claude model, never its command": {
			providers: tops, typ: "debugger", prov: "", want: settings.Agent{Command: []string{"claude"}, Model: "sonnet"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := settings.Profile{Agent: top, Providers: tc.providers, Groups: tc.groups}
			got, err := p.AgentFor(tc.typ, tc.prov)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			got.Command[0] = "mutated"
			again, err := p.AgentFor(tc.typ, tc.prov)
			require.NoError(t, err)
			assert.NotEqual(t, "mutated", again.Command[0], "the profile's command is not shared")
		})
	}
}

// TestAgentForRefuses: a provider that is neither claude nor codex is
// invalid; the debugger refuses codex and review refuses claude, whether
// asked for or the type's default, each a gap that names what lifts it
// (A62).
func TestAgentForRefuses(t *testing.T) {
	t.Parallel()
	top := settings.Agent{Command: []string{"claude"}}
	tests := map[string]struct {
		groups    map[string]settings.Group
		typ, prov string
		want      error
		says      string
	}{
		"a provider neither claude nor codex": {typ: "coder", prov: "gemini", want: settings.ErrInvalid, says: `"gemini" is neither claude nor codex`},
		"a provider's case is exact":          {typ: "coder", prov: "Codex", want: settings.ErrInvalid},
		"a provider with white space":         {typ: "coder", prov: " codex", want: settings.ErrInvalid},
		"a provider like a flag":              {typ: "coder", prov: "-codex", want: settings.ErrInvalid},
		"a provider not ASCII":                {typ: "coder", prov: "cödex", want: settings.ErrInvalid},
		"a provider with a bad byte":          {typ: "coder", prov: "codex\xff", want: settings.ErrInvalid},
		"the debugger under codex":            {typ: "debugger", prov: "codex", want: settings.ErrProviderRefused, says: "NERD-5530"},
		"review under claude":                 {typ: "review", prov: "claude", want: settings.ErrProviderRefused, says: "NERD-5529"},
		"review's group provider is claude":   {groups: map[string]settings.Group{"review": {Provider: "claude"}}, typ: "review", want: settings.ErrProviderRefused, says: "NERD-5529"},
		"review's own command is claude":      {groups: map[string]settings.Group{"review": {Agent: settings.Agent{Command: []string{"claude"}}}}, typ: "review", want: settings.ErrProviderRefused, says: "read-only Claude review launch"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := settings.Profile{Agent: top, Groups: tc.groups}
			_, err := p.AgentFor(tc.typ, tc.prov)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), tc.says)
			if errors.Is(err, settings.ErrProviderRefused) {
				assert.Contains(t, err.Error(), "a gap, not a rule", "the refusal says it is temporary")
			}
		})
	}
	p := settings.Profile{Agent: top}
	for _, typ := range []string{"Debugger", "Review", "coder"} {
		for _, prov := range []string{"claude", "codex"} {
			_, err := p.AgentFor(typ, prov)
			require.NoError(t, err, "%s under %s: the refusals match the type exactly", typ, prov)
		}
	}
}

// TestModelFamily: a model's name says which provider runs it when it is a
// name the plugin knows, matched without regard to case; any other name
// belongs to neither, never guessed at.
func TestModelFamily(t *testing.T) {
	t.Parallel()
	for model, want := range map[string]string{
		"claude-opus-5-5":                     "claude",
		"claude-opus-5-5[1m]":                 "claude",
		"claude-sonnet-5-5@20261001":          "claude",
		"anthropic.claude-opus-5-5-v1:0":      "claude",
		"us.anthropic.claude-sonnet-5-5-v1:0": "claude",
		"opus":                                "claude",
		"Opus":                                "claude",
		"opus[1m]":                            "claude",
		"sonnet":                              "claude",
		"haiku":                               "claude",
		"fable":                               "claude",
		"opusplan":                            "claude",
		"gpt-6.1-sol":                         "codex",
		"GPT-5.5":                             "codex",
		"gpt-5.5-codex":                       "codex",
		"gpt-oss-20b":                         "codex",
		"codex-mini-latest":                   "codex",
		"o3":                                  "codex",
		"o4-mini":                             "codex",
		"":                                    "",
		"default":                             "",
		"anthropic/claude-opus-5-5":           "",
		"llama3":                              "",
		"opus-like":                           "",
		"claude":                              "",
		"gpt":                                 "",
		"oz":                                  "",
		"my-gpt-5":                            "",
		// Odd bytes and sizes: the match is on the name's leading ASCII,
		// so an invalid byte, a NUL or a BOM before it makes no family, and
		// one after it keeps the family its prefix names.
		"\xffgpt-5":                        "",
		"\ufeffgpt-5":                      "",
		"ＧＰＴ-5":                            "",
		"gpt-5\x00":                        "codex",
		"claude-\xff":                      "claude",
		"CLAUDE-OPUS-5-5":                  "claude",
		"opus[1M]":                         "claude",
		"O3":                               "codex",
		"o":                                "",
		"gpt-":                             "codex",
		strings.Repeat("x", 4096):          "",
		"gpt-" + strings.Repeat("5", 4096): "codex",
	} {
		assert.Equal(t, want, settings.ModelFamily(model), model)
	}
}

// TestLoadReadsTheProviderTables: [agent.<provider>], a group's provider
// and a group's [agent.<provider>] reach the profile from any layer, a repo
// file's models included, and a type then runs its provider's command and
// model (A62).
func TestLoadReadsTheProviderTables(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, `schema_version = 1
[agent.codex]
command = ["codex", "--search"]
model = "gpt-6.1-sol"

[projects.app]
repo = "REPO"

[projects.app.groups.coder]
workers = 1
slots = 2
agent = { provider = "codex" }

[projects.app.groups.qa]
workers = 1
slots = 1
[projects.app.groups.qa.agent.codex]
command = ["codex", "-c", "x=1"]
`, repo)
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n[groups.qa.agent.codex]\nmodel = \"gpt-5.5\"\n[agent.claude]\nmodel = \"sonnet\"\n", repo)

	got, err := settings.Load(settings.LoadOptions{
		ConfigDir: dir, Project: "app", LookupEnv: envOf(nil),
		Set: []string{"groups.coder.agent.codex.model=gpt-5.6-sol"},
	})
	require.NoError(t, err)
	p := *got.Profile
	assert.Equal(t, "codex", p.DefaultProvider("coder"))
	assert.Equal(t, settings.Agent{Command: []string{"codex", "--search"}, Model: "gpt-5.6-sol"}, agentFor(t, p, "coder"), "--set gives the coder's codex its model")
	qa, err := p.AgentFor("qa", "codex")
	require.NoError(t, err)
	assert.Equal(t, settings.Agent{Command: []string{"codex", "-c", "x=1"}, Model: "gpt-5.5"}, qa, "the repo file sets a provider's model")
	assert.Equal(t, settings.Agent{Command: []string{"claude"}, Model: "sonnet"}, agentFor(t, p, "qa"), "[agent.claude]'s model with the claude command")
	require.NoError(t, p.CheckModelProviders())
}

// TestLoadRefusesProviderTables: each provider's command is D12's, carries
// no flag the workforce sets and runs that provider bare, flags only, since
// herdr runs the agent by kind; a group's provider is claude or codex and its
// own command's; the debugger takes no codex and no command, and review no
// claude, from whichever key would choose it; with routing on no provider's
// model is set nor a codex profile chosen; a repo file sets no provider and no
// command; a group that only describes takes no provider's table (A62).
func TestLoadRefusesProviderTables(t *testing.T) {
	t.Parallel()
	group := func(name, body string) string {
		return "[projects.app.groups." + name + "]\nworkers = 0\nslots = 1\n" + body
	}
	tests := map[string]struct {
		shared, repoFile string
		want             error
		key, says        string
	}{
		"a codex command that runs claude": {shared: "[agent.codex]\ncommand = [\"claude\"]\n", want: settings.ErrInvalid, key: "agent.codex.command", says: "names codex bare, then flags only"},
		"a provider command by its path":   {shared: "[agent.claude]\ncommand = [\"/usr/bin/claude\"]\n", want: settings.ErrInvalid, key: "agent.claude.command", says: "the command runs /usr/bin/claude"},
		"another program":                  {shared: "[agent.codex]\ncommand = [\"bash\"]\n", want: settings.ErrBypassesAgentisan, key: "agent.codex.command"},
		"a bare claude":                    {shared: "[agent.claude]\ncommand = [\"claude\", \"--bare\"]\n", want: settings.ErrBypassesAgentisan, key: "agent.claude.command"},
		"a model flag":                     {shared: "[agent.codex]\ncommand = [\"codex\", \"-m\", \"o3\"]\n", want: settings.ErrInvalid, key: "agent.codex.command", says: "-m is set by the workforce"},
		"codex's routed effort":            {shared: group("qa", "[projects.app.groups.qa.agent.codex]\ncommand = [\"codex\", \"-c\", \"model_reasoning_effort=high\"]\n"), want: settings.ErrInvalid, key: "groups.qa.agent.codex.command"},
		"a group's codex table running claude": {
			shared: group("qa", "[projects.app.groups.qa.agent.codex]\ncommand = [\"claude\"]\n"), want: settings.ErrInvalid, key: "groups.qa.agent.codex.command",
		},
		"a provider neither claude nor codex":   {shared: group("qa", "agent = { provider = \"gemini\" }\n"), want: settings.ErrInvalid, key: "groups.qa.agent.provider", says: `"gemini" is neither claude nor codex`},
		"a provider's case":                     {shared: group("qa", "agent = { provider = \"Codex\" }\n"), want: settings.ErrInvalid, key: "groups.qa.agent.provider", says: `"Codex" is neither`},
		"an empty provider":                     {shared: group("qa", "agent = { provider = \"\" }\n"), want: settings.ErrInvalid, key: "groups.qa.agent.provider", says: `"" is neither`},
		"an empty provider command":             {shared: "[agent.codex]\ncommand = []\n", want: settings.ErrBypassesAgentisan, key: "agent.codex.command", says: "agent.command is empty"},
		"a provider command by a relative path": {shared: "[agent.codex]\ncommand = [\"../codex\"]\n", want: settings.ErrInvalid, key: "agent.codex.command", says: "the command runs ../codex"},
		"a provider command like a flag":        {shared: "[agent.claude]\ncommand = [\"-m/claude\"]\n", want: settings.ErrInvalid, key: "agent.claude.command", says: "the command runs -m/claude"},
		"a provider against the group command":  {shared: group("qa", "agent = { provider = \"claude\", command = [\"codex\"] }\n"), want: settings.ErrInvalid, key: "groups.qa.agent.provider", says: "the group's own command runs codex"},
		"the debugger's provider codex":         {shared: group("debugger", "agent = { provider = \"codex\" }\n"), want: settings.ErrProviderRefused, key: "groups.debugger.agent.provider", says: "NERD-5530"},
		"a debugger codex model":                {shared: group("debugger", "[projects.app.groups.debugger.agent.codex]\nmodel = \"gpt-5\"\n"), want: settings.ErrProviderRefused, key: "groups.debugger.agent.codex.model", says: "NERD-5530"},
		"a debugger claude command":             {shared: group("debugger", "[projects.app.groups.debugger.agent.claude]\ncommand = [\"claude\"]\n"), want: settings.ErrInvalid, key: "groups.debugger.agent.claude.command", says: "sets no command"},
		"review's provider claude":              {shared: group("review", "agent = { provider = \"claude\" }\n"), want: settings.ErrProviderRefused, key: "groups.review.agent.provider", says: "NERD-5529"},
		"review's own command claude":           {shared: group("review", "agent = { command = [\"claude\"] }\n"), want: settings.ErrProviderRefused, key: "groups.review.agent.command", says: "read-only Claude review launch"},
		"a review claude model":                 {shared: group("review", "[projects.app.groups.review.agent.claude]\nmodel = \"sonnet\"\n"), want: settings.ErrProviderRefused, key: "groups.review.agent.claude.model"},
		"a provider's model with routing on":    {shared: "[routing]\nenabled = true\n[agent.codex]\nmodel = \"gpt-5\"\n", want: settings.ErrInvalid, key: "agent.codex.model", says: "routing decides the model"},
		"a group provider's model with routing on": {
			shared: "[routing]\nenabled = true\n" + group("qa", "[projects.app.groups.qa.agent.claude]\nmodel = \"sonnet\"\n"), want: settings.ErrInvalid, key: "groups.qa.agent.claude.model", says: "routing decides the model",
		},
		"a codex profile with routing on": {shared: "[routing]\nenabled = true\n[agent.codex]\ncommand = [\"codex\", \"-p\", \"fast\"]\n", want: settings.ErrInvalid, key: "agent.codex.command", says: "chooses a profile"},
		"a provider's unsafe model":       {shared: "[agent.codex]\nmodel = \"gpt-5; rm\"\n", want: settings.ErrInvalid, key: "agent.codex.model", says: "a model is a letter or digit"},
		"a repo file's provider":          {repoFile: "[groups.qa]\nworkers = 1\nslots = 1\nagent = { provider = \"codex\" }\n", want: settings.ErrInvalid, key: "groups.qa.agent.provider (repo, "},
		"a repo file's provider command":  {repoFile: "[agent.codex]\ncommand = [\"codex\"]\n", want: settings.ErrInvalid, key: "agent.codex.command (repo, "},
		"a describing group's codex table": {
			shared: "[projects.app.groups.notes]\ndescription = \"notes\"\n[projects.app.groups.notes.agent.codex]\nmodel = \"gpt-5\"\n", want: settings.ErrInvalid, key: "groups.notes.agent.codex", says: "only describes",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			shared := tc.shared
			project := "[projects.app]\nrepo = \"REPO\"\n"
			if strings.Contains(shared, "[projects.app.") {
				shared = strings.Replace(shared, "[projects.app.", project+"[projects.app.", 1)
			} else {
				shared += project
			}
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+shared, repo)
			if tc.repoFile != "" {
				writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+tc.repoFile, repo)
			}
			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), tc.key)
			assert.Contains(t, err.Error(), tc.says)
			if errors.Is(err, settings.ErrProviderRefused) {
				require.ErrorIs(t, err, settings.ErrInvalid, "a refusal at load is the config's")
			}
		})
	}
}

// TestCheckModelProviders: config resolve --project refuses a model whose
// name says the other provider runs it than its table is for, naming the key
// and both providers and never the model; a model it does not know passes,
// and Load never runs it (A62).
func TestCheckModelProviders(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		body string
		// key is the key refused, "" when the profile passes.
		key, says string
	}{
		"agent.model against the top command": {body: "[agent]\ncommand = [\"codex\"]\nmodel = \"opus\"\n", key: "agent.model", says: "a claude model, and the agent it is for runs codex"},
		"a codex table's claude model":        {body: "[agent.codex]\nmodel = \"claude-opus-5-5\"\n", key: "agent.codex.model", says: "a claude model, and the agent it is for runs codex"},
		"a claude table's codex model":        {body: "[agent.claude]\nmodel = \"o3\"\n", key: "agent.claude.model", says: "a codex model, and the agent it is for runs claude"},
		"a group model against its provider":  {body: "[projects.app.groups.coder]\nworkers = 1\nslots = 1\nagent = { provider = \"codex\", model = \"sonnet\" }\n", key: "groups.coder.agent.model", says: "runs codex"},
		"a group's table":                     {body: "[projects.app.groups.qa]\nworkers = 1\nslots = 1\n[projects.app.groups.qa.agent.claude]\nmodel = \"gpt-6.1-sol\"\n", key: "groups.qa.agent.claude.model", says: "a codex model"},
		"review's model":                      {body: "[projects.app.groups.review]\nworkers = 0\nslots = 1\nagent = { model = \"fable\" }\n", key: "groups.review.agent.model", says: "runs codex"},
		"the debugger's model":                {body: "[projects.app.groups.debugger]\nworkers = 0\nslots = 1\nagent = { model = \"gpt-5\" }\n", key: "groups.debugger.agent.model", says: "runs claude"},
		"models the providers run":            {body: "[agent]\nmodel = \"opus\"\n[agent.codex]\nmodel = \"gpt-6.1-sol\"\n[projects.app.groups.review]\nworkers = 0\nslots = 1\nagent = { model = \"gpt-5\" }\n"},
		"a model it does not know":            {body: "[agent.codex]\nmodel = \"llama3\"\n[agent.claude]\nmodel = \"my-model\"\n"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			body := tc.body
			project := "[projects.app]\nrepo = \"REPO\"\n"
			if strings.Contains(body, "[projects.app.") {
				body = strings.Replace(body, "[projects.app.", project+"[projects.app.", 1)
			} else {
				body += project
			}
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+body, repo)
			res, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
			require.NoError(t, err, "Load never refuses a model's family")

			err = res.Profile.CheckModelProviders()
			if tc.key == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Contains(t, err.Error(), tc.key+": ")
			assert.Contains(t, err.Error(), tc.says)
			for _, model := range []string{"opus", "claude-opus-5-5", "o3", "sonnet", "gpt-6.1-sol", "fable", "gpt-5"} {
				assert.NotContains(t, err.Error(), `"`+model+`"`, "the error never quotes the model")
			}
		})
	}
}

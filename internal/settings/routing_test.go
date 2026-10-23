package settings_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// routingApp is a project for the routing tests to select.
const routingApp = "[projects.app]\nrepo = \"REPO\"\n"

//go:fix inline

// loadRouting loads a shared file, a repo file and --set for project app,
// or for no project when project is false.
func loadRouting(t *testing.T, file, repoFile string, set []string, project bool) (settings.Resolved, error) {
	t.Helper()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n"+file+routingApp, repo)
	if repoFile != "" {
		writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+repoFile, repo)
	}
	opts := settings.LoadOptions{ConfigDir: dir, Set: set, LookupEnv: envOf(nil)}
	if project {
		opts.Project = "app"
	}
	return settings.Load(opts)
}

// TestRoutingResolves: [routing] from the shared file and --set resolves
// into the deployment routing takes, each leaf with its layer, and a
// deployment that sets nothing is routing off.
func TestRoutingResolves(t *testing.T) {
	t.Parallel()
	file := "[routing]\nenabled = true\n" +
		"[routing.bindings.claude_mid]\nmodel_id = \"claude-sonnet-5-5-20261001\"\nefforts = [\"medium\", \"high\"]\n" +
		"[routing.bindings.codex_small]\nenabled = false\n" +
		"[routing.bindings.claude_small]\n" +
		"[routing.overrides]\nalex_debugger = \"claude_premium\"\n"
	for _, project := range []bool{false, true} {
		res, err := loadRouting(t, file, "", []string{"routing.bindings.codex_premium.model_id="}, project)
		require.NoError(t, err)
		assert.Equal(t, routing.Deployment{
			Enabled: true,
			Bindings: map[string]routing.Facts{
				"claude_mid":    {ModelID: new("claude-sonnet-5-5-20261001"), Efforts: []string{"medium", "high"}},
				"codex_small":   {Enabled: new(false)},
				"claude_small":  {},
				"codex_premium": {ModelID: new("")},
			},
			Overrides: map[string]string{"alex_debugger": "claude_premium"},
		}, res.Routing())
		for path, layer := range map[string]string{
			"routing.enabled":                         settings.LayerShared,
			"routing.bindings.claude_mid.model_id":    settings.LayerShared,
			"routing.overrides.alex_debugger":         settings.LayerShared,
			"routing.bindings.codex_premium.model_id": settings.LayerFlag,
		} {
			assert.Equal(t, layer, res.Sources[path], path)
		}
		_, err = routing.New(routing.V1(), res.Routing())
		require.NoError(t, err, "what Load passes, routing takes")
	}
	res, err := loadRouting(t, "", "", nil, true)
	require.NoError(t, err)
	assert.Equal(t, routing.Deployment{}, res.Routing(), "no [routing] is routing off")
	res, err = loadRouting(t, "[routing]\n", "", []string{"routing.enabled=true"}, true)
	require.NoError(t, err)
	assert.True(t, res.Routing().Enabled, "--set turns it on")
	assert.Equal(t, settings.LayerFlag, res.Sources["routing.enabled"])
}

// TestRoutingRefuses: a [routing] key the rules do not have, a fact or an
// override they do not allow, [routing] outside the shared file and --set,
// an override from --set, and a model key while routing decides the model
// are each refused, naming the key and its layer.
func TestRoutingRefuses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		file, repoFile string
		set            []string
		project        bool
		wantErr        error
		wantIn         []string
	}{
		"a binding the rules do not have":           {file: "[routing.bindings.claude_huge]\nmodel_id = \"x\"\n", wantErr: settings.ErrUnknownKey, wantIn: []string{"routing.bindings.claude_huge"}},
		"a binding key it does not know":            {file: "[routing.bindings.claude_mid]\nalias = \"Sonnet\"\n", wantErr: settings.ErrUnknownKey},
		"a role the rules do not have":              {file: "[routing.overrides]\nqa = \"claude_mid\"\n", wantErr: settings.ErrUnknownKey, wantIn: []string{"routing.overrides.qa"}},
		"a [routing] key it does not know":          {file: "[routing]\nprovider = \"claude\"\n", wantErr: settings.ErrUnknownKey},
		"enabled as a string":                       {file: "[routing]\nenabled = \"true\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"a boolean"}},
		"efforts as a string":                       {file: "[routing.bindings.claude_mid]\nefforts = \"high\"\n", wantErr: settings.ErrInvalid},
		"a model id that is a flag":                 {file: "[routing.bindings.claude_mid]\nmodel_id = \"--bare\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.bindings.claude_mid.model_id (shared, "}},
		"an effort no client takes":                 {file: "[routing.bindings.claude_mid]\nefforts = [\"turbo\"]\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.bindings.claude_mid.efforts (shared, "}},
		"no efforts":                                {file: "[routing.bindings.claude_mid]\nefforts = []\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.bindings.claude_mid.efforts"}},
		"a model id from --set that is a flag":      {set: []string{"routing.bindings.claude_mid.model_id=-x"}, wantErr: settings.ErrInvalid, wantIn: []string{"(flag, --set)"}},
		"an override the role does not allow":       {file: "[routing.overrides]\nclerk = \"claude_strong\"\n", wantErr: routing.ErrOverrideRefused, wantIn: []string{"routing.overrides.clerk (shared, "}},
		"a premium override for the boss":           {file: "[routing.overrides]\nboss = \"claude_premium\"\n", wantErr: settings.ErrInvalid},
		"an override from --set":                    {set: []string{"routing.overrides.alex_debugger=claude_premium"}, wantErr: settings.ErrInvalid, wantIn: []string{"never by --set"}},
		"an empty overrides table from --set":       {set: []string{"routing.overrides={}"}, wantErr: settings.ErrInvalid, wantIn: []string{"never by --set"}},
		"[routing] in a project table":              {file: "[projects.other]\nrepo = \"REPO\"\n[projects.other.routing]\nenabled = true\n", wantErr: settings.ErrUnknownKey},
		"[routing] in a repo file":                  {repoFile: "[routing]\nenabled = true\n", project: true, wantErr: settings.ErrUnknownKey},
		"an override in a repo file":                {repoFile: "[routing.overrides]\nalex_debugger = \"claude_premium\"\n", project: true, wantErr: settings.ErrUnknownKey},
		"the top-level model while routing is on":   {file: "[agent]\nmodel = \"opus\"\n[routing]\nenabled = true\n", wantErr: settings.ErrInvalid, wantIn: []string{"agent.model (shared, ", "routing decides the model"}},
		"a group's model while routing is on":       {file: "[routing]\nenabled = true\n[groups.qa]\nworkers = 0\nslots = 1\nagent = { model = \"sonnet\" }\n", wantErr: settings.ErrInvalid, wantIn: []string{"groups.qa.agent.model (shared, "}},
		"a later group's model while routing is on": {file: "[routing]\nenabled = true\n[groups.coder]\nworkers = 0\nslots = 1\n[groups.research]\nworkers = 0\nslots = 1\nagent = { command = [\"codex\"], model = \"o3\" }\n", wantErr: settings.ErrInvalid, wantIn: []string{"groups.research.agent.model"}},
		"a model with routing on from --set":        {file: "[agent]\nmodel = \"opus\"\n", set: []string{"routing.enabled=true"}, wantErr: settings.ErrInvalid, wantIn: []string{"agent.model (shared, "}},
		"a model from --set while routing is on":    {file: "[routing]\nenabled = true\n", set: []string{"agent.model=opus"}, wantErr: settings.ErrInvalid, wantIn: []string{"agent.model (flag, --set)"}},
		"a project's model while routing is on":     {file: "[routing]\nenabled = true\n[projects.app.agent]\nmodel = \"opus\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"projects.app.agent.model"}},
		"a repo file's model while routing is on":   {file: "[routing]\nenabled = true\n", repoFile: "[agent]\nmodel = \"opus\"\n", project: true, wantErr: settings.ErrInvalid, wantIn: []string{"agent.model (repo, "}},
		"a repo file's group model while routing is on (A46)": {
			file: "[routing]\nenabled = true\n", repoFile: "[groups.qa]\nworkers = 1\nslots = 1\nagent = { model = \"opus\" }\n", project: true,
			wantErr: settings.ErrInvalid, wantIn: []string{"groups.qa.agent.model (repo, "},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadRouting(t, tc.file, tc.repoFile, tc.set, tc.project)
			require.ErrorIs(t, err, tc.wantErr)
			for _, s := range tc.wantIn {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
}

// TestRoutingRefusesACodexProfile: while routing is on, a codex command
// choosing a profile, which can set the model and effort routing decides,
// is refused in each form, the [agent] command's and a group's alike, by
// its flag and never its profile. With routing off it resolves, and
// claude's -p, its print mode, is never a profile.
func TestRoutingRefusesACodexProfile(t *testing.T) {
	t.Parallel()
	forms := map[string]struct {
		args []string
		flag string
	}{
		"-p":                    {[]string{"-p", "mine"}, "-p"},
		"-p attached":           {[]string{"-pmine"}, "-p"},
		"-p joined":             {[]string{"-p=mine"}, "-p"},
		"--profile":             {[]string{"--profile", "mine"}, "--profile"},
		"--profile joined":      {[]string{"--profile=mine"}, "--profile"},
		"-c profile":            {[]string{"-c", "profile=mine"}, "profile key"},
		"--config profile":      {[]string{"--config=profile=mine"}, "profile key"},
		"-c profile quoted":     {[]string{"-c", `"profile" = mine`}, "profile key"},
		"-p after another flag": {[]string{"--search", "-p", "mine"}, "-p"},
	}
	for name, tc := range forms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			command := `["codex", "` + strings.Join(escape(tc.args), `", "`) + `"]`
			for where, file := range map[string]string{
				"agent.command (shared, ":              "[agent]\ncommand = " + command + "\n",
				"groups.coder.agent.command (shared, ": "[groups.coder]\nworkers = 0\nslots = 1\nagent = { command = " + command + " }\n",
			} {
				_, err := loadRouting(t, file+"[routing]\nenabled = true\n", "", nil, true)
				require.ErrorIs(t, err, settings.ErrInvalid, where)
				assert.Contains(t, err.Error(), where)
				assert.Contains(t, err.Error(), "codex's "+tc.flag+" chooses a profile")
				assert.NotContains(t, err.Error(), "mine", "the profile is never echoed")
				_, err = loadRouting(t, file+"[routing]\nenabled = false\n", "", nil, true)
				require.NoError(t, err, "%s with routing off", where)
			}
		})
	}
	_, err := loadRouting(t, "[agent]\ncommand = [\"/usr/local/bin/codex\", \"--profile=mine\"]\n[routing]\nenabled = true\n", "", nil, true)
	require.ErrorIs(t, err, settings.ErrInvalid, "codex by its path is codex")
	assert.Contains(t, err.Error(), "codex's --profile chooses a profile")
	_, err = loadRouting(t, "[agent]\ncommand = [\"claude\", \"-p\"]\n[routing]\nenabled = true\n", "", nil, true)
	require.NoError(t, err, "claude's -p is its print mode, not a profile")
	_, err = loadRouting(t, "[agent]\ncommand = [\"codex\", \"-c\", \"profiles.mine.model=x\"]\n[routing]\nenabled = true\n", "", nil, true)
	require.NoError(t, err, "a profile's own table chooses none")
}

// escape quotes a TOML basic string's body.
func escape(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, `"`, `\"`)
	}
	return out
}

// TestRoutingOffKeepsTheModelKeys: with routing off, as it is unless the
// user turns it on, every model key resolves as before.
func TestRoutingOffKeepsTheModelKeys(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ file, repoFile string }{
		"routing unset":       {file: "[agent]\nmodel = \"opus\"\n[groups.qa]\nworkers = 0\nslots = 1\nagent = { model = \"sonnet\" }\n"},
		"routing off":         {file: "[routing]\nenabled = false\n[agent]\nmodel = \"opus\"\n"},
		"a repo file's model": {file: "[routing]\nenabled = false\n", repoFile: "[agent]\nmodel = \"sonnet\"\n"},
	} {
		res, err := loadRouting(t, tc.file, tc.repoFile, nil, true)
		require.NoError(t, err, name)
		assert.False(t, res.Routing().Enabled, name)
		assert.NotEmpty(t, res.Profile.Agent.Model, name)
	}
	res, err := loadRouting(t, "[routing]\nenabled = true\n", "", nil, true)
	require.NoError(t, err)
	assert.Empty(t, res.Profile.Agent.Model, "routing on with no model key resolves")
}

// TestResolvedPolicy: the resolved [routing] makes the policy a spawn
// decides by, named by a version that follows the deployment; a deployment
// Load never checked is refused, not decided by.
func TestResolvedPolicy(t *testing.T) {
	t.Parallel()
	res, err := loadRouting(t, "[routing.bindings.claude_mid]\nmodel_id = \"claude-sonnet-5-6\"\n", "", nil, false)
	require.NoError(t, err)
	p, err := res.Policy()
	require.NoError(t, err)
	want, err := routing.New(routing.V1(), res.Routing())
	require.NoError(t, err)
	assert.Equal(t, want.Version(), p.Version())
	plain, err := routing.New(routing.V1(), routing.Deployment{})
	require.NoError(t, err)
	assert.NotEqual(t, plain.Version(), p.Version(), "the model id is in the version")

	_, err = settings.Resolved{Values: map[string]any{"routing": map[string]any{"overrides": map[string]any{"boss": "claude_premium"}}}}.Policy()
	require.ErrorIs(t, err, settings.ErrInvalid)
	require.ErrorIs(t, err, routing.ErrOverrideRefused)
	_, err = settings.Profile{Routing: routing.Deployment{Overrides: map[string]string{"boss": "claude_premium"}}}.Policy()
	require.ErrorIs(t, err, settings.ErrInvalid)
	require.ErrorIs(t, err, routing.ErrOverrideRefused)
}

// TestProfilePolicyIsTheResolvedOne: a project's spawns decide by the
// policy config resolve --project names, version and all, for a [routing]
// whose facts and overrides are in that version.
func TestProfilePolicyIsTheResolvedOne(t *testing.T) {
	t.Parallel()
	res, err := loadRouting(t, "[routing]\nenabled = true\n[routing.bindings.claude_mid]\nmodel_id = \"claude-sonnet-5-6\"\nefforts = [\"high\", \"max\"]\n[routing.overrides]\nresearcher = \"claude_small\"\n", "", nil, true)
	require.NoError(t, err)
	resolved, err := res.Policy()
	require.NoError(t, err)
	spawns, err := res.Profile.Policy()
	require.NoError(t, err)
	assert.Equal(t, resolved.Version(), spawns.Version())
	plain, err := routing.New(routing.V1(), routing.Deployment{Enabled: true})
	require.NoError(t, err)
	assert.NotEqual(t, plain.Version(), spawns.Version(), "the facts and the override are in the version")
	assert.True(t, spawns.Enabled())
}

// TestCheckRoutingProviders: config resolve --project refuses a user
// override a worker type with its role cannot run, naming the override,
// the type and both providers, whether or not routing is on; a type whose
// agent runs the binding's provider, a type with no role, one that only
// describes and a role with no override pass, and Load alone never refuses.
func TestCheckRoutingProviders(t *testing.T) {
	t.Parallel()
	qa := "[groups.qa]\nworkers = 0\nslots = 1\nagent = { command = [\"codex\"] }\n"
	precheck := "[groups.precheck]\nworkers = 0\nslots = 1\n"
	override := "[routing.overrides]\npre_check = \"claude_mid\"\n"
	for name, routingOn := range map[string]string{"routing off": "", "routing on": "[routing]\nenabled = true\n"} {
		res, err := loadRouting(t, routingOn+precheck+qa+override, "", nil, true)
		require.NoError(t, err, "%s: Load leaves the check to config resolve", name)
		err = res.Profile.CheckRoutingProviders()
		require.ErrorIs(t, err, settings.ErrInvalid, name)
		require.ErrorIs(t, err, routing.ErrProviderMismatch, name)
		assert.Equal(t, "routing.overrides.pre_check: invalid config: provider mismatch: claude_mid is a claude binding, and the worker type qa runs codex", err.Error(), name)
	}
	res, err := loadRouting(t, precheck+strings.Replace(qa, `["codex"]`, `["/usr/local/bin/codex"]`, 1)+override, "", nil, true)
	require.NoError(t, err)
	err = res.Profile.CheckRoutingProviders()
	require.ErrorIs(t, err, routing.ErrProviderMismatch, "codex by its path runs codex")
	assert.Contains(t, err.Error(), "and the worker type qa runs codex")
	res, err = loadRouting(t, "[groups.precheck]\nworkers = 0\nslots = 1\nagent = { command = [\"/usr/local/bin/claude\"] }\n"+override, "", nil, true)
	require.NoError(t, err)
	require.NoError(t, res.Profile.CheckRoutingProviders(), "claude by its path runs claude")
	for name, file := range map[string]string{
		"a type running the binding's provider": precheck + override,
		"no override":                           qa,
		"another role's override":               qa + "[routing.overrides]\nresearcher = \"claude_small\"\n",
		"a type with no role":                   "[groups.designer]\nworkers = 0\nslots = 1\nagent = { command = [\"codex\"] }\n" + override,
		"a type that only describes":            "[agent]\ncommand = [\"codex\"]\n[groups.qa]\ndescription = \"verifies a handoff\"\n[groups.coder]\nworkers = 0\nslots = 1\nagent = { command = [\"claude\"] }\n" + override,
		"the override's own provider":           qa + "[routing.overrides]\npre_check = \"codex_strong\"\n",
	} {
		res, err := loadRouting(t, file, "", nil, true)
		require.NoError(t, err, name)
		require.NoError(t, res.Profile.CheckRoutingProviders(), name)
	}
}

// TestRoutingJevResolves: [routing.jev] from the shared file and --set
// resolves into the deployment's Jev facts, each leaf with its layer; the
// env file is no routing fact, and expands ~/ with HOME. A [routing.jev]
// that sets no fact leaves the deployment's version as it was, and Jev
// enabled while routing is off asks nothing.
func TestRoutingJevResolves(t *testing.T) {
	t.Parallel()
	file := "[routing]\nenabled = true\n[routing.jev]\nenabled = true\nenv_file = \"~/.config/typesafe.env\"\nconfidence_pct = 70\n"
	res, err := loadRouting(t, file, "", []string{"routing.jev.sufficiency_pct=85"}, true)
	require.NoError(t, err)
	assert.Equal(t, &routing.JevFacts{Enabled: true, ConfidencePct: new(70), SufficiencyPct: new(85)}, res.Routing().Jev)
	for path, layer := range map[string]string{
		"routing.jev.enabled":         settings.LayerShared,
		"routing.jev.env_file":        settings.LayerShared,
		"routing.jev.confidence_pct":  settings.LayerShared,
		"routing.jev.sufficiency_pct": settings.LayerFlag,
	} {
		assert.Equal(t, layer, res.Sources[path], path)
	}
	envFile, err := res.JevEnvFile(envOf(map[string]string{"HOME": "/home/u"}))
	require.NoError(t, err)
	assert.Equal(t, "/home/u/.config/typesafe.env", envFile)
	_, err = res.JevEnvFile(envOf(nil))
	require.ErrorIs(t, err, settings.ErrInvalid, "~/ with no HOME")
	p, err := res.Policy()
	require.NoError(t, err)
	assert.True(t, p.JevEnabled())

	base, err := loadRouting(t, "[routing]\nenabled = true\n", "", nil, true)
	require.NoError(t, err)
	baseVersion := base.Routing()
	fileOnly, err := loadRouting(t, "[routing]\nenabled = true\n[routing.jev]\nenv_file = \"/run/typesafe.env\"\n", "", nil, true)
	require.NoError(t, err)
	assert.Nil(t, fileOnly.Routing().Jev, "the env file alone is no routing fact")
	assert.Equal(t, baseVersion, fileOnly.Routing())
	envFile, err = fileOnly.JevEnvFile(envOf(nil))
	require.NoError(t, err)
	assert.Equal(t, "/run/typesafe.env", envFile)
	envFile, err = base.JevEnvFile(envOf(nil))
	require.NoError(t, err)
	assert.Empty(t, envFile, "no env file")

	routingOff, err := loadRouting(t, "", "", []string{"routing.jev.enabled=true", "routing.jev.env_file=/run/typesafe.env"}, false)
	require.NoError(t, err, "Jev enabled while routing is off resolves")
	assert.Equal(t, settings.LayerFlag, routingOff.Sources["routing.jev.enabled"])
	p, err = routingOff.Policy()
	require.NoError(t, err)
	assert.False(t, p.JevEnabled(), "and asks nothing")
}

// TestRoutingJevRefuses: Jev enabled with no env file, an env file that is
// no absolute or ~/ path, a threshold outside a whole percent, and
// [routing.jev] anywhere but the shared file and --set are each refused,
// naming the key and its layer.
func TestRoutingJevRefuses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		file, repoFile string
		set            []string
		project        bool
		wantErr        error
		wantIn         []string
	}{
		"enabled with no env file":             {file: "[routing.jev]\nenabled = true\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.enabled (shared, ", "routing.jev.env_file"}},
		"enabled with an empty env file":       {file: "[routing.jev]\nenabled = true\nenv_file = \"\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.enabled (shared, "}},
		"enabled by --set with no env file":    {set: []string{"routing.jev.enabled=true"}, wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.enabled (flag, --set)"}},
		"enabled with routing on, no file":     {file: "[routing]\nenabled = true\n[routing.jev]\nenabled = true\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.env_file"}},
		"a relative env file":                  {file: "[routing.jev]\nenv_file = \"typesafe.env\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.env_file (shared, ", "not an absolute path"}},
		"a bare ~/":                            {file: "[routing.jev]\nenv_file = \"~/\"\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.env_file"}},
		"a control character in it":            {set: []string{`routing.jev.env_file="/run/a\nb.env"`}, wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.env_file (flag, --set)"}},
		"an env file that is a table":          {file: "[routing.jev]\nenv_file = { env = \"TYPESAFE_API_KEY\" }\n", wantErr: settings.ErrInvalid},
		"confidence 0":                         {file: "[routing.jev]\nconfidence_pct = 0\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.confidence_pct (shared, ", "1 to 100"}},
		"confidence 101":                       {file: "[routing.jev]\nconfidence_pct = 101\n", wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.confidence_pct"}},
		"sufficiency 150 from --set":           {set: []string{"routing.jev.sufficiency_pct=150"}, wantErr: settings.ErrInvalid, wantIn: []string{"routing.jev.sufficiency_pct (flag, --set)"}},
		"a threshold as a fraction":            {file: "[routing.jev]\nconfidence_pct = 0.75\n", wantErr: settings.ErrInvalid},
		"a [routing.jev] key it does not know": {file: "[routing.jev]\nkey = { env = \"TYPESAFE_API_KEY\" }\n", wantErr: settings.ErrUnknownKey, wantIn: []string{"routing.jev.key"}},
		"the endpoint":                         {file: "[routing.jev]\nurl = \"https://example.com\"\n", wantErr: settings.ErrUnknownKey},
		"[routing.jev] in a repo file":         {repoFile: "[routing.jev]\nenabled = true\nenv_file = \"/tmp/x.env\"\n", project: true, wantErr: settings.ErrUnknownKey},
		"an env file in a repo file":           {repoFile: "[routing.jev]\nenv_file = \"/tmp/x.env\"\n", project: true, wantErr: settings.ErrUnknownKey},
		"[routing.jev] in a project table":     {file: "[projects.other]\nrepo = \"REPO\"\n[projects.other.routing.jev]\nenabled = true\n", wantErr: settings.ErrUnknownKey},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := loadRouting(t, tc.file, tc.repoFile, tc.set, tc.project)
			require.ErrorIs(t, err, tc.wantErr)
			for _, s := range tc.wantIn {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
}

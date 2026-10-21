package settings_test

import (
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
}

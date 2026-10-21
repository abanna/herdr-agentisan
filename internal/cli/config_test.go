package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// configSecret stands in for a secret the environment or 1Password holds.
// No output may ever contain it.
const configSecret = "hunter2"

const linearRef = "op://Vault/Linear/credential"

// configCtx injects exactly env as the environment and op as the 1Password
// reader, so a test never sees the developer's real HERDR_PLUGIN_CONFIG_DIR
// or HOME and never runs the real op.
func configCtx(t *testing.T, env map[string]string, op func(context.Context, string) (string, error)) context.Context {
	t.Helper()
	ctx := cli.WithLookupEnv(t.Context(), func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	if op == nil {
		op = func(context.Context, string) (string, error) {
			t.Error("op must not be called")
			return "", errors.New("unexpected op call")
		}
	}
	return cli.WithOpRead(ctx, op)
}

func opAnswers(values map[string]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, ref string) (string, error) {
		if v, ok := values[ref]; ok {
			return v, nil
		}
		return "", errors.New("[ERROR] item not found")
	}
}

// configFixture writes a shared file with projects app and alpha, and app's
// repo with the Agentisan bundle installed. It returns the config dir and
// the repo.
func configFixture(t *testing.T) (string, string) {
	t.Helper()
	dir, repo := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".claude"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".claude", "version.txt"), []byte("1\n"), 0o600))
	shared := `schema_version = 1

[integrations.github]
token = { env = "GITHUB_TOKEN" }

[projects.app]
repo = "` + repo + `"
color = "#123456"
short = "app"

[projects.app.integrations.linear]
token = { op = "` + linearRef + `" }

[projects.alpha]
repo = "/nonexistent"
short = "al"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(shared), 0o600))
	return dir, repo
}

// defaultPolicy is the routing policy's version with no [routing] set.
func defaultPolicy(t *testing.T) string {
	t.Helper()
	p, err := routing.New(routing.V1(), routing.Deployment{})
	require.NoError(t, err)
	return p.Version()
}

func TestConfigResolveText(t *testing.T) {
	t.Parallel()
	dir, repo := configFixture(t)

	out, err := runCtx(configCtx(t, nil, nil), "config", "resolve", "--project", "app", "--config-dir", dir)
	require.NoError(t, err)
	assert.Equal(t, `agent.command = ["claude"]  # default
color = "#123456"  # project
integrations.github.token = { env = "GITHUB_TOKEN" }  # shared
integrations.linear.token = { op = "`+linearRef+`" }  # project
notices.gap = 30  # default
notices.repeat = 300  # default
repo = "`+repo+`"  # project
rotation.thresholds = [55, 65, 75]  # default
short = "app"  # project
routing policy: `+defaultPolicy(t)+`
`, out)
}

func TestConfigResolveWithoutAProjectListsThem(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)

	out, err := runCtx(configCtx(t, nil, nil), "config", "resolve", "--config-dir", dir)
	require.NoError(t, err)
	assert.Equal(t, `agent.command = ["claude"]  # default
integrations.github.token = { env = "GITHUB_TOKEN" }  # shared
notices.gap = 30  # default
notices.repeat = 300  # default
rotation.thresholds = [55, 65, 75]  # default
routing policy: `+defaultPolicy(t)+`
projects: alpha, app
`, out)

	out, err = runCtx(configCtx(t, nil, nil), "config", "resolve", "--config-dir", t.TempDir())
	require.NoError(t, err, "no shared file: the defaults alone")
	assert.Equal(t, `agent.command = ["claude"]  # default
notices.gap = 30  # default
notices.repeat = 300  # default
rotation.thresholds = [55, 65, 75]  # default
routing policy: `+defaultPolicy(t)+`
projects: (none)
`, out)
}

// TestConfigResolveShowsThePolicyVersion: the routing policy's version
// follows [routing], from whichever layer sets it (A50).
func TestConfigResolveShowsThePolicyVersion(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)
	on, err := routing.New(routing.V1(), routing.Deployment{Enabled: true})
	require.NoError(t, err)
	require.NotEqual(t, defaultPolicy(t), on.Version())

	out, err := runCtx(configCtx(t, nil, nil), "config", "resolve", "--config-dir", dir, "--set", "routing.enabled=true")
	require.NoError(t, err)
	assert.Contains(t, out, "routing.enabled = true  # flag\nrouting policy: "+on.Version()+"\n")
}

// TestConfigResolveSetKeepsCommas: --set is a string array, so a TOML list
// with commas arrives whole rather than split into three flags.
func TestConfigResolveSetKeepsCommas(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)

	out, err := runCtx(configCtx(t, nil, nil), "config", "resolve", "--config-dir", dir,
		"--project", "app", "--set", "rotation.thresholds=[50,60,70]", "--set", "agent.model=opus")
	require.NoError(t, err)
	assert.Contains(t, out, "rotation.thresholds = [50, 60, 70]  # flag\n")
	assert.Contains(t, out, "agent.model = \"opus\"  # flag\n")
}

func TestConfigResolveJSON(t *testing.T) {
	t.Parallel()
	dir, repo := configFixture(t)

	tests := map[string]struct {
		args  []string
		check func(t *testing.T, got map[string]any)
	}{
		"a project": {
			args: []string{"--project", "app"},
			check: func(t *testing.T, got map[string]any) {
				t.Helper()
				assert.Equal(t, "app", got["project"])
				values, ok := got["values"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, repo, values["repo"])
				assert.Equal(t, []any{"claude"}, values["agent"].(map[string]any)["command"])
				integrations := values["integrations"].(map[string]any)
				assert.Equal(t, map[string]any{"env": "GITHUB_TOKEN"}, integrations["github"].(map[string]any)["token"])
				assert.Equal(t, map[string]any{"op": linearRef}, integrations["linear"].(map[string]any)["token"])
				assert.Equal(t, map[string]any{
					"agent.command":             settings.LayerDefault,
					"color":                     settings.LayerProject,
					"integrations.github.token": settings.LayerShared,
					"integrations.linear.token": settings.LayerProject,
					"notices.gap":               settings.LayerDefault,
					"notices.repeat":            settings.LayerDefault,
					"repo":                      settings.LayerProject,
					"rotation.thresholds":       settings.LayerDefault,
					"short":                     settings.LayerProject,
				}, got["sources"])
				assert.Equal(t, []any{"alpha", "app"}, got["projects"])
				assert.Equal(t, defaultPolicy(t), got["routing_policy"])
				assert.NotContains(t, got, "secrets", "secrets appear only with --check-secrets")
			},
		},
		"no project": {
			check: func(t *testing.T, got map[string]any) {
				t.Helper()
				assert.Contains(t, got, "project")
				assert.Nil(t, got["project"])
				assert.Equal(t, []any{"alpha", "app"}, got["projects"])
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"config", "resolve", "--config-dir", dir, "--json"}, tc.args...)
			out, err := runCtx(configCtx(t, nil, nil), args...)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got), "output: %s", out)
			tc.check(t, got)
		})
	}
}

// TestConfigResolveConfigDir: the directory comes from --config-dir, else
// HERDR_PLUGIN_CONFIG_DIR, else the command fails naming both.
func TestConfigResolveConfigDir(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)
	empty := t.TempDir()

	tests := map[string]struct {
		env     map[string]string
		args    []string
		wantErr error
		wantIn  []string
	}{
		"from HERDR_PLUGIN_CONFIG_DIR": {
			env: map[string]string{"HERDR_PLUGIN_CONFIG_DIR": dir},
		},
		"--config-dir beats HERDR_PLUGIN_CONFIG_DIR": {
			env:  map[string]string{"HERDR_PLUGIN_CONFIG_DIR": empty},
			args: []string{"--config-dir", dir},
		},
		"neither": {
			wantErr: settings.ErrNoConfigDir,
			wantIn:  []string{"--config-dir", "HERDR_PLUGIN_CONFIG_DIR"},
		},
		"HERDR_PLUGIN_CONFIG_DIR empty": {
			env:     map[string]string{"HERDR_PLUGIN_CONFIG_DIR": ""},
			wantErr: settings.ErrNoConfigDir,
			wantIn:  []string{"--config-dir", "HERDR_PLUGIN_CONFIG_DIR"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"config", "resolve", "--project", "app"}, tc.args...)
			out, err := runCtx(configCtx(t, tc.env, nil), args...)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				for _, s := range tc.wantIn {
					assert.Contains(t, err.Error(), s)
				}
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out, "color = \"#123456\"  # project")
		})
	}
}

func TestConfigResolveCheckSecrets(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)

	tests := map[string]struct {
		env      map[string]string
		op       map[string]string
		wantErr  bool
		wantOut  []string
		jsonWant []any
	}{
		"all set": {
			env: map[string]string{"GITHUB_TOKEN": configSecret},
			op:  map[string]string{linearRef: configSecret},
			wantOut: []string{
				"integrations.github.token: set\n",
				"integrations.linear.token: set\n",
			},
		},
		"env unset": {
			op:      map[string]string{linearRef: configSecret},
			wantErr: true,
			wantOut: []string{
				"integrations.github.token: missing (",
				"integrations.linear.token: set\n",
			},
		},
		"op fails": {
			env:     map[string]string{"GITHUB_TOKEN": configSecret},
			wantErr: true,
			wantOut: []string{
				"integrations.github.token: set\n",
				"integrations.linear.token: missing (",
				"item not found",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := configCtx(t, tc.env, opAnswers(tc.op))

			out, err := runCtx(ctx, "config", "resolve", "--config-dir", dir, "--project", "app", "--check-secrets")
			if tc.wantErr {
				require.ErrorIs(t, err, settings.ErrSecretUnavailable)
				assert.NotContains(t, err.Error(), configSecret)
			} else {
				require.NoError(t, err)
			}
			for _, s := range tc.wantOut {
				assert.Contains(t, out, s)
			}
			assert.NotContains(t, out, configSecret, "a secret value must never be printed")

			out, err = runCtx(ctx, "config", "resolve", "--config-dir", dir, "--project", "app", "--check-secrets", "--json")
			assert.Equal(t, tc.wantErr, err != nil)
			assert.NotContains(t, out, configSecret)
			var got struct {
				Secrets []struct {
					Path   string `json:"path"`
					Ref    string `json:"ref"`
					Status string `json:"status"`
					Reason string `json:"reason"`
				} `json:"secrets"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got), "output: %s", out)
			require.Len(t, got.Secrets, 2)
			assert.Equal(t, "integrations.github.token", got.Secrets[0].Path)
			assert.Equal(t, "env:GITHUB_TOKEN", got.Secrets[0].Ref)
			assert.Equal(t, linearRef, got.Secrets[1].Ref)
			for _, s := range got.Secrets {
				assert.Contains(t, []string{"set", "missing"}, s.Status)
				assert.Equal(t, s.Status == "missing", s.Reason != "")
			}
		})
	}
}

// TestConfigResolveErrors: every sentinel exits non-zero with a message
// naming the key path.
func TestConfigResolveErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		shared  string
		args    []string
		wantErr error
		wantIn  string
	}{
		"unsupported schema": {
			shared: "schema_version = 7\n", wantErr: settings.ErrUnsupportedSchema, wantIn: "schema_version",
		},
		"unknown key": {
			shared: "schema_version = 1\n[agent]\nmodle = \"opus\"\n", wantErr: settings.ErrUnknownKey, wantIn: "agent.modle",
		},
		"secret value": {
			shared:  "schema_version = 1\n[integrations.github]\ntoken = \"" + configSecret + "\"\n",
			wantErr: settings.ErrSecretValue, wantIn: "integrations.github.token",
		},
		"bypasses Agentisan": {
			shared: "schema_version = 1\n[agent]\ncommand = [\"aider\"]\n", wantErr: settings.ErrBypassesAgentisan, wantIn: "agent.command",
		},
		"invalid": {
			shared: "schema_version = 1\ncolor = \"red\"\n", wantErr: settings.ErrInvalid, wantIn: "color",
		},
		"no project": {
			shared: "schema_version = 1\n", args: []string{"--project", "nope"}, wantErr: settings.ErrNoProject, wantIn: "nope",
		},
		"a project named like a flag": {
			shared: "schema_version = 1\n", args: []string{"--project", "--help"}, wantErr: settings.ErrNoProject, wantIn: "--help",
		},
		"bad --set": {
			shared: "schema_version = 1\n", args: []string{"--set", "color"}, wantErr: settings.ErrInvalid, wantIn: "--set",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(tc.shared), 0o600))

			args := append([]string{"config", "resolve", "--config-dir", dir}, tc.args...)
			_, err := runCtx(configCtx(t, nil, nil), args...)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.NotContains(t, err.Error(), configSecret)
		})
	}
}

func TestConfigRejectsArguments(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)

	for name, args := range map[string][]string{
		"bare group":   {"config"},
		"unknown verb": {"config", "bogus"},
		"positional":   {"config", "resolve", "--config-dir", dir, "extra"},
		"unknown flag": {"config", "resolve", "--config-dir", dir, "--bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := runCtx(configCtx(t, nil, nil), args...)
			require.Error(t, err)
		})
	}
}

// TestConfigResolveRefusesWhatARepoFileCannotSet: config resolve --project
// refuses a repo file that sets a key outside the ones it may set, or a model
// that is not safe to pass, from any layer (ADR-001 A46). The error names the
// key and its layer, and neither it nor the output holds the value.
func TestConfigResolveRefusesWhatARepoFileCannotSet(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		repoFile string
		set      []string
		wantIn   string
	}{
		"keys to type":           {repoFile: "[spawn]\nshell_ready_keys = [\"from-the-repo\", \"enter\"]\n", wantIn: "spawn.shell_ready_keys (repo, "},
		"a group command":        {repoFile: "[groups.qa]\nworkers = 1\nslots = 1\nagent = { command = [\"claude\", \"--from-the-repo\"] }\n", wantIn: "groups.qa.agent.command (repo, "},
		"a model a shell breaks": {repoFile: "[agent]\nmodel = \"\\\\';echo from-the-repo;#\"\n", wantIn: "agent.model (repo, "},
		"a model like a flag":    {set: []string{"agent.model=--from-the-repo"}, wantIn: "agent.model (flag, "},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, repo := configFixture(t)
			if tc.repoFile != "" {
				require.NoError(t, os.WriteFile(filepath.Join(repo, settings.RepoFileName), []byte("schema_version = 1\n"+tc.repoFile), 0o600))
			}
			args := []string{"config", "resolve", "--project", "app", "--config-dir", dir}
			for _, s := range tc.set {
				args = append(args, "--set", s)
			}

			out, err := runCtx(configCtx(t, nil, nil), args...)
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.NotContains(t, err.Error()+out, "from-the-repo", "never the value")
		})
	}
}

// TestConfigResolveRendersAnyValue: whatever a value holds, text output stays
// one line per leaf with the value as a TOML string, and --json stays valid
// JSON carrying the same string.
func TestConfigResolveRendersAnyValue(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)

	tests := map[string]struct {
		set      string
		wantText string
		want     string
	}{
		"non-ASCII":         {set: "integrations.wiki.url=café", wantText: `integrations.wiki.url = "café"  # flag`, want: "café"},
		"a NUL":             {set: `integrations.wiki.url="a\u0000b"`, wantText: `integrations.wiki.url = "a\u0000b"  # flag`, want: "a\x00b"},
		"a newline and CR":  {set: `integrations.wiki.url="a\r\nb"`, wantText: `integrations.wiki.url = "a\r\nb"  # flag`, want: "a\r\nb"},
		"quotes and spaces": {set: `integrations.wiki.url="say \"hi\" ; ls"`, wantText: `integrations.wiki.url = "say \"hi\" ; ls"  # flag`, want: `say "hi" ; ls`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args := []string{"config", "resolve", "--config-dir", dir, "--project", "app", "--set", tc.set}

			out, err := runCtx(configCtx(t, nil, nil), args...)
			require.NoError(t, err)
			assert.Contains(t, out, tc.wantText+"\n")
			assert.Len(t, strings.Split(strings.TrimSuffix(out, "\n"), "\n"), 11, "one line per leaf, then the routing policy: %q", out)

			out, err = runCtx(configCtx(t, nil, nil), append(args, "--json")...)
			require.NoError(t, err)
			var got struct {
				Values struct {
					Integrations struct {
						Wiki struct {
							URL string `json:"url"`
						} `json:"wiki"`
					} `json:"integrations"`
				} `json:"values"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got), "output: %s", out)
			assert.Equal(t, tc.want, got.Values.Integrations.Wiki.URL)
		})
	}
}

// TestConfigResolveShowsTheSpacesProject: config resolve reports
// spaces.project like any leaf, with the layer it came from, and refuses one
// that names no configured project.
func TestConfigResolveShowsTheSpacesProject(t *testing.T) {
	t.Parallel()
	dir, _ := configFixture(t)

	out, err := runCtx(configCtx(t, nil, nil), "config", "resolve", "--config-dir", dir, "--set", "spaces.project=alpha")
	require.NoError(t, err)
	assert.Contains(t, out, "spaces.project = \"alpha\"  # flag\n")

	out, err = runCtx(configCtx(t, nil, nil), "config", "resolve", "--config-dir", dir, "--project", "app", "--json", "--set", "spaces.project=app")
	require.NoError(t, err)
	var got struct {
		Values  map[string]any    `json:"values"`
		Sources map[string]string `json:"sources"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, map[string]any{"project": "app"}, got.Values["spaces"])
	assert.Equal(t, settings.LayerFlag, got.Sources["spaces.project"])

	_, err = runCtx(configCtx(t, nil, nil), "config", "resolve", "--config-dir", dir, "--set", "spaces.project=beta")
	require.ErrorIs(t, err, settings.ErrNoProject)
	assert.Contains(t, err.Error(), "known projects: alpha, app")
}

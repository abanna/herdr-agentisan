package plugin_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// lookupFrom builds an environment lookup from a map, so no test reads the
// process environment — a shell inside herdr has the real variables set.
func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestEnvFromAnActionInvocation(t *testing.T) {
	t.Parallel()

	env, err := plugin.EnvFrom(lookupFrom(map[string]string{
		"HERDR_ENV":                 "1",
		"HERDR_SOCKET_PATH":         "/run/herdr.sock",
		"HERDR_BIN_PATH":            "/usr/bin/herdr",
		"HERDR_PLUGIN_ID":           "nerdsrun.agentisan",
		"HERDR_PLUGIN_ROOT":         "/plugins/agentisan",
		"HERDR_PLUGIN_CONFIG_DIR":   "/cfg",
		"HERDR_PLUGIN_STATE_DIR":    "/state",
		"HERDR_PLUGIN_ACTION_ID":    "ping",
		"HERDR_PLUGIN_CONTEXT_JSON": `{"invocation_source":"keybinding","workspace_id":"w1","focused_pane_status":"working","worktree":null,"added_by_a_newer_herdr":true}`,
	}))
	require.NoError(t, err)
	assert.True(t, env.InHerdr)
	assert.Equal(t, "/run/herdr.sock", env.SocketPath)
	assert.Equal(t, "/usr/bin/herdr", env.BinPath)
	assert.Equal(t, "nerdsrun.agentisan", env.PluginID)
	assert.Equal(t, "/plugins/agentisan", env.Root)
	assert.Equal(t, "/cfg", env.ConfigDir)
	assert.Equal(t, "/state", env.StateDir)
	assert.Equal(t, "ping", env.ActionID)
	require.NotNil(t, env.Context, "unknown context fields must be tolerated, not rejected")
	assert.Equal(t, "keybinding", env.Context.InvocationSource)
	assert.Equal(t, "w1", env.Context.WorkspaceID)
	assert.Equal(t, "working", env.Context.FocusedPaneStatus)
	assert.Empty(t, env.Context.Worktree, "null decodes to empty")
	assert.Nil(t, env.Event)
}

func TestEnvFromAnEventHook(t *testing.T) {
	t.Parallel()

	env, err := plugin.EnvFrom(lookupFrom(map[string]string{
		"HERDR_PLUGIN_EVENT":      "pane.agent_status_changed",
		"HERDR_PLUGIN_EVENT_JSON": `{"event":"pane_agent_status_changed","data":{"pane_id":"w1:p1","agent_status":"blocked"}}`,
	}))
	require.NoError(t, err)
	assert.Equal(t, "pane.agent_status_changed", env.EventName)
	require.NotNil(t, env.Event)
	assert.Equal(t, "pane_agent_status_changed", env.Event.Kind)
	assert.JSONEq(t, `{"pane_id":"w1:p1","agent_status":"blocked"}`, string(env.Event.Data))
}

func TestEnvClasses(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		vars    map[string]string
		wantErr bool
		check   func(t *testing.T, env plugin.Env)
	}{
		"outside herdr everything is empty": {
			vars: map[string]string{},
			check: func(t *testing.T, env plugin.Env) {
				t.Helper()
				assert.False(t, env.InHerdr)
				assert.Empty(t, env.SocketPath)
				assert.Nil(t, env.Context)
			},
		},
		"HERDR_ENV set to something other than 1": {
			vars:  map[string]string{"HERDR_ENV": "true"},
			check: func(t *testing.T, env plugin.Env) { t.Helper(); assert.False(t, env.InHerdr) },
		},
		"empty JSON variables are treated as absent": {
			vars: map[string]string{"HERDR_PLUGIN_CONTEXT_JSON": "", "HERDR_PLUGIN_EVENT_JSON": ""},
			check: func(t *testing.T, env plugin.Env) {
				t.Helper()
				assert.Nil(t, env.Context)
				assert.Nil(t, env.Event)
			},
		},
		"malformed context JSON":  {vars: map[string]string{"HERDR_PLUGIN_CONTEXT_JSON": "{"}, wantErr: true},
		"context JSON not object": {vars: map[string]string{"HERDR_PLUGIN_CONTEXT_JSON": "[1]"}, wantErr: true},
		"malformed event JSON":    {vars: map[string]string{"HERDR_PLUGIN_EVENT_JSON": "nope"}, wantErr: true},
		"context JSON invalid UTF8": {vars: map[string]string{"HERDR_PLUGIN_CONTEXT_JSON": "{\"workspace_label\":\"a\xffb\"}"}, check: func(t *testing.T, env plugin.Env) {
			t.Helper()
			require.NotNil(t, env.Context)
			assert.NotEmpty(t, env.Context.WorkspaceLabel, "invalid bytes are replaced, not fatal")
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, err := plugin.EnvFrom(lookupFrom(tc.vars))
			if tc.wantErr {
				require.ErrorIs(t, err, plugin.ErrInvalidEnv)
				return
			}
			require.NoError(t, err)
			tc.check(t, env)
		})
	}
}

// fakeNotifier records what Ping asked herdr to show.
type fakeNotifier struct {
	got    herdr.Notification
	result herdr.NotificationResult
	err    error
}

func (f *fakeNotifier) ShowNotification(_ context.Context, n herdr.Notification) (herdr.NotificationResult, error) {
	f.got = n
	return f.result, f.err
}

// TestPingOutcomes: shown=false is herdr declining politely, not a failure —
// every reason herdr can give maps to a successful ping that reports it.
func TestPingOutcomes(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"shown", "disabled", "rate_limited", "no_foreground_client", "busy"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			n := &fakeNotifier{result: herdr.NotificationResult{Shown: reason == "shown", Reason: reason}}

			got, err := plugin.Ping(t.Context(), n, "v1.2.3")
			require.NoError(t, err)
			assert.Equal(t, reason == "shown", got.Shown)
			assert.Equal(t, reason, got.Reason)
			assert.Equal(t, plugin.PingTitle, n.got.Title)
			assert.Contains(t, n.got.Body, "v1.2.3")
		})
	}
}

func TestPingPropagatesTransportErrors(t *testing.T) {
	t.Parallel()

	n := &fakeNotifier{err: herdr.ErrUnavailable}
	_, err := plugin.Ping(t.Context(), n, "dev")
	require.ErrorIs(t, err, herdr.ErrUnavailable)
}

const validManifest = `
id = "nerdsrun.agentisan"
name = "Agentisan"
version = "0.1.0"
min_herdr_version = "0.9.3"
description = "d"
platforms = ["linux", "macos"]

[[build]]
command = ["go", "build", "-o", "bin/herdr-agentisan", "./cmd/herdr-agentisan"]

[[actions]]
id = "ping"
title = "Ping"
contexts = ["global"]
command = ["bin/herdr-agentisan", "action", "ping"]
`

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "herdr-plugin.toml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadManifest(t *testing.T) {
	t.Parallel()

	m, err := plugin.LoadManifest(writeManifest(t, validManifest))
	require.NoError(t, err)
	assert.Equal(t, "nerdsrun.agentisan", m.ID)
	assert.Equal(t, "bin/herdr-agentisan", m.BuildOutput())
	require.Len(t, m.Actions, 1)
	assert.Equal(t, "nerdsrun.agentisan.ping", m.QualifiedActionID(m.Actions[0]))
}

// TestManifestRejections: herdr only warns about many of these at link time,
// so the gate has to refuse them before a broken manifest ships.
func TestManifestRejections(t *testing.T) {
	t.Parallel()

	replace := func(old, repl string) string {
		require.Contains(t, validManifest, old, "fixture lacks %q", old)
		return strings.Replace(validManifest, old, repl, 1)
	}
	tests := map[string]string{
		"unknown top-level key (typo)": replace(`min_herdr_version`, `min_herd_version`),
		"invalid plugin id":            replace(`id = "nerdsrun.agentisan"`, `id = "nerds run"`),
		"missing name":                 replace(`name = "Agentisan"`, ``),
		"unknown platform":             replace(`["linux", "macos"]`, `["linux", "plan9"]`),
		"dotted action id":             replace(`id = "ping"`, `id = "p.ing"`),
		"action without command":       replace(`command = ["bin/herdr-agentisan", "action", "ping"]`, `command = []`),
		"unknown action context":       replace(`contexts = ["global"]`, `contexts = ["galaxy"]`),
		"duplicate action id":          validManifest + "\n[[actions]]\nid = \"ping\"\ntitle = \"again\"\ncommand = [\"x\"]\n",
		"build without -o target":      replace(`"-o", "bin/herdr-agentisan", `, ``),
		"not TOML":                     "id = [",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := plugin.LoadManifest(writeManifest(t, body))
			require.ErrorIs(t, err, plugin.ErrInvalidManifest)
		})
	}
}

// TestLoadManifestReadFailures: an unreadable path is an I/O failure, not an
// invalid manifest, so callers can tell "fix the TOML" from "wrong path".
func TestLoadManifestReadFailures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, path := range map[string]string{
		"missing file": filepath.Join(dir, "absent.toml"),
		"a directory":  dir,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := plugin.LoadManifest(path)
			require.Error(t, err)
			assert.False(t, errors.Is(err, plugin.ErrInvalidManifest), "an I/O failure is not an invalid manifest")
		})
	}
}

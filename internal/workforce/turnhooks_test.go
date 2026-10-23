package workforce_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// withHooks is a Claude worker's launch: args, then the turn hooks (A42).
func withHooks(args ...string) []string { return append(args, "--settings", workforce.TurnHooks) }

// withHooksAny is withHooks as agent.start's params decode.
func withHooksAny(args ...any) []any { return append(args, "--settings", workforce.TurnHooks) }

// TestTurnHooksAreTheSamples: the --settings a Claude worker starts with is
// the sample's two hooks, value for value, so Claude Code runs a handler the
// user installed from the sample only once (A42).
func TestTurnHooksAreTheSamples(t *testing.T) {
	t.Parallel()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "claude-hooks.example.json"))
	require.NoError(t, err)
	var want, got any
	require.NoError(t, json.Unmarshal(sample, &want))
	require.NoError(t, json.Unmarshal([]byte(workforce.TurnHooks), &got))
	assert.Equal(t, want, got)
}

// TestTurnHooksAreShellSafe: herdr types the --settings value into the
// pane's shell inside single quotes (A46), where a quote ends them and fish
// reads a backslash as an escape; anything unprintable is never typed.
func TestTurnHooksAreShellSafe(t *testing.T) {
	t.Parallel()
	for i := range len(workforce.TurnHooks) {
		c := workforce.TurnHooks[i]
		assert.True(t, c >= 0x20 && c < 0x7f, "byte %d is printable ASCII", i)
	}
	assert.NotContains(t, workforce.TurnHooks, "'")
	assert.NotContains(t, workforce.TurnHooks, `\`)
}

// TestPlanPassesTheTurnHooks: a Claude launch ends with the turn hooks, and
// one whose command passes a --settings of its own, in either form, goes
// without them, since Claude Code applies only one --settings; a Codex
// launch never gets them.
func TestPlanPassesTheTurnHooks(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		command []string
		want    []string
	}{
		"claude":                    {command: []string{"claude"}, want: withHooks("--name", "ha-coder-1")},
		"claude with its own flags": {command: []string{"claude", "--permission-mode", "auto"}, want: withHooks("--permission-mode", "auto", "--name", "ha-coder-1")},
		"its own --settings":        {command: []string{"claude", "--settings", "/etc/mine.json"}, want: []string{"--settings", "/etc/mine.json", "--name", "ha-coder-1"}},
		"its own --settings=":       {command: []string{"claude", `--settings={"model":"opus"}`}, want: []string{`--settings={"model":"opus"}`, "--name", "ha-coder-1"}},
		// Matched whole, as Claude matches a long flag (A53).
		"a longer flag is not it": {command: []string{"claude", "--settings-file", "x"}, want: withHooks("--settings-file", "x", "--name", "ha-coder-1")},
		"nor one in another case": {command: []string{"claude", "--SETTINGS", "x"}, want: withHooks("--SETTINGS", "x", "--name", "ha-coder-1")},
		"codex":                   {command: []string{"codex"}, want: []string{"--no-daemon"}},
		"codex passing --settings": {
			command: []string{"codex", "--settings", "x"}, want: []string{"--settings", "x", "--no-daemon"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &settings.Profile{Agent: settings.Agent{Command: tc.command}}
			_, args, err := workforce.Plan(p, "coder", "", "")
			require.NoError(t, err)
			assert.Equal(t, tc.want, args)
			n := 0
			for _, a := range args {
				if a == "--settings" || strings.HasPrefix(a, "--settings=") {
					n++
				}
			}
			assert.LessOrEqual(t, n, 1, "never two --settings")
		})
	}
}

// ownSettings is teamConfig with the coder group's agent passing its own
// --settings file, which A53 lets a command do.
var ownSettings = strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.coder]\n", "[projects.herdr-agentisan.groups.coder]\nagent = { command = [\"claude\", \"--settings\", \"/etc/mine.json\"] }\n", 1)

// TestSpawnWarnsOfItsOwnSettings: a spawn whose Claude command passes its
// own --settings starts its workers without the turn hooks, with one
// warning for the group, and never refuses; another group is not warned of.
func TestSpawnWarnsOfItsOwnSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, ownSettings)

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "coder", Count: 2})
	require.NoError(t, err)
	require.Len(t, res.Workers, 2)
	require.Len(t, res.Warnings, 1, "one warning for the group, not one per worker")
	assert.Contains(t, res.Warnings[0], "coder's agent command passes a --settings of its own, and Claude Code applies only one, so its workers start without the turn hooks")
	assert.NotContains(t, res.Warnings[0], "/etc/mine.json", "the value, which may hold anything, is never echoed")
	for _, id := range []string{"ha-coder-1", "ha-coder-2"} {
		assert.Equal(t, []any{"--settings", "/etc/mine.json", "--name", id}, startArgs(f)[id])
	}

	res, err = f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "qa", Count: 1})
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
	assert.Equal(t, withHooksAny("--name", "ha-qa-1"), startArgs(f)["ha-qa-1"])
}

// startArgs is each agent.start's arguments by the agent's name: workers
// come up side by side, in no set order.
func startArgs(f *fixture) map[string]any {
	out := map[string]any{}
	for _, s := range f.h.params("agent.start") {
		name, _ := s["name"].(string)
		out[name] = s["args"]
	}
	return out
}

// TestUpWarnsOfItsOwnSettings: team up gives the same warning for each
// group it fills whose command passes its own --settings.
func TestUpWarnsOfItsOwnSettings(t *testing.T) {
	t.Parallel()
	f := upFixture(t, ownSettings)

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	require.Len(t, res.Workers, 3)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "coder's agent command passes a --settings of its own")
	assert.Equal(t, withHooksAny("--name", "ha-qa-1"), startArgs(f)["ha-qa-1"], "qa's own launch has them")
}

// TestSpawnWarnsOfCodexTurnHooks: a spawn with a Codex worker warns, and
// never refuses, when Codex's turn hooks are not installed or cannot be
// checked, after the ctx hooks' warning; a spawn with none asks nothing.
func TestSpawnWarnsOfCodexTurnHooks(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		group           string
		ctx, turned     bool
		turnErr         error
		want            []string
		asked           bool
		withoutCtxCheck bool
	}{
		"not installed": {group: "review", ctx: true, want: []string{"Codex's turn hooks are not installed, so a Codex worker reports no item or stage: run `herdr-agentisan codex hooks --turn`"}, asked: true},
		"installed":     {group: "review", ctx: true, turned: true, asked: true},
		"not checkable": {group: "review", ctx: true, turnErr: errors.New("hooks.json is not JSON"), want: []string{"could not check Codex's turn hooks: hooks.json is not JSON"}, asked: true},
		"neither installed": {
			group: "review", want: []string{"Codex's ctx hooks are not installed", "Codex's turn hooks are not installed"}, asked: true,
		},
		"with no ctx check": {group: "review", withoutCtxCheck: true, want: []string{"Codex's turn hooks are not installed"}, asked: true},
		"no Codex worker":   {group: "coder"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			asked := false
			if !tc.withoutCtxCheck {
				f.svc.CodexHooks = func() (bool, error) { return tc.ctx, nil }
			}
			f.svc.CodexTurnHooks = func() (bool, error) { asked = true; return tc.turned, tc.turnErr }

			res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: tc.group, Count: 1})
			require.NoError(t, err)
			require.Len(t, res.Workers, 1)
			require.Len(t, res.Warnings, len(tc.want))
			for i, w := range tc.want {
				assert.Contains(t, res.Warnings[i], w)
			}
			assert.Equal(t, tc.asked, asked)
		})
	}
}

// TestSpawnWarnsOnlyOfClaudesSettings: the --settings warning is Claude's,
// whose launch drops a second one; a Codex command is never warned of it.
func TestSpawnWarnsOnlyOfClaudesSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, strings.Replace(teamConfig, "[projects.herdr-agentisan.groups.review]\n", "[projects.herdr-agentisan.groups.review]\nagent = { command = [\"codex\", \"--settings\", \"x\"] }\n", 1))

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: "review", Count: 1})
	require.NoError(t, err)
	require.Len(t, res.Workers, 1)
	assert.Empty(t, res.Warnings)
}

// TestUpWarnsOfCodexHooks: team up fills a Codex group too, and warns, never
// refusing, of its ctx hooks and its turn hooks when either is missing,
// as a spawn does (A42, A43). A group's own warning comes first, then the
// machine's: a Claude group's own --settings before Codex's hooks.
func TestUpWarnsOfCodexHooks(t *testing.T) {
	t.Parallel()
	config := strings.Replace(teamConfig, "groups.review]\nworkers = 0", "groups.review]\nworkers = 1", 1)
	ownSettings := strings.Replace(config, "[projects.herdr-agentisan.groups.coder]\n", "[projects.herdr-agentisan.groups.coder]\nagent = { command = [\"claude\", \"--settings\", \"x\"] }\n", 1)
	for name, tc := range map[string]struct {
		config      string
		ctx, turned bool
		want        []string
	}{
		"neither installed":  {config: config, want: []string{"Codex's ctx hooks are not installed", "Codex's turn hooks are not installed"}},
		"turn hooks missing": {config: config, ctx: true, want: []string{"Codex's turn hooks are not installed"}},
		"both installed":     {config: config, ctx: true, turned: true},
		"and the coders' own --settings": {config: ownSettings, want: []string{
			"coder's agent command passes a --settings of its own", "Codex's ctx hooks are not installed", "Codex's turn hooks are not installed",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upFixture(t, tc.config)
			f.svc.CodexHooks = func() (bool, error) { return tc.ctx, nil }
			f.svc.CodexTurnHooks = func() (bool, error) { return tc.turned, nil }

			res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
			require.NoError(t, err)
			require.Len(t, res.Workers, 4, "two coders, a QA and the Codex reviewer come up")
			require.Len(t, res.Warnings, len(tc.want))
			for i, w := range tc.want {
				assert.Contains(t, res.Warnings[i], w)
			}
		})
	}
}

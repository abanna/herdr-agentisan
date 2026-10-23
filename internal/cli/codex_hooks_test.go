package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/codexhooks"
)

// codexEnv injects an environment holding only env, so the command never
// sees the developer's real CODEX_HOME or HOME.
func codexEnv(t *testing.T, env map[string]string) context.Context {
	t.Helper()
	return cli.WithLookupEnv(t.Context(), func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
}

func TestCodexHooksInstalls(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ctx := codexEnv(t, map[string]string{"CODEX_HOME": home, "HOME": "/nonexistent"})
	path := filepath.Join(home, codexhooks.FileName)

	out, err := runCtx(ctx, "codex", "hooks")
	require.NoError(t, err)
	assert.Contains(t, out, path+": written")
	for _, line := range []string{"PostToolUse  added", "Stop         added", "PostCompact  added"} {
		assert.Contains(t, out, line)
	}
	assert.Contains(t, out, "Trust all and continue", "the one step left is the user's")
	assert.Contains(t, out, "--no-daemon")
	assert.Contains(t, out, "trust each repo")
	assert.NotContains(t, out, "features.hooks")
	_, err = os.Stat(path)
	require.NoError(t, err)

	out, err = runCtx(ctx, "codex", "hooks")
	require.NoError(t, err)
	assert.Contains(t, out, path+": unchanged")
	assert.Contains(t, out, "Stop         unchanged")

	out, err = runCtx(ctx, "codex", "hooks", "--check")
	require.NoError(t, err)
	assert.Contains(t, out, "Stop         present  "+path)
	assert.NotContains(t, out, "Trust all", "a check only reports")
}

func TestCodexHooksJSON(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ctx := codexEnv(t, map[string]string{"CODEX_HOME": home})

	out, err := runCtx(ctx, "codex", "hooks", "--json")
	require.NoError(t, err)
	var res codexhooks.Result
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	assert.True(t, res.Changed)
	assert.Equal(t, filepath.Join(home, codexhooks.FileName), res.Path)
	assert.True(t, res.Installed())

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &raw))
	assert.ElementsMatch(t, []string{"path", "changed", "hooks", "hooks_off"}, keys(raw))
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCodexHooksCheckFailsUntilInstalled(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ctx := codexEnv(t, map[string]string{"CODEX_HOME": home})

	out, err := runCtx(ctx, "codex", "hooks", "--check")
	require.ErrorIs(t, err, codexhooks.ErrNotInstalled)
	assert.Contains(t, out, "PostToolUse  missing")
	assert.NoFileExists(t, filepath.Join(home, codexhooks.FileName), "a check writes nothing")

	out, err = runCtx(ctx, "codex", "hooks", "--check", "--json")
	require.ErrorIs(t, err, codexhooks.ErrNotInstalled)
	var res codexhooks.Result
	require.NoError(t, json.Unmarshal([]byte(out), &res), "the JSON is printed before the exit")
	assert.False(t, res.Installed())
}

func TestCodexHooksDefaultsToHomesCodex(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ctx := codexEnv(t, map[string]string{"HOME": home})

	_, err := runCtx(ctx, "codex", "hooks")
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(home, ".codex", codexhooks.FileName))
}

func TestCodexHooksWarnsWhenCodexRunsNoHooks(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[features]\nhooks = false\n"), 0o600))
	ctx := codexEnv(t, map[string]string{"CODEX_HOME": home})

	for _, args := range [][]string{{"codex", "hooks"}, {"codex", "hooks", "--check"}} {
		out, err := runCtx(ctx, args...)
		require.NoError(t, err, args)
		assert.Contains(t, out, "features.hooks = false", args)
	}
}

func TestCodexHooksErrors(t *testing.T) {
	t.Parallel()
	bad := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bad, codexhooks.FileName), []byte("{"), 0o600))
	tests := map[string]struct {
		env  map[string]string
		args []string
		want error
	}{
		"no home":             {env: map[string]string{}, args: []string{"codex", "hooks"}, want: codexhooks.ErrNoHome},
		"a CODEX_HOME absent": {env: map[string]string{"CODEX_HOME": filepath.Join(bad, "gone")}, args: []string{"codex", "hooks", "--check"}, want: codexhooks.ErrNoHome},
		"an invalid file":     {env: map[string]string{"CODEX_HOME": bad}, args: []string{"codex", "hooks"}, want: codexhooks.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := runCtx(codexEnv(t, tc.env), tc.args...)
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, out, "nothing but the error")
		})
	}
	for _, args := range [][]string{{"codex"}, {"codex", "nope"}, {"codex", "hooks", "extra"}} {
		_, err := runCtx(codexEnv(t, map[string]string{"CODEX_HOME": bad}), args...)
		require.Error(t, err, args)
	}
}

// TestCodexHooksTurn: --turn installs the turn hooks with the ctx hooks, and
// names each line's report, since Stop runs both; --check --turn fails until
// all five are in, and names --turn in its hint.
func TestCodexHooksTurn(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	ctx := codexEnv(t, map[string]string{"CODEX_HOME": home})
	path := filepath.Join(home, codexhooks.FileName)

	_, err := runCtx(ctx, "codex", "hooks")
	require.NoError(t, err)
	_, err = runCtx(ctx, "codex", "hooks", "--check", "--turn")
	require.ErrorIs(t, err, codexhooks.ErrNotInstalled, "the ctx hooks alone are not all five")
	assert.Contains(t, err.Error(), "run herdr-agentisan codex hooks --turn")

	out, err := runCtx(ctx, "codex", "hooks", "--turn")
	require.NoError(t, err)
	assert.Contains(t, out, path+": written")
	for _, line := range []string{
		"  PostToolUse      codex unchanged\n", "  Stop             codex unchanged\n", "  PostCompact      codex unchanged\n",
		"  Stop             turn  added\n", "  UserPromptSubmit turn  added\n",
	} {
		assert.Contains(t, out, line)
	}
	assert.Contains(t, out, "Trust all and continue")

	out, err = runCtx(ctx, "codex", "hooks", "--check", "--turn")
	require.NoError(t, err)
	assert.Contains(t, out, "  Stop             turn  present  "+path+"\n")

	out, err = runCtx(ctx, "codex", "hooks", "--check")
	require.NoError(t, err)
	assert.Contains(t, out, "  Stop         present  "+path+"\n", "a plain check reads as before")
	assert.NotContains(t, out, "UserPromptSubmit")

	out, err = runCtx(ctx, "codex", "hooks", "--check", "--turn", "--json")
	require.NoError(t, err)
	var res codexhooks.Result
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	assert.True(t, res.Installed())
	assert.True(t, res.TurnInstalled())
	var raw struct {
		Hooks []map[string]any `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &raw))
	require.Len(t, raw.Hooks, 5)
	assert.ElementsMatch(t, []string{"event", "report", "state", "source"}, keys(raw.Hooks[3]))
	assert.Equal(t, "turn", raw.Hooks[3]["report"])
}

// TestCodexHooksTurnRefusesASecondCopy: --turn writes nothing while
// config.toml already runs report turn, as the pasted sample does.
func TestCodexHooksTurnRefusesASecondCopy(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "codex-turn-hooks.example.toml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), sample, 0o600))
	ctx := codexEnv(t, map[string]string{"CODEX_HOME": home})

	_, err = runCtx(ctx, "codex", "hooks", "--turn")
	require.ErrorIs(t, err, codexhooks.ErrInConfigTOML)
	assert.NoFileExists(t, filepath.Join(home, codexhooks.FileName))
}

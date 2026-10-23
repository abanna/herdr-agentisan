package codexhooks_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/codexhooks"
)

// turnCmd is what each turn hook runs (A42).
const turnCmd = "herdr-agentisan report turn"

var (
	ctxStop  = ours("Stop", cmd, "")
	turnStop = ours("Stop", turnCmd, "")
	turnUPS  = ours("UserPromptSubmit", turnCmd, "")
	// allFive is a whole hooks.json with every hook of ours: Stop holds
	// the ctx group, then the turn group.
	allFive = `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ctxStop + `,` + turnStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[` + turnUPS + `]}}`
	// fiveInOrder is every hook of ours as a result lists them.
	fiveInOrder = []string{"codex PostToolUse", "codex Stop", "codex PostCompact", "turn Stop", "turn UserPromptSubmit"}
)

// byReport is each hook's state by its report and event: Stop has two.
func byReport(r codexhooks.Result) map[string]codexhooks.State {
	out := map[string]codexhooks.State{}
	for _, h := range r.Hooks {
		out[h.Report+" "+h.Event] = h.State
	}
	return out
}

func reports(r codexhooks.Result) []string {
	var out []string
	for _, h := range r.Hooks {
		out = append(out, h.Report+" "+h.Event)
	}
	return out
}

// five is every hook of ours in states ctx and turn.
func five(ctx, turn codexhooks.State) map[string]codexhooks.State {
	return map[string]codexhooks.State{
		"codex PostToolUse": ctx, "codex Stop": ctx, "codex PostCompact": ctx,
		"turn Stop": turn, "turn UserPromptSubmit": turn,
	}
}

// TestTurnHooksMatchTheSample pins the turn hooks InstallWithTurn writes to
// the sample a user may paste instead: an edited hook loses Codex's trust.
func TestTurnHooksMatchTheSample(t *testing.T) {
	t.Parallel()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "codex-turn-hooks.example.toml"))
	require.NoError(t, err)
	var cfg struct {
		Hooks map[string][]codexhooks.Group `toml:"hooks"`
	}
	dec := toml.NewDecoder(bytes.NewReader(sample))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&cfg))

	want := map[string][]codexhooks.Group{}
	for event, g := range codexhooks.TurnHooks() {
		want[event] = []codexhooks.Group{g}
	}
	assert.Equal(t, want, cfg.Hooks)
	assert.Len(t, cfg.Hooks, 2, "Stop and UserPromptSubmit")
}

// TestInstallWithTurn: the turn hooks go in beside the ctx hooks, each in a
// group of its own and after every group already in its event, so no hook
// already there moves and Codex keeps its trust in them (A43).
func TestInstallWithTurn(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		start   *string // nil: no hooks.json
		want    string
		states  map[string]codexhooks.State
		changed bool
	}{
		"no file": {want: allFive, states: five(codexhooks.Added, codexhooks.Added), changed: true},
		"the ctx hooks already in": {
			start: new(installed), want: allFive, states: five(codexhooks.Unchanged, codexhooks.Added), changed: true,
		},
		"after the user's own Stop hook": {
			start:   new(`{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ctxStop + `,` + userB + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`),
			want:    `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ctxStop + `,` + userB + `,` + turnStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[` + turnUPS + `]}}`,
			states:  five(codexhooks.Unchanged, codexhooks.Added),
			changed: true,
		},
		"a turn hook already first in Stop stays first": {
			start: new(`{"hooks":{"Stop":[` + turnStop + `]}}`),
			want:  `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + turnStop + `,` + ctxStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[` + turnUPS + `]}}`,
			states: map[string]codexhooks.State{
				"codex PostToolUse": codexhooks.Added, "codex Stop": codexhooks.Added, "codex PostCompact": codexhooks.Added,
				"turn Stop": codexhooks.Unchanged, "turn UserPromptSubmit": codexhooks.Added,
			},
			changed: true,
		},
		"all five": {start: new(allFive), want: allFive, states: five(codexhooks.Unchanged, codexhooks.Unchanged)},
		// The last hook installed is not the one that decides the write: both
		// turn hooks are in, the ctx hooks are not, and the file still changes.
		"both turn hooks in, the ctx hooks not": {
			start:   new(`{"hooks":{"Stop":[` + turnStop + `],"UserPromptSubmit":[` + turnUPS + `]}}`),
			want:    `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + turnStop + `,` + ctxStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[` + turnUPS + `]}}`,
			states:  five(codexhooks.Added, codexhooks.Unchanged),
			changed: true,
		},
		// Only `herdr-agentisan report turn`, by any path and with
		// anything after it, is ours: an empty command, one short of the
		// binary, another verb and one that only starts with turn are the
		// user's.
		"commands that are not ours, and one by its path with a redirect": {
			start: new(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":""}]},{"hooks":[{"type":"command","command":"report turn"}]},{"hooks":[{"type":"command","command":"herdr-agentisan report Turn"}]},{"hooks":[{"type":"command","command":"herdr-agentisan report turnover"}]}],"Stop":[` + ours("Stop", "/home/u/go/bin/herdr-agentisan  report turn >/dev/null 2>&1", "") + `]}}`),
			want:  `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ours("Stop", "/home/u/go/bin/herdr-agentisan  report turn >/dev/null 2>&1", "") + `,` + ctxStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[{"hooks":[{"type":"command","command":""}]},{"hooks":[{"type":"command","command":"report turn"}]},{"hooks":[{"type":"command","command":"herdr-agentisan report Turn"}]},{"hooks":[{"type":"command","command":"herdr-agentisan report turnover"}]},` + turnUPS + `]}}`,
			states: map[string]codexhooks.State{
				"codex PostToolUse": codexhooks.Added, "codex Stop": codexhooks.Added, "codex PostCompact": codexhooks.Added,
				"turn Stop": codexhooks.Unchanged, "turn UserPromptSubmit": codexhooks.Added,
			},
			changed: true,
		},
		"an old turn hook, brought in line in place with its own command": {
			start: new(`{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ctxStop + `,` + turnStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/opt/bin/herdr-agentisan report turn","timeout":5}]},` + userB + `]}}`),
			want:  `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ctxStop + `,` + turnStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[` + ours("UserPromptSubmit", "/opt/bin/herdr-agentisan report turn", "") + `,` + userB + `]}}`,
			states: map[string]codexhooks.State{
				"codex PostToolUse": codexhooks.Unchanged, "codex Stop": codexhooks.Unchanged, "codex PostCompact": codexhooks.Unchanged,
				"turn Stop": codexhooks.Unchanged, "turn UserPromptSubmit": codexhooks.Updated,
			},
			changed: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := filepath.Join(home, codexhooks.FileName)
			if tc.start != nil {
				write(t, path, *tc.start, 0o600)
			}

			res, err := codexhooks.InstallWithTurn(home)
			require.NoError(t, err)
			assert.Equal(t, tc.changed, res.Changed)
			assert.Equal(t, tc.states, byReport(res))
			assert.Equal(t, fiveInOrder, reports(res))
			for _, h := range res.Hooks {
				assert.Equal(t, path, h.Source)
			}
			assert.True(t, res.Installed())
			assert.True(t, res.TurnInstalled())
			got := read(t, path)
			assert.Equal(t, decode(t, tc.want), decode(t, got), got)
			if !tc.changed {
				assert.Equal(t, *tc.start, got, "an unchanged file is not rewritten")
			}

			again, err := codexhooks.InstallWithTurn(home)
			require.NoError(t, err)
			assert.False(t, again.Changed, "a second install changes nothing")
			assert.Equal(t, got, read(t, path))
		})
	}
}

// TestInstallLeavesTheTurnHooksAlone: a plain install is the ctx hooks
// alone, as before the turn hooks: it never touches one, even one it could
// not tell apart.
func TestInstallLeavesTheTurnHooksAlone(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, codexhooks.FileName)
	messy := `{"hooks":[{"type":"command","command":"notify"},{"type":"command","command":"herdr-agentisan report turn"}]}`
	write(t, path, `{"hooks":{"Stop":[`+turnStop+`],"UserPromptSubmit":[`+messy+`]}}`, 0o600)

	res, err := codexhooks.Install(home)
	require.NoError(t, err)
	assert.Equal(t, []string{"codex PostToolUse", "codex Stop", "codex PostCompact"}, reports(res))
	want := `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + turnStop + `,` + ctxStop + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[` + messy + `]}}`
	assert.Equal(t, decode(t, want), decode(t, read(t, path)))
	assert.False(t, res.TurnInstalled(), "a plain install reports no turn hook")
}

func TestInstallWithTurnRefuses(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"report turn beside another hook": `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"notify"},{"type":"command","command":"herdr-agentisan report turn"}]}]}}`,
		"report turn twice in one event":  `{"hooks":{"Stop":[` + turnStop + `,` + userB + `,` + ours("Stop", "/x/herdr-agentisan report turn", "") + `]}}`,
		"both reports in one group":       `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"herdr-agentisan report codex"},{"type":"command","command":"herdr-agentisan report turn"}]}]}}`,
	}
	for name, start := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := filepath.Join(home, codexhooks.FileName)
			write(t, path, start, 0o600)

			_, err := codexhooks.InstallWithTurn(home)
			require.ErrorIs(t, err, codexhooks.ErrAmbiguous)
			assert.Contains(t, err.Error(), path)
			assert.Equal(t, start, read(t, path), "a refused file is left as it was")
			_, err = codexhooks.CheckWithTurn(home)
			require.ErrorIs(t, err, codexhooks.ErrAmbiguous, "check refuses what install refuses")
			assertOnly(t, home, codexhooks.FileName)
		})
	}
}

// TestInstallWithTurnLeavesConfigTOMLTurnHooksAlone: a config.toml that
// already runs `report turn`, as the pasted sample does, refuses the turn
// install with nothing written, the ctx hooks included, since a second copy
// would run every report twice; a plain install is not refused for it.
func TestInstallWithTurnLeavesConfigTOMLTurnHooksAlone(t *testing.T) {
	t.Parallel()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "codex-turn-hooks.example.toml"))
	require.NoError(t, err)

	t.Run("the turn install refuses", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		config := filepath.Join(home, "config.toml")
		write(t, config, string(sample), 0o600)

		res, err := codexhooks.InstallWithTurn(home)
		require.ErrorIs(t, err, codexhooks.ErrInConfigTOML)
		assert.Contains(t, err.Error(), config)
		assert.Contains(t, err.Error(), turnCmd)
		assert.Equal(t, five(codexhooks.Missing, codexhooks.Present), byReport(res))
		assert.Equal(t, string(sample), read(t, config), "config.toml is never written")
		assertOnly(t, home, "config.toml")
	})
	t.Run("a plain install does not", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		write(t, filepath.Join(home, "config.toml"), string(sample), 0o600)

		res, err := codexhooks.Install(home)
		require.NoError(t, err)
		assert.True(t, res.Installed())
		assert.Equal(t, decode(t, installed), decode(t, read(t, filepath.Join(home, codexhooks.FileName))))
	})
	t.Run("a check reads them there", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		config := filepath.Join(home, "config.toml")
		write(t, config, string(sample), 0o600)
		write(t, filepath.Join(home, codexhooks.FileName), installed, 0o600)

		res, err := codexhooks.CheckWithTurn(home)
		require.NoError(t, err)
		assert.Equal(t, five(codexhooks.Present, codexhooks.Present), byReport(res))
		for _, h := range res.Hooks {
			want := filepath.Join(home, codexhooks.FileName)
			if h.Report == "turn" {
				want = config
			}
			assert.Equal(t, want, h.Source, h.Report+" "+h.Event)
		}
		assert.True(t, res.TurnInstalled())
	})
}

func TestCheckWithTurn(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		hooks       *string
		want        map[string]codexhooks.State
		ctx, turned bool
	}{
		"nothing installed":  {want: five(codexhooks.Missing, codexhooks.Missing)},
		"all five":           {hooks: new(allFive), want: five(codexhooks.Present, codexhooks.Present), ctx: true, turned: true},
		"the ctx hooks only": {hooks: new(installed), want: five(codexhooks.Present, codexhooks.Missing), ctx: true},
		// Each report's hooks are told apart by their report, not their
		// event alone: Stop runs both.
		"the turn hooks only": {
			hooks:  new(`{"hooks":{"Stop":[` + turnStop + `],"UserPromptSubmit":[` + turnUPS + `]}}`),
			want:   five(codexhooks.Missing, codexhooks.Present),
			turned: true,
		},
		"an old turn hook": {
			hooks: new(`{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ctxStop + `,{"hooks":[{"type":"command","command":"herdr-agentisan report turn","timeout":10}]}],"PostCompact":[` + ours("PostCompact", cmd, "") + `],"UserPromptSubmit":[` + turnUPS + `]}}`),
			want: map[string]codexhooks.State{
				"codex PostToolUse": codexhooks.Present, "codex Stop": codexhooks.Present, "codex PostCompact": codexhooks.Present,
				"turn Stop": codexhooks.Differs, "turn UserPromptSubmit": codexhooks.Present,
			},
			ctx: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if tc.hooks != nil {
				write(t, filepath.Join(home, codexhooks.FileName), *tc.hooks, 0o600)
			}
			res, err := codexhooks.CheckWithTurn(home)
			require.NoError(t, err)
			assert.False(t, res.Changed)
			assert.Equal(t, tc.want, byReport(res))
			assert.Equal(t, fiveInOrder, reports(res))
			assert.Equal(t, tc.ctx, res.Installed())
			assert.Equal(t, tc.turned, res.TurnInstalled())
		})
	}

	t.Run("a plain check reports the ctx hooks alone", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		write(t, filepath.Join(home, codexhooks.FileName), allFive, 0o600)
		res, err := codexhooks.Check(home)
		require.NoError(t, err)
		assert.Equal(t, []string{"codex PostToolUse", "codex Stop", "codex PostCompact"}, reports(res))
		assert.True(t, res.Installed())
		assert.False(t, res.TurnInstalled())
	})
}

// TestTurnInstalledIn: Codex runs the turn hooks only once both are
// installed and config.toml leaves hooks on; the ctx hooks alone are not
// them.
func TestTurnInstalledIn(t *testing.T) {
	t.Parallel()
	lookup := func(home string) func(string) (string, bool) {
		return func(k string) (string, bool) { return home, k == "CODEX_HOME" }
	}
	home := t.TempDir()
	turned, err := codexhooks.TurnInstalledIn(lookup(home))
	require.NoError(t, err)
	assert.False(t, turned, "none yet")

	_, err = codexhooks.Install(home)
	require.NoError(t, err)
	turned, err = codexhooks.TurnInstalledIn(lookup(home))
	require.NoError(t, err)
	assert.False(t, turned, "the ctx hooks are not the turn hooks")

	_, err = codexhooks.InstallWithTurn(home)
	require.NoError(t, err)
	turned, err = codexhooks.TurnInstalledIn(lookup(home))
	require.NoError(t, err)
	assert.True(t, turned)

	write(t, filepath.Join(home, "config.toml"), "[features]\nhooks = false\n", 0o600)
	turned, err = codexhooks.TurnInstalledIn(lookup(home))
	require.NoError(t, err)
	assert.False(t, turned, "Codex runs no hook with hooks off")

	_, err = codexhooks.TurnInstalledIn(func(string) (string, bool) { return "", false })
	require.ErrorIs(t, err, codexhooks.ErrNoHome)
	bad := t.TempDir()
	write(t, filepath.Join(bad, codexhooks.FileName), "{not json", 0o600)
	_, err = codexhooks.TurnInstalledIn(lookup(bad))
	require.ErrorIs(t, err, codexhooks.ErrInvalid)
}

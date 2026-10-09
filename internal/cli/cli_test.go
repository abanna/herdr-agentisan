package cli_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/config"
)

// run executes a fresh command tree with args and returns combined output.
// Cobra writes both streams to the buffer we set.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()

	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// TestRootIsNamedAfterTheRepository: herdr invokes the plugin by the binary
// name in its manifest, and `--help` is how an agent discovers the tool, so
// the root must carry the repository's name and none of the template's.
func TestRootIsNamedAfterTheRepository(t *testing.T) {
	t.Parallel()

	root := cli.Root()
	assert.Equal(t, "herdr-agentisan", root.Name())
	for _, sub := range root.Commands() {
		assert.NotEqual(t, "notes", sub.Name(), "the template's notes tree must be gone")
	}
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args  []string
		check func(t *testing.T, out string)
	}{
		"plain text names version and commit": {
			args: []string{"version"},
			check: func(t *testing.T, out string) {
				t.Helper()
				assert.Contains(t, out, config.Version)
				assert.Contains(t, out, config.Commit)
			},
		},
		"--json is machine-readable": {
			args: []string{"version", "--json"},
			check: func(t *testing.T, out string) {
				t.Helper()
				var got map[string]string
				require.NoError(t, json.Unmarshal([]byte(out), &got), "output: %s", out)
				assert.Equal(t, config.Version, got["version"])
				assert.Equal(t, config.Commit, got["commit"])
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := run(t, tc.args...)
			require.NoError(t, err)
			tc.check(t, out)
		})
	}
}

// TestVersionRejectsUnexpectedArguments: herdr passes a fixed argv from the
// manifest, so anything extra is a manifest or caller bug and must fail loudly
// rather than be ignored.
func TestVersionRejectsUnexpectedArguments(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"positional argument":        {"version", "extra"},
		"unknown flag":               {"version", "--bogus"},
		"flag-like operand after --": {"version", "--", "--json"},
		"argument with whitespace":   {"version", "two words"},
		"non-UTF-8 argument":         {"version", "a\xffb"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := run(t, args...)
			require.Error(t, err)
		})
	}
}

// TestEveryCommandIsDocumented guards the help text agents read to discover
// the tool: a command with no Short renders as a blank line in `--help`.
func TestEveryCommandIsDocumented(t *testing.T) {
	t.Parallel()

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		assert.NotEmptyf(t, c.Short, "command %q has no Short description", c.CommandPath())
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cli.Root())
}

// TestTwoTreesDoNotShareFlagState guards against binding a flag to a
// package-level variable: cobra writes to the address it is given, so a global
// would make two trees in one process overwrite each other's flag. This
// surfaced as cross-test contamination only when the suite ran in parallel.
func TestTwoTreesDoNotShareFlagState(t *testing.T) {
	t.Parallel()

	versionOf := func(root *cobra.Command) *cobra.Command {
		t.Helper()
		c, _, err := root.Find([]string{"version"})
		require.NoError(t, err)
		return c
	}
	a, b := versionOf(cli.Root()), versionOf(cli.Root())

	require.NoError(t, a.Flags().Set("json", "true"))

	assert.Equal(t, "true", a.Flags().Lookup("json").Value.String())
	assert.Equal(t, "false", b.Flags().Lookup("json").Value.String(),
		"setting one tree's --json must not leak into another")
}

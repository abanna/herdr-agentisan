package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
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

// pingCtx injects an environment pointing at srv, so the command never sees
// the developer's real HERDR_SOCKET_PATH.
func pingCtx(t *testing.T, socket string) context.Context {
	t.Helper()
	return cli.WithLookupEnv(t.Context(), func(k string) (string, bool) {
		if k == "HERDR_SOCKET_PATH" && socket != "" {
			return socket, true
		}
		return "", false
	})
}

func runCtx(ctx context.Context, args ...string) (string, error) {
	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func toastReply(shown bool, reason string) herdrtest.Handler {
	return func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: map[string]any{"type": "notification_show", "shown": shown, "reason": reason}}
	}
}

func TestActionPing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		shown  bool
		reason string
		args   []string
		check  func(t *testing.T, out string)
	}{
		"shown, text": {
			shown: true, reason: "shown", args: []string{"action", "ping"},
			check: func(t *testing.T, out string) { t.Helper(); assert.Contains(t, out, "notification shown") },
		},
		"declined, text names the reason": {
			shown: false, reason: "rate_limited", args: []string{"action", "ping"},
			check: func(t *testing.T, out string) { t.Helper(); assert.Contains(t, out, "rate_limited") },
		},
		"json": {
			shown: true, reason: "shown", args: []string{"action", "ping", "--json"},
			check: func(t *testing.T, out string) {
				t.Helper()
				assert.JSONEq(t, `{"shown":true,"reason":"shown"}`, out)
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, toastReply(tc.shown, tc.reason))

			out, err := runCtx(pingCtx(t, srv.Path), tc.args...)
			require.NoError(t, err)
			tc.check(t, out)
			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "notification.show", reqs[0].Method)
		})
	}
}

func TestActionPingRejectsArguments(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"positional":   {"action", "ping", "extra"},
		"unknown flag": {"action", "ping", "--bogus"},
		"unknown verb": {"action", "pong"},
		"bare group":   {"action"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, toastReply(true, "shown"))
			_, err := runCtx(pingCtx(t, srv.Path), args...)
			require.Error(t, err)
			assert.Empty(t, srv.Requests(), "a rejected invocation must not reach herdr")
		})
	}
}

func TestActionPingOutsideHerdrFailsClearly(t *testing.T) {
	t.Parallel()

	_, err := runCtx(pingCtx(t, ""), "action", "ping")
	require.ErrorIs(t, err, herdr.ErrNoSocket)
	assert.ErrorContains(t, err, "HERDR_SOCKET_PATH")
}

func TestActionPingSurfacesHerdrErrors(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "internal", Message: "boom"}}
	})

	_, err := runCtx(pingCtx(t, srv.Path), "action", "ping")
	require.ErrorIs(t, err, herdr.ErrAPI)
}

// TestManifestMatchesTheBinary is the drift gate between herdr-plugin.toml and
// the cobra tree. `herdr plugin link` runs no build and herdr only warns about
// a bad manifest, so without this a typo ships and fails in a live herdr.
func TestManifestMatchesTheBinary(t *testing.T) {
	t.Parallel()

	m, err := plugin.LoadManifest(filepath.Join("..", "..", plugin.ManifestFile))
	require.NoError(t, err)
	require.NotEmpty(t, m.Actions, "the manifest declares no actions")

	// The Taskfile builds the same path the manifest's build step writes.
	taskfile, err := os.ReadFile(filepath.Join("..", "..", "Taskfile.yml"))
	require.NoError(t, err)
	assert.Contains(t, string(taskfile), "BINARY_DIR: bin")
	assert.Equal(t, "bin/herdr-agentisan", m.BuildOutput())
	assert.Contains(t, string(taskfile), "-o {{.BINARY_DIR}}/herdr-agentisan ./cmd/herdr-agentisan")

	root := cli.Root()
	for _, a := range m.Actions {
		t.Run(a.ID, func(t *testing.T) {
			require.NotEmpty(t, a.Command)
			assert.Equal(t, m.BuildOutput(), a.Command[0], "action must run the binary the build step produces")

			cmd, rest, err := root.Find(a.Command[1:])
			require.NoError(t, err)
			// Find stops at the deepest match and returns the remainder, so
			// "action bogus" resolves to `action` with ["bogus"] left over.
			assert.Empty(t, rest, "argv %v does not name a command exactly", a.Command[1:])
			assert.NotSame(t, root, cmd, "argv %v resolves to the root", a.Command[1:])
			assert.True(t, cmd.Runnable(), "%q is not runnable", cmd.CommandPath())
			assert.False(t, cmd.HasSubCommands(), "%q is a group, not an action", cmd.CommandPath())
		})
	}
}

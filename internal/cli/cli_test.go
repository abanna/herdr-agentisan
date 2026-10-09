package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/report"
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

// reportCtx injects a pane environment pointing at socket and a debug logger
// writing to logs. Empty values stay unset, so a test never sees the
// developer's real HERDR_SOCKET_PATH or HERDR_PANE_ID and cannot push a token
// into a live pane.
func reportCtx(t *testing.T, socket, pane string, logs *bytes.Buffer) context.Context {
	t.Helper()
	env := map[string]string{}
	if socket != "" {
		env["HERDR_SOCKET_PATH"] = socket
	}
	if pane != "" {
		env["HERDR_PANE_ID"] = pane
	}
	logger, err := logging.New(logs, "debug", "json")
	require.NoError(t, err)
	ctx := logging.Into(t.Context(), logger)
	return cli.WithLookupEnv(ctx, func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
}

// runReport executes `report statusline` with stdin and returns everything the
// command wrote to stdout and stderr together.
func runReport(ctx context.Context, stdin string, args ...string) (string, error) {
	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"report", "statusline"}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

const statusline426 = `{"model":{"display_name":"Opus"},"context_window":{"used_percentage":42.6}}`

func TestReportStatuslinePushesCtx(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
	})
	var logs bytes.Buffer

	out, err := runReport(reportCtx(t, srv.Path, "w14:p1", &logs), statusline426+"\n")
	require.NoError(t, err)
	assert.Empty(t, out, "the statusline runs this on every refresh: it must print nothing")

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "pane.report_metadata", reqs[0].Method)
	assert.JSONEq(t, `{"pane_id":"w14:p1","source":"agentisan","tokens":{"ctx":"43"},"ttl_ms":180000}`, string(reqs[0].Params))
	assert.Contains(t, logs.String(), `"ctx":43`)
}

// TestReportStatuslineDoesNothing covers every case the issue says must do
// nothing: exit 0, no herdr call, and no output at all, so a statusline that
// forgot the redirect still renders cleanly. The socket points at herdrtest
// wherever it is set, so "no herdr call" is observed, not assumed.
func TestReportStatuslineDoesNothing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		noSocket bool
		pane     string
		stdin    string
		want     error
	}{
		"HERDR_SOCKET_PATH unset": {noSocket: true, pane: "w1:p1", stdin: statusline426, want: report.ErrNotInPane},
		"HERDR_PANE_ID unset":     {stdin: statusline426, want: report.ErrNotInPane},
		"percentage missing":      {pane: "w1:p1", stdin: `{"context_window":{}}`, want: report.ErrNoContext},
		"percentage malformed":    {pane: "w1:p1", stdin: `{"context_window":{"used_percentage":"42%"}}`, want: report.ErrMalformed},
		"stdin empty":             {pane: "w1:p1", stdin: "", want: report.ErrMalformed},
		"percentage out of range": {pane: "w1:p1", stdin: `{"context_window":{"used_percentage":101}}`, want: report.ErrMalformed},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
			})
			socket := srv.Path
			if tc.noSocket {
				socket = ""
			}
			var logs bytes.Buffer

			out, err := runReport(reportCtx(t, socket, tc.pane, &logs), tc.stdin)
			require.NoError(t, err)
			assert.Empty(t, out)
			assert.Empty(t, srv.Requests(), "no herdr call")
			assert.Contains(t, logs.String(), tc.want.Error(), "the reason goes to the debug log")
		})
	}
}

// TestReportStatuslineSwallowsHerdrErrors: a herdr failure is logged at debug
// level and nowhere else, and the command still exits 0.
func TestReportStatuslineSwallowsHerdrErrors(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane w1:p1 not found"}}
	})
	var logs bytes.Buffer

	out, err := runReport(reportCtx(t, srv.Path, "w1:p1", &logs), statusline426)
	require.NoError(t, err)
	assert.Empty(t, out)
	require.Len(t, srv.Requests(), 1)
	assert.Contains(t, logs.String(), "pane_not_found")
}

func TestReportRejectsArguments(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"positional":   {"report", "statusline", "extra"},
		"unknown flag": {"report", "statusline", "--bogus"},
		"unknown verb": {"report", "bogus"},
		"bare group":   {"report"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
			})
			var logs bytes.Buffer

			root := cli.Root()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetIn(strings.NewReader(statusline426))
			root.SetArgs(args)
			require.Error(t, root.ExecuteContext(reportCtx(t, srv.Path, "w1:p1", &logs)))
			assert.Empty(t, srv.Requests(), "a rejected invocation must not reach herdr")
		})
	}
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

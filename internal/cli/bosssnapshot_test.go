package cli_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
)

// NERD-5748: the two output formats make the same selector-free request.
func TestBossSnapshotCLI(t *testing.T) {
	t.Parallel()
	const reply = `{"v":1,"team":"alpha","at":"2026-11-24T09:00:02Z","team_poll":{"state":"fresh","as_of":"2026-11-24T09:00:00Z"},"boss":null,"groups":[{"name":"coder","workers":[{"id":null,"generation":null,"pane_id":"p1","name":"a\n\u001b[31m\u202eb","agent":"claude","status":"working","item":null,"stage":null,"stage_at":null,"stage_stale":false,"idle_since":null,"ctx":0}]}],"workers_omitted":0,"board":{"state":"fresh","as_of":"2026-11-24T09:00:02Z","mark":0,"newest":0,"unread":0}}`
	for _, format := range []string{"text", "json", "refused", "malformed"} {
		t.Run(format, func(t *testing.T) {
			e := newDaemonEnvWith(t, failingHerdr)
			answer := `{"ok":true,"data":` + reply + "}"
			switch format {
			case "refused":
				answer = `{"ok":false,"error":{"code":"not_allowed","message":"PRIVATE herdr error"}}`
			case "malformed":
				answer = strings.Replace(answer, `"team":`, `"TEAM":`, 1)
			}
			sent := fakeDaemon(t, e, answer)
			args := []string{"boss", "snapshot"}
			if format == "json" {
				args = append(args, "--json")
			}
			out, _, err := runOut(t, e.lookup(nil), args...)
			require.Len(t, sent, 1)
			assert.JSONEq(t, `{"v":1,"op":"boss.snapshot","args":{}}`, <-sent)
			switch format {
			case "json":
				require.NoError(t, err)
				assert.JSONEq(t, reply, out)
			case "refused", "malformed":
				assert.Equal(t, 1, cli.ExitCode(err))
				assert.Empty(t, out)
				assert.NotContains(t, err.Error(), "PRIVATE")
			default:
				require.NoError(t, err)
				assert.Contains(t, out, "alpha")
				assert.Contains(t, out, "2s")
				assert.Contains(t, out, "Board (state.db)")
				assert.Contains(t, out, `a\n\x1b[31m\u202eb`)
				assert.NotContains(t, out, "\x1b")
				assert.NotContains(t, out, "\u202e")
			}
		})
	}
	_, _, err := runOut(t, newDaemonEnvWith(t, failingHerdr).lookup(nil), "boss", "snapshot", "--project", "beta")
	require.ErrorContains(t, err, "unknown flag")
}

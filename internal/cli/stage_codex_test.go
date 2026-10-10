package cli_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// TestReportStageInCodexSandbox: the command reads Codex's variables from
// the pane environment, and a step run in Codex's network sandbox reports
// nothing, prints nothing, exits 0 and says why in the debug log (A25).
func TestReportStageInCodexSandbox(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, paneHerdr("w15:p4"))
	var logs bytes.Buffer
	env := map[string]string{
		"HERDR_SOCKET_PATH": srv.Path, "HERDR_PANE_ID": "w15:p4",
		"CODEX_THREAD_ID": "019a", "CODEX_SANDBOX_NETWORK_DISABLED": "1",
	}
	ctx := cli.WithLookupEnv(reportCtx(t, "", "", &logs), func(k string) (string, bool) { v, ok := env[k]; return v, ok })

	stdout, stderr, err := runStage(ctx, "--item=NERD-5279", "--stage=build_test")
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
	assert.Empty(t, srv.Requests(), "no herdr call")
	assert.Contains(t, logs.String(), report.ErrCodexSandboxed.Error())
}

package workforce_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestCheckReach: a command Codex ran in its network sandbox is refused as
// report.ErrCodexSandboxed, its text first, then the cause and who to ask;
// any other, Codex's outside that sandbox included, passes (A25). The
// variable reads as report.PaneFrom reads it: set to "" is not sandboxed.
func TestCheckReach(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env       map[string]string
		sandboxed bool
	}{
		"Codex's network sandbox":           {env: map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "1", "CODEX_THREAD_ID": "t1"}, sandboxed: true},
		"the variable alone":                {env: map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "1"}, sandboxed: true},
		"any value":                         {env: map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "true"}, sandboxed: true},
		"set empty":                         {env: map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": ""}},
		"Codex outside the network sandbox": {env: map[string]string{"CODEX_THREAD_ID": "t1"}},
		"no Codex":                          {env: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := workforce.CheckReach(report.PaneFrom(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }))
			if !tc.sandboxed {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, report.ErrCodexSandboxed)
			assert.Equal(t, "codex ran the command in its network sandbox: its seccomp filter denies every connect, the daemon's socket and herdr's included, "+
				"so it cannot run here; ask your boss to run it for you on your turn's last line (BOSS: …), as the boss marks handoff ready <your id> (ADR-001 A33)", err.Error())
		})
	}
}

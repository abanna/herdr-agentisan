package workforce

import (
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// CheckReach is why a command Codex ran in its network sandbox reached
// neither herdr nor the daemon (ADR-001 A25): the sandbox's seccomp filter
// denies every connect, their sockets included, and the dial's own error
// names no cause. The worker asks its boss instead, who runs the command for
// it, as the boss marks handoff ready for it (A33). The error is
// report.ErrCodexSandboxed, wrapped; nil for a command Codex did not run in
// that sandbox. Ask it only once a dial has failed: Codex sets the sandbox's
// variable from the turn's network policy, so a command whose escalation it
// approved carries the variable too, and reaches both.
func CheckReach(pane report.Pane) error {
	if !pane.CodexNetworkSandboxed {
		return nil
	}
	return fmt.Errorf("%w: its seccomp filter denies every connect, the daemon's socket and herdr's included, "+
		"so it cannot run here; ask your boss to run it for you on your turn's last line (BOSS: …), as the boss marks handoff ready <your id> (ADR-001 A33)", report.ErrCodexSandboxed)
}

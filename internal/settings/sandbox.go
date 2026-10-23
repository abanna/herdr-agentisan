package settings

import (
	"slices"
	"strings"
)

// Codex's sandbox modes, as codex --sandbox takes them.
const (
	SandboxReadOnly   = "read-only"
	SandboxFullAccess = "danger-full-access"
)

// sandboxes are the worker types with a sandbox of their own for Codex
// (ADR-001 A59). The reviewer reads only (A55). A coder, qa or precheck
// worker runs the repo's tests, git, signing, gh and the team's files, which
// only Codex's full access allows, as a Claude worker runs with no OS
// sandbox. Every other type keeps Codex's own configuration.
var sandboxes = map[string]string{
	ReviewType: SandboxReadOnly,
	"coder":    SandboxFullAccess,
	"qa":       SandboxFullAccess,
	"precheck": SandboxFullAccess,
}

// SandboxFor is the sandbox a Codex worker of type typ starts in when its
// command chooses none; "" leaves it to Codex's own configuration.
func SandboxFor(typ string) string { return sandboxes[typ] }

// sandboxFlags choose a Codex sandbox by themselves, with their hidden
// aliases in Codex 0.163.0: --approve-for-me (--not-so-yolo) runs
// workspace-write with automatic review, and the bypass (--yolo) runs none.
// --approve-for-me also conflicts with --sandbox, so Codex would refuse to
// start were one added to it.
var sandboxFlags = []string{
	"--approve-for-me", "--not-so-yolo",
	"--dangerously-bypass-approvals-and-sandbox", "--yolo",
}

// sandboxKeys are the Codex settings that choose a sandbox: sandbox_mode,
// and default_permissions, which picks a permissions profile. A --sandbox
// flag would silently override either.
var sandboxKeys = []string{"sandbox_mode", "default_permissions"}

// ChoosesSandbox reports whether a codex command's arguments choose its
// sandbox: -s or --sandbox in any form (-s x, -sx, -s=x, --sandbox x,
// --sandbox=x), one of sandboxFlags, or a -c or --config setting one of
// sandboxKeys. The user's choice then stands, and the workforce adds none.
// A profile (-p) or Codex's config.toml can set either key too, but the
// flag the workforce adds outranks both, by Codex's own precedence.
func ChoosesSandbox(args []string) bool {
	for i, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		if !strings.HasPrefix(name, "--") && len(name) > 2 {
			name = name[:2]
		}
		if name == "-s" || name == "--sandbox" || slices.Contains(sandboxFlags, arg) || slices.Contains(sandboxKeys, codexConfigKey(args, i)) {
			return true
		}
	}
	return false
}

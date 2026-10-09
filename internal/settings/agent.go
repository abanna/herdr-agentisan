package settings

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// agentPrograms are the agents Agentisan can run a workflow in (D12),
// matched on the base name of agent.command's first element.
var agentPrograms = []string{"claude", "codex"}

const settingSourcesFlag = "--setting-sources"

// CheckAgentCommand enforces D12 on an agent argv: it must name claude or
// codex, and must not start a session that skips the repo's Agentisan
// bundle, either with --bare or with a --setting-sources list that leaves out
// project. Whether the bundle is installed is checked by Load, which knows
// the repo.
func CheckAgentCommand(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%w: agent.command is empty", ErrBypassesAgentisan)
	}
	if prog := filepath.Base(argv[0]); !slices.Contains(agentPrograms, prog) {
		return fmt.Errorf("%w: %q is not %s", ErrBypassesAgentisan, prog, strings.Join(agentPrograms, " or "))
	}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--bare" || strings.HasPrefix(arg, "--bare="):
			return fmt.Errorf("%w: --bare skips the repo's Agentisan bundle", ErrBypassesAgentisan)
		case arg == settingSourcesFlag:
			i++
			if i >= len(argv) || !includesProject(argv[i]) {
				return errSettingSources()
			}
		case strings.HasPrefix(arg, settingSourcesFlag+"="):
			if !includesProject(strings.TrimPrefix(arg, settingSourcesFlag+"=")) {
				return errSettingSources()
			}
		}
	}
	return nil
}

func errSettingSources() error {
	return fmt.Errorf("%w: %s must include project, or the repo's Agentisan bundle is not loaded", ErrBypassesAgentisan, settingSourcesFlag)
}

// includesProject reports whether a comma-separated setting-sources list
// names project.
func includesProject(list string) bool {
	for s := range strings.SplitSeq(list, ",") {
		if strings.TrimSpace(s) == "project" {
			return true
		}
	}
	return false
}

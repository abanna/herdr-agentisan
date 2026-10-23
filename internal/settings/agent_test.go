package settings_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestCheckAgentCommand: D12 refuses any agent command that would start a
// session outside Agentisan.
func TestCheckAgentCommand(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		argv []string
		ok   bool
	}{
		"claude":                                {argv: []string{"claude"}, ok: true},
		"claude by absolute path":               {argv: []string{"/usr/bin/claude"}, ok: true},
		"codex":                                 {argv: []string{"codex"}, ok: true},
		"codex by path, with flags":             {argv: []string{"/opt/codex/bin/codex", "--model", "o3"}, ok: true},
		"claude with a model":                   {argv: []string{"claude", "--model", "opus"}, ok: true},
		"another agent":                         {argv: []string{"aider"}},
		"a wrapper named after claude":          {argv: []string{"/usr/bin/claude-wrapper"}},
		"claude behind a launcher":              {argv: []string{"env", "claude"}},
		"empty command":                         {argv: []string{}},
		"nil command":                           {argv: nil},
		"empty program":                         {argv: []string{""}},
		"claude in another case":                {argv: []string{"Claude"}},
		"claude under a non-ASCII directory":    {argv: []string{"/opt/clàude/bin/claude"}, ok: true},
		"a near-miss non-ASCII name":            {argv: []string{"claudé"}},
		"invalid UTF-8 in the program":          {argv: []string{"claude\xff"}},
		"a NUL in the program":                  {argv: []string{"claude\x00"}},
		"program and flag in one element":       {argv: []string{"claude --bare"}},
		"an option as the program":              {argv: []string{"--bare"}},
		"a Windows program name":                {argv: []string{"claude.exe"}},
		"--bare":                                {argv: []string{"claude", "--bare"}},
		"--bare after other flags":              {argv: []string{"claude", "--model", "opus", "--bare"}},
		"--bare=true":                           {argv: []string{"claude", "--bare=true"}},
		"codex --bare":                          {argv: []string{"codex", "--bare"}},
		"--setting-sources=... with project":    {argv: []string{"claude", "--setting-sources=user,project,local"}, ok: true},
		"--setting-sources=project alone":       {argv: []string{"claude", "--setting-sources=project"}, ok: true},
		"--setting-sources ... with project":    {argv: []string{"claude", "--setting-sources", "user,project"}, ok: true},
		"--setting-sources spaced list":         {argv: []string{"claude", "--setting-sources", "user, project"}, ok: true},
		"--setting-sources=... without project": {argv: []string{"claude", "--setting-sources=user,local"}},
		"--setting-sources ... without project": {argv: []string{"claude", "--setting-sources", "user"}},
		"--setting-sources= empty":              {argv: []string{"claude", "--setting-sources="}},
		"--setting-sources with no value":       {argv: []string{"claude", "--setting-sources"}},
		"--setting-sources projects is not it":  {argv: []string{"claude", "--setting-sources=projects"}},

		// D12 knows nothing of routing: every Codex flag P7 sorts passes it,
		// and the test each row names refuses or allows it (A50).
		"codex --oss (refused: TestRoutingRefusesACodexProvider)":                              {argv: []string{"codex", "--oss"}, ok: true},
		"codex --local-provider (refused: TestRoutingRefusesACodexProvider)":                   {argv: []string{"codex", "--local-provider", "ollama"}, ok: true},
		"codex -c model_provider (refused: TestRoutingRefusesACodexProvider)":                  {argv: []string{"codex", "-c", "model_provider=oss"}, ok: true},
		"codex's hook-trust bypass (refused: TestLoadRefusesWorkforceKeys)":                    {argv: []string{"codex", "--dangerously-bypass-hook-trust"}, ok: true},
		"codex -c bypass_hook_trust (refused: TestLoadRefusesWorkforceKeys)":                   {argv: []string{"codex", "-c", "bypass_hook_trust=true"}, ok: true},
		"codex -c hooks.state (refused: TestLoadRefusesCodexHookState)":                        {argv: []string{"codex", "-c", `hooks.state={simple={trusted_hash="x"}}`}, ok: true},
		"codex -c hooks (refused: TestLoadRefusesCodexHookState)":                              {argv: []string{"codex", "-c", `hooks={state={simple={trusted_hash="x"}}}`}, ok: true},
		"codex -s (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":                     {argv: []string{"codex", "-s", "read-only"}, ok: true},
		"codex --sandbox (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":              {argv: []string{"codex", "--sandbox=workspace-write"}, ok: true},
		"codex --approve-for-me (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":       {argv: []string{"codex", "--approve-for-me"}, ok: true},
		"codex --not-so-yolo (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":          {argv: []string{"codex", "--not-so-yolo"}, ok: true},
		"codex's approvals bypass (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":     {argv: []string{"codex", "--dangerously-bypass-approvals-and-sandbox"}, ok: true},
		"codex --yolo (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":                 {argv: []string{"codex", "--yolo"}, ok: true},
		"codex -c sandbox_mode (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":        {argv: []string{"codex", "-c", "sandbox_mode=read-only"}, ok: true},
		"codex -c default_permissions (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)": {argv: []string{"codex", "-c", "default_permissions=mine"}, ok: true},
		"codex --search (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":               {argv: []string{"codex", "--search"}, ok: true},
		"codex --no-daemon (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":            {argv: []string{"codex", "--no-daemon"}, ok: true},
		"codex --add-dir (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":              {argv: []string{"codex", "--add-dir", "/srv/shared"}, ok: true},
		"codex -c oss_provider (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":        {argv: []string{"codex", "-c", "oss_provider=ollama"}, ok: true},
		"codex -c model_providers (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":     {argv: []string{"codex", "-c", "model_providers.mine.base_url=https://proxy.example/v1"}, ok: true},
		"codex -c openai_base_url (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":     {argv: []string{"codex", "-c", "openai_base_url=https://proxy.example/v1"}, ok: true},
		"codex -C (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":                     {argv: []string{"codex", "-C", "/srv/app"}, ok: true},
		"codex --cd (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":                   {argv: []string{"codex", "--cd=/srv/app"}, ok: true},
		"codex --worktree (allowed: TestRoutingKeepsTheCodexFlagsItDoesNotDecide)":             {argv: []string{"codex", "--worktree"}, ok: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := settings.CheckAgentCommand(tc.argv)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, settings.ErrBypassesAgentisan)
		})
	}
}

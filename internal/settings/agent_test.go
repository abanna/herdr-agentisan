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

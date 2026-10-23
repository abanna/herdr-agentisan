package settings_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestSandboxFor: a Codex coder, qa or precheck worker runs with full
// access, the reviewer read-only, and every other type keeps Codex's own
// configuration (A59).
func TestSandboxFor(t *testing.T) {
	t.Parallel()
	full, readOnly := settings.SandboxFullAccess, settings.SandboxReadOnly
	for name, tc := range map[string]struct{ typ, want string }{
		"coder":                        {"coder", full},
		"qa":                           {"qa", full},
		"precheck":                     {"precheck", full},
		"review":                       {settings.ReviewType, readOnly},
		"research":                     {"research", ""},
		"librarian":                    {"librarian", ""},
		"debugger":                     {settings.DebuggerType, ""},
		"a type one letter past coder": {"coders", ""},
		"another case":                 {"Coder", ""},
		"padded":                       {" coder", ""},
		"non-ASCII":                    {"codér", ""},
		"no type":                      {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, settings.SandboxFor(tc.typ))
		})
	}
	assert.Equal(t, "danger-full-access", full, "codex --sandbox's own name")
	assert.Equal(t, "read-only", readOnly, "codex --sandbox's own name")
}

// TestChoosesSandbox: a codex command chooses its sandbox with -s or
// --sandbox in any form, the bypass or --approve-for-me (or their hidden
// aliases), or a -c or --config setting sandbox_mode or default_permissions;
// a profile, a permissions profile defined but not picked, a workspace-write
// detail or any other flag chooses none.
func TestChoosesSandbox(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args    []string
		chooses bool
	}{
		"--sandbox x":                   {args: []string{"--sandbox", "workspace-write"}, chooses: true},
		"--sandbox=x":                   {args: []string{"--sandbox=read-only"}, chooses: true},
		"-s x":                          {args: []string{"--no-daemon", "-s", "workspace-write"}, chooses: true},
		"-sx":                           {args: []string{"-sread-only"}, chooses: true},
		"-s=x":                          {args: []string{"-s=danger-full-access"}, chooses: true},
		"the bypass":                    {args: []string{"--dangerously-bypass-approvals-and-sandbox"}, chooses: true},
		"--approve-for-me":              {args: []string{"--approve-for-me"}, chooses: true},
		"-c sandbox_mode":               {args: []string{"-c", `sandbox_mode="workspace-write"`}, chooses: true},
		"--config sandbox_mode":         {args: []string{"--config", "sandbox_mode=read-only"}, chooses: true},
		"--config=sandbox_mode":         {args: []string{"--config=sandbox_mode=read-only"}, chooses: true},
		"-c attached":                   {args: []string{"-csandbox_mode=read-only"}, chooses: true},
		"-c a quoted key":               {args: []string{"-c", `"sandbox_mode"="read-only"`}, chooses: true},
		"--not-so-yolo":                 {args: []string{"--not-so-yolo"}, chooses: true},
		"--yolo":                        {args: []string{"--yolo"}, chooses: true},
		"-c default_permissions":        {args: []string{"-c", `default_permissions=":workspace"`}, chooses: true},
		"--config=default_permissions":  {args: []string{"--config=default_permissions=work"}, chooses: true},
		"a permissions profile defined": {args: []string{"-c", "permissions.work.network=true"}},
		"no arguments":                  {},
		"--no-daemon alone":             {args: []string{"--no-daemon"}},
		"a profile":                     {args: []string{"-p", "work", "--profile=home"}},
		"a workspace-write detail":      {args: []string{"-c", "sandbox_workspace_write.network_access=true"}},
		"another flag":                  {args: []string{"--search", "--dangerously-bypass-hook-trust"}},
		"sandbox in another's value":    {args: []string{"-c", "notify=--sandbox"}},
		"a bare -s at the end":          {args: []string{"--no-daemon", "-s"}, chooses: true},
		"a bare -c at the end":          {args: []string{"--no-daemon", "-c"}},
		"--Sandbox":                     {args: []string{"--Sandbox", "read-only"}},
		"-S":                            {args: []string{"-S", "read-only"}},
		"a non-ASCII flag":              {args: []string{"--sändbox", "read-only"}},
		"one element with a space":      {args: []string{"--sandbox read-only"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.chooses, settings.ChoosesSandbox(tc.args))
		})
	}
}

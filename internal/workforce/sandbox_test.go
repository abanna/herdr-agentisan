package workforce_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestSpawnGivesCodexItsTypesSandbox: a Codex coder, qa or precheck starts
// with full access and a Codex reviewer read-only, whether its command comes
// from [agent], its group or the review type's own (A59). The flag goes
// right after --no-daemon, before the model and effort. A sandbox the
// command chooses itself stands, and nothing is added. Other types and
// Claude get none.
func TestSpawnGivesCodexItsTypesSandbox(t *testing.T) {
	t.Parallel()
	full, readOnly := []any{"--no-daemon", "--sandbox", "danger-full-access"}, []any{"--no-daemon", "--sandbox", "read-only"}
	for name, tc := range map[string]struct {
		top   string // the [agent] command, if any
		group string
		agent string // the group's agent table, if any
		req   workforce.SpawnRequest
		want  []any
	}{
		"a coder running [agent]'s codex":   {top: `["codex"]`, group: "coder", want: full},
		"a qa's own codex":                  {group: "qa", agent: `{ command = ["codex"] }`, want: full},
		"a precheck's own codex":            {group: "precheck", agent: `{ command = ["codex"] }`, want: full},
		"the review type's built-in":        {group: "review", want: readOnly},
		"a review command choosing none":    {group: "review", agent: `{ command = ["codex"] }`, want: readOnly},
		"a review command choosing its own": {group: "review", agent: `{ command = ["codex", "-s", "workspace-write"] }`, want: []any{"-s", "workspace-write", "--no-daemon"}},
		"a research's codex":                {group: "research", agent: `{ command = ["codex"] }`, want: []any{"--no-daemon"}},
		"a librarian's codex":               {group: "librarian", agent: `{ command = ["codex"] }`, want: []any{"--no-daemon"}},
		"a coder's own --sandbox":           {group: "coder", agent: `{ command = ["codex", "--sandbox=workspace-write"] }`, want: []any{"--sandbox=workspace-write", "--no-daemon"}},
		"a coder's bypass":                  {group: "coder", agent: `{ command = ["codex", "--dangerously-bypass-approvals-and-sandbox"] }`, want: []any{"--dangerously-bypass-approvals-and-sandbox", "--no-daemon"}},
		"a coder's --approve-for-me":        {group: "coder", agent: `{ command = ["codex", "--approve-for-me"] }`, want: []any{"--approve-for-me", "--no-daemon"}},
		"a coder's -c sandbox_mode":         {group: "coder", agent: `{ command = ["codex", "-c", "sandbox_mode=\"read-only\""] }`, want: []any{"-c", `sandbox_mode="read-only"`, "--no-daemon"}},
		"a coder's --not-so-yolo":           {group: "coder", agent: `{ command = ["codex", "--not-so-yolo"] }`, want: []any{"--not-so-yolo", "--no-daemon"}},
		"a coder's -c default_permissions":  {group: "coder", agent: `{ command = ["codex", "-c", "default_permissions=\":workspace\""] }`, want: []any{"-c", `default_permissions=":workspace"`, "--no-daemon"}},
		"a coder's profile is no choice":    {group: "coder", agent: `{ command = ["codex", "-p", "work"] }`, want: []any{"-p", "work", "--no-daemon", "--sandbox", "danger-full-access"}},
		"a claude coder":                    {group: "coder", agent: `{ command = ["claude"] }`, want: []any{"--name", "ha-coder-1"}},
		"before the model and effort": {
			group: "qa", agent: `{ command = ["codex"] }`, req: workforce.SpawnRequest{Model: "o3", Effort: "high"},
			want: []any{"--no-daemon", "--sandbox", "danger-full-access", "--model", "o3", "-c", `model_reasoning_effort="high"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config := teamConfig
			if tc.top != "" {
				config = strings.Replace(config, "schema_version = 1\n", "schema_version = 1\n[agent]\ncommand = "+tc.top+"\n", 1)
			}
			section := "[projects.herdr-agentisan.groups." + tc.group + "]\n"
			if !strings.Contains(config, section) {
				config += "\n" + section + "workers = 0\nslots = 1\n"
			}
			if tc.agent != "" {
				config = strings.Replace(config, section, section+"agent = "+tc.agent+"\n", 1)
			}
			f := newFixture(t, config)

			req := tc.req
			req.Group, req.Count = tc.group, 1
			_, err := f.svc.Spawn(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, tc.want, f.h.params("agent.start")[0]["args"])
		})
	}
}

package team_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// TestCtxDisplay is the ctx display (ADR-001 A28): "ctx" and the percentage
// right-aligned in four cells, so seven characters with nothing at either end
// for herdr to trim, in the band the value falls in: ok up to 55, warn above
// 55, hot above 65. Anything but a whole percentage from 0 to 100, an absent
// or expired ctx included, is "ctx  --" in the ok band.
func TestCtxDisplay(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ raw, key, value string }{
		"zero":              {raw: "0", key: "ctx_ok", value: "ctx  0%"},
		"one digit":         {raw: "9", key: "ctx_ok", value: "ctx  9%"},
		"the ok ceiling":    {raw: "55", key: "ctx_ok", value: "ctx 55%"},
		"the warn floor":    {raw: "56", key: "ctx_warn", value: "ctx 56%"},
		"the warn ceiling":  {raw: "65", key: "ctx_warn", value: "ctx 65%"},
		"the hot floor":     {raw: "66", key: "ctx_hot", value: "ctx 66%"},
		"full":              {raw: "100", key: "ctx_hot", value: "ctx100%"},
		"absent or expired": {raw: "", key: "ctx_ok", value: "ctx  --"},
		"over 100":          {raw: "101", key: "ctx_ok", value: "ctx  --"},
		"negative":          {raw: "-1", key: "ctx_ok", value: "ctx  --"},
		"a percent sign":    {raw: "56%", key: "ctx_ok", value: "ctx  --"},
		"a fraction":        {raw: "56.5", key: "ctx_ok", value: "ctx  --"},
		"not a number":      {raw: "lots", key: "ctx_ok", value: "ctx  --"},
		"leading zeros":     {raw: "007", key: "ctx_ok", value: "ctx  7%"},
		"overflowing int":   {raw: "99999999999999999999", key: "ctx_ok", value: "ctx  --"},
		"invalid UTF-8":     {raw: "5\xff", key: "ctx_ok", value: "ctx  --"},
		"a truncated rune":  {raw: "56\xd9", key: "ctx_ok", value: "ctx  --"},
		"non-ASCII digits":  {raw: "\u0665\u0666", key: "ctx_ok", value: "ctx  --"},
		"a byte-order mark": {raw: "\ufeff56", key: "ctx_ok", value: "ctx  --"},
		"an embedded NUL":   {raw: "5\x006", key: "ctx_ok", value: "ctx  --"},
		"a trailing CR":     {raw: "56\r", key: "ctx_ok", value: "ctx  --"},
		"inner whitespace":  {raw: "5 6", key: "ctx_ok", value: "ctx  --"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, value := team.CtxDisplay(tc.raw)
			assert.Equal(t, tc.key, key)
			assert.Equal(t, tc.value, value)
			assert.Len(t, value, 7, "every display is the same width")
		})
	}
}

// TestCtxKeysFitHerdr: the band keys are valid herdr token keys (at most 32
// of [A-Za-z0-9_-], src/app/api_helpers.rs:258-265), distinct from each
// other and from the agent's own ctx key, which report writes.
func TestCtxKeysFitHerdr(t *testing.T) {
	t.Parallel()
	key := regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	keys := []string{team.CtxOKKey, team.CtxWarnKey, team.CtxHotKey}
	assert.Equal(t, []string{"ctx_ok", "ctx_warn", "ctx_hot"}, keys)
	for _, k := range keys {
		assert.Regexp(t, key, k)
	}
}

// TestCtxTokens: every pane herdr's agents panel lists, one with an agent
// herdr detected or one agent.list names, gets its ctx display, in a team or
// not; a plain shell gets none. A pane only agent.list names shows "ctx  --",
// whatever ctx a Codex left in it (ADR-001 A27). Each carries all three band
// keys, the two not in use empty, so a pane changing band never shows two.
// Sorted by pane id.
func TestCtxTokens(t *testing.T) {
	t.Parallel()
	ctx := func(id, ws, agent, raw string) herdr.PaneInfo {
		pane := herdr.PaneInfo{PaneID: id, WorkspaceID: ws, Agent: agent, AgentStatus: "idle"}
		if raw != "" {
			pane.Tokens = map[string]string{"ctx": raw}
		}
		return pane
	}
	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}, {WorkspaceID: "w2", Label: "notes"}},
		Panes: []herdr.PaneInfo{
			ctx("wN:p2", "wN", "codex", "70"),
			ctx("wN:p1", "wN", "claude", "40"),
			ctx("wN:p3", "wN", "claude", ""),
			ctx("wN:p4", "wN", "", "61"), // a shell, its ctx left behind
			ctx("wN:p5", "wN", "", "45"), // named by agent.list only, a Codex's ctx left behind
			ctx("w2:p1", "w2", "claude", "58"),
		},
		Agents: []herdr.AgentInfo{{PaneID: "wN:p5", Name: "ha-codér-✓"}, {PaneID: "wN:p4", Name: ""}, {PaneID: "w9:p9", Name: "gone"}},
	}
	band := func(id, key, value string) team.PaneTokens {
		tokens := map[string]string{"ctx_ok": "", "ctx_warn": "", "ctx_hot": ""}
		tokens[key] = value
		return team.PaneTokens{PaneID: id, Tokens: tokens}
	}

	assert.Equal(t, []team.PaneTokens{
		band("w2:p1", "ctx_warn", "ctx 58%"),
		band("wN:p1", "ctx_ok", "ctx 40%"),
		band("wN:p2", "ctx_hot", "ctx 70%"),
		band("wN:p3", "ctx_ok", "ctx  --"),
		band("wN:p5", "ctx_ok", "ctx  --"),
	}, team.CtxTokens(l))
	assert.Empty(t, team.CtxTokens(team.Layout{}), "no panes, no tokens")
}

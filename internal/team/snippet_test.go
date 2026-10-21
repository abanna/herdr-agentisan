package team_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// sidebarToken is one token of a herdr sidebar row: a bare name, or a table
// with a style and rules.
type sidebarToken struct {
	Token string
	Fg    string
	Rules []map[string]any
}

// herdrColor is the colour herdr accepts (src/config/sidebar.rs:65-79).
var herdrColor = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// readSidebarSnippet decodes docs/config/herdr-sidebar.example.toml strictly,
// checking what herdr 0.9.3 checks when it loads the rows: at most 16 rows of
// 16 tokens (src/config/sidebar.rs:11-12), a token table's keys, a colour's
// form, and a rule's keys with exactly one condition, ignore_case on text
// conditions only (src/config/sidebar/rules.rs:26-87).
func readSidebarSnippet(t *testing.T) (agents, spaces [][]sidebarToken) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "herdr-sidebar.example.toml"))
	require.NoError(t, err)
	var cfg struct {
		UI struct {
			Sidebar struct {
				Agents struct {
					Rows [][]any `toml:"rows"`
				} `toml:"agents"`
				Spaces struct {
					Rows [][]any `toml:"rows"`
				} `toml:"spaces"`
			} `toml:"sidebar"`
		} `toml:"ui"`
	}
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&cfg))
	return sidebarRows(t, cfg.UI.Sidebar.Agents.Rows), sidebarRows(t, cfg.UI.Sidebar.Spaces.Rows)
}

func sidebarRows(t *testing.T, rows [][]any) [][]sidebarToken {
	t.Helper()
	require.NotEmpty(t, rows)
	require.LessOrEqual(t, len(rows), 16)
	out := make([][]sidebarToken, 0, len(rows))
	for _, row := range rows {
		require.NotEmpty(t, row)
		require.LessOrEqual(t, len(row), 16)
		var tokens []sidebarToken
		for _, raw := range row {
			tokens = append(tokens, sidebarTokenOf(t, raw))
		}
		out = append(out, tokens)
	}
	return out
}

func sidebarTokenOf(t *testing.T, raw any) sidebarToken {
	t.Helper()
	if name, ok := raw.(string); ok {
		return sidebarToken{Token: name}
	}
	table, ok := raw.(map[string]any)
	require.True(t, ok, "a token is a name or a table, not %T", raw)
	for k := range table {
		require.Contains(t, []string{"token", "fg", "bold", "dim", "rules"}, k, "herdr rejects a token table key %q", k)
	}
	tok := sidebarToken{}
	tok.Token, _ = table["token"].(string)
	require.NotEmpty(t, tok.Token, "a token table names its token")
	if fg, ok := table["fg"]; ok {
		tok.Fg, _ = fg.(string)
		require.Regexp(t, herdrColor, tok.Fg)
	}
	if rules, ok := table["rules"]; ok {
		list, ok := rules.([]any)
		require.True(t, ok, "rules is a list")
		for _, r := range list {
			rule, ok := r.(map[string]any)
			require.True(t, ok, "a rule is a table")
			conditions := 0
			for k, v := range rule {
				require.Contains(t, []string{"equals", "contains", "starts_with", "gt", "lt", "ignore_case", "fg", "bold", "dim", "hide"}, k, "herdr rejects a rule key %q", k)
				if slices.Contains([]string{"equals", "contains", "starts_with", "gt", "lt"}, k) {
					conditions++
				}
				if k == "fg" {
					require.Regexp(t, herdrColor, v)
				}
			}
			require.Equal(t, 1, conditions, "a rule has exactly one condition: %v", rule)
			_, numeric := rule["gt"]
			_, below := rule["lt"]
			_, folds := rule["ignore_case"]
			require.False(t, folds && (numeric || below), "ignore_case applies to text conditions only")
			tok.Rules = append(tok.Rules, rule)
		}
	}
	return tok
}

func names(row []sidebarToken) []string {
	out := make([]string, 0, len(row))
	for _, tok := range row {
		out = append(out, tok.Token)
	}
	return out
}

// colour is the colour herdr gives value: the first rule that matches wins,
// with its fg or the token's own (src/config/sidebar/rules.rs:129-195).
func (tok sidebarToken) colour(t *testing.T, value string) string {
	t.Helper()
	for _, rule := range tok.Rules {
		fold, _ := rule["ignore_case"].(bool)
		text := func(s string) string {
			if fold {
				return strings.ToLower(s)
			}
			return s
		}
		var match bool
		switch {
		case rule["contains"] != nil:
			match = strings.Contains(text(value), text(fmt.Sprint(rule["contains"])))
		case rule["starts_with"] != nil:
			match = strings.HasPrefix(text(value), text(fmt.Sprint(rule["starts_with"])))
		case rule["equals"] != nil:
			match = text(value) == text(fmt.Sprint(rule["equals"]))
		default:
			n, err := strconv.ParseFloat(value, 64)
			gt, isGT := rule["gt"].(float64)
			lt, isLT := rule["lt"].(float64)
			match = err == nil && ((isGT && n > gt) || (isLT && n < lt))
		}
		if match {
			require.NotEqual(t, true, rule["hide"], "a $team rule hides nothing")
			if fg, ok := rule["fg"].(string); ok {
				return fg
			}
			return tok.Fg
		}
	}
	return tok.Fg
}

// TestSidebarSnippet keeps the documented rows in step with what the daemon
// writes (ADR-001 A28 and its addendum):
//   - agents, line 1: the state icon, the three ctx band tokens, each with a
//     colour of its own and no rule (gt and lt cannot read "ctx 56%"), then
//     the name; line 2: the item and the stage, and no workspace, since the
//     agent view shows the focused team;
//   - spaces, line 2: `$team` alone, red when any agent is blocked, else pink
//     when any is ready, else its own colour, which the rules get right for
//     $team's real values, two-digit counts included.
func TestSidebarSnippet(t *testing.T) {
	t.Parallel()
	agents, spaces := readSidebarSnippet(t)

	require.Len(t, agents, 2)
	assert.Equal(t, []string{"state_icon", "$" + team.CtxOKKey, "$" + team.CtxWarnKey, "$" + team.CtxHotKey, "agent"}, names(agents[0]))
	var fgs []string
	for _, tok := range agents[0][1:4] {
		assert.NotEmpty(t, tok.Fg, "%s has its band's colour", tok.Token)
		assert.Empty(t, tok.Rules, "%s needs no rule", tok.Token)
		fgs = append(fgs, tok.Fg)
	}
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(fgs))), 3, "each band has its own colour")
	assert.Equal(t, []string{"$" + report.ItemKey, "$" + report.StageKey}, names(agents[1]))

	require.Len(t, spaces, 2)
	assert.Equal(t, []string{"state_icon", "workspace"}, names(spaces[0]))
	require.Equal(t, []string{"$" + team.Key}, names(spaces[1]))
	tok := spaces[1][0]
	base := tok.colour(t, team.Counts{Panes: 4}.String())
	blocked := tok.colour(t, team.Counts{Panes: 4, Blocked: 1, Ready: 1}.String())
	ready := tok.colour(t, team.Counts{Panes: 4, Ready: 1}.String())
	assert.NotEmpty(t, base)
	assert.NotEqual(t, base, ready, "a ready agent shows")
	assert.NotEqual(t, base, blocked, "a blocked agent shows")
	assert.NotEqual(t, ready, blocked, "blocked and ready look different")
	for _, c := range []team.Counts{{Panes: 1}, {Panes: 12, Working: 10}, {Panes: 20, Working: 20}} {
		assert.Equal(t, base, tok.colour(t, c.String()), "%q", c.String())
	}
	for _, c := range []team.Counts{{Panes: 12, Ready: 10}, {Panes: 3, Working: 3, Ready: 2}, {Panes: 30, Ready: 20}} {
		assert.Equal(t, ready, tok.colour(t, c.String()), "%q", c.String())
	}
	for _, c := range []team.Counts{{Panes: 4, Blocked: 2}, {Panes: 12, Blocked: 10}} {
		assert.Equal(t, blocked, tok.colour(t, c.String()), "%q", c.String())
	}
}

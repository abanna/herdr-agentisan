package dashboard_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// dimSGR is the SGR fragment of the dashboard's dim text.
var dimSGR = regexp.QuoteMeta(truecolor("#6c7086")) + `[0-9;]*m`

// TestModelLabel: a row's model column is its model as ShortModel names it;
// with none reported, the agent herdr detects in its pane, cleaned and cut
// to the column (ADR-001 A68), so a Codex still starting says it is one;
// with neither, nothing. It always fits the column.
func TestModelLabel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ model, kind, want string }{
		"a model beats the agent":         {model: "claude-opus-5-5", kind: "claude", want: "opus"},
		"a codex model":                   {model: "gpt-6.1-sol", kind: "codex", want: "sol6.1"},
		"a model the agent does not run":  {model: "claude-opus-5-5", kind: "codex", want: "opus"},
		"a model and no agent":            {model: "gpt-5.5-codex", want: "cdx5.5"},
		"a codex with no model yet":       {kind: "codex", want: "codex"},
		"a claude with no model yet":      {kind: "claude", want: "claude"},
		"a kind herdr adds":               {kind: "gemini", want: "gemini"},
		"a kind wider than the column":    {kind: "opencode-nightly", want: "openco"},
		"a wide kind":                     {kind: "日本語エージェント", want: "日本語"},
		"a wide kind past an odd cell":    {kind: "a日本語", want: "a日本"},
		"a kind with a control character": {kind: "co\x1bdex", want: "co dex"},
		"a kind with an escape sequence":  {kind: "\x1b[31mcodex", want: "[31mco"},
		"a kind padded with space":        {kind: "  codex\t", want: "codex"},
		"a kind of white space":           {kind: " \t\n"},
		"invalid UTF-8 in a kind":         {kind: "co\xffdex", want: "co\ufffddex"},
		"a blank model is no model":       {model: "   ", kind: "codex", want: "codex"},
		"a decomposed kind":               {kind: "cafe\u0301", want: "cafe\u0301"},
		"a kind like an option":           {kind: "--codex", want: "--code"},
		"a NUL in a kind":                 {kind: "co\x00dex", want: "co dex"},
		"a kind ending in CRLF":           {kind: "codex\r\n", want: "codex"},
		"a kind in another case":          {kind: "Codex", want: "Codex"},
		"a truncated multibyte kind":      {kind: "co\xe2\x82", want: "co\ufffd"},
		"a BOM before a kind":             {kind: "\ufeffcodex", want: "\ufeffcodex"},
		"mixed encodings in a kind":       {kind: "é\xff日", want: "é\ufffd日"},
		"neither":                         {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := dashboard.ModelLabel(snapshot.Agent{Name: "a", Status: snapshot.StatusIdle, Model: tc.model, Kind: tc.kind})
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, ansi.StringWidth(got), dashboard.ModelCells, "the label fits its column")
		})
	}
}

// TestTheModelColumnFallsBackToTheAgent: in the overview, a row with no
// model shows its agent in the model column, dim, and the column is as wide
// as what it draws, so a group where no row has a model still draws it
// (A68). A row with a model draws it as before, not dim, and a row with
// neither leaves the cell blank.
func TestTheModelColumnFallsBackToTheAgent(t *testing.T) {
	t.Parallel()

	snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "coder", Agents: []snapshot.Agent{
		{Name: "ha-coder-1", PaneID: "w1:p1", Status: snapshot.StatusWorking, Kind: "claude", Model: "claude-opus-5-5", Ctx: new(57)},
		{Name: "ha-coder-4", PaneID: "w1:p2", Status: snapshot.StatusWorking, Kind: "codex"},
		{Name: "w1:p3", Unnamed: true, Shell: true, PaneID: "w1:p3", Status: snapshot.StatusUnknown},
	}}}}
	frame := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at}, 97, 20, plain)
	// The column is five cells, codex's: opus is padded to it.
	assert.Regexp(t, `ha-coder-1 +working  opus {3}▓▓▓░░  57%`, lineWith(t, frame, "ha-coder-1"))
	assert.Regexp(t, `ha-coder-4 +working  codex  ░░░░░   --`, lineWith(t, frame, "ha-coder-4"))
	assert.Regexp(t, `w1:p3 +shell {11}░░░░░   --`, lineWith(t, frame, "w1:p3"))

	colour := dashboard.Render(dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Agent: 2}, Now: at}, 97, 20, fullColour)
	assert.Regexp(t, dimSGR+`codex`, colour, "the agent in the model's place is dim")
	assert.NotRegexp(t, dimSGR+`opus`, colour, "a model is not")

	const model = 1
	for kind, want := range map[string]int{"codex": 5, "claude": 6, "opencode-nightly": dashboard.ModelCells, "": 0} {
		groups := []snapshot.Group{{Name: "coder", Agents: []snapshot.Agent{
			{Name: "a", Status: snapshot.StatusIdle, Kind: kind},
			{Name: "b", Status: snapshot.StatusIdle},
		}}}
		_, cols := dashboard.ColumnWidths(groups, at, 97)
		assert.Equal(t, want, cols[model], "a group of model-less %q rows", kind)
	}
}

// TestGroupViewNamesTheAgent: each box's model line names the agent herdr
// detects, dim, ahead of its model: "codex · sol6.1  ctx …"; the agent alone
// before a model is reported; the model alone from a snapshot with no agent,
// as before (A68); and the gauge alone with neither.
func TestGroupViewNamesTheAgent(t *testing.T) {
	t.Parallel()

	snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "coder", Agents: []snapshot.Agent{
		{Name: "both", PaneID: "w1:p1", Status: snapshot.StatusWorking, Kind: "codex", Model: "gpt-6.1-sol", Ctx: new(33)},
		{Name: "kind", PaneID: "w1:p2", Status: snapshot.StatusWorking, Kind: "codex"},
		{Name: "model", PaneID: "w1:p3", Status: snapshot.StatusIdle, Model: "claude-opus-5-5", Ctx: new(57)},
		{Name: "neither", PaneID: "w1:p4", Status: snapshot.StatusIdle},
		{Name: "long", PaneID: "w1:p5", Status: snapshot.StatusIdle, Kind: "opencode-nightly", Model: "o3"},
	}}}}
	frame := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at, Group: "coder"}, 97, 60, plain)
	body := func(name string) string {
		t.Helper()
		ls := lines(frame)
		for i, l := range ls {
			if regexp.MustCompile(`╭─ . ` + name + ` · `).MatchString(l) {
				require.Less(t, i+1, len(ls))
				return ls[i+1]
			}
		}
		require.Failf(t, "no box", "%q in\n%s", name, frame)
		return ""
	}
	assert.Regexp(t, `^│ codex · sol6\.1  ctx ▓▓▓░░░░░░░ 33% +│$`, body("both"))
	assert.Regexp(t, `^│ codex  ctx ░░░░░░░░░░ -- +│$`, body("kind"))
	assert.Regexp(t, `^│ opus  ctx ▓▓▓▓▓▓░░░░ 57% +│$`, body("model"))
	assert.Regexp(t, `^│ ctx ░░░░░░░░░░ -- +│$`, body("neither"))
	assert.Regexp(t, `^│ openco · o3  ctx `, body("long"), "the agent is cut to the column, as in the overview")

	colour := dashboard.Render(dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Agent: 3}, Now: at, Group: "coder"}, 97, 60, fullColour)
	assert.Regexp(t, dimSGR+`codex`, colour, "the agent is dim")
	assert.NotRegexp(t, dimSGR+`sol6\.1`, colour, "the model is not")
}

// TestGroupViewModelLineStyles pins both sides of A68's separator: the dot
// is dim like the agent, and the model retains its default foreground.
func TestGroupViewModelLineStyles(t *testing.T) {
	t.Parallel()

	snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "coder", Agents: []snapshot.Agent{
		{Name: "a", Status: snapshot.StatusWorking, Kind: "codex", Model: "gpt-6.1-sol"},
	}}}}
	frame := dashboard.Render(dashboard.Frame{Snapshot: snap, Group: "coder", Now: at}, 97, 20, fullColour)
	var modelLine string
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(ansi.Strip(line), "codex · sol6.1") {
			modelLine = line
			break
		}
	}
	require.NotEmpty(t, modelLine, "the group view draws the provider, dot and model together")

	for name, tc := range map[string]struct{ text, sgr string }{
		"the dot is dim":     {text: "·", sgr: dimSGR + `[^\x1b]*$`},
		"the model is plain": {text: "sol6.1", sgr: `^\x1b\[(0|39)?m[^\x1b]*$`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			start := strings.Index(modelLine, tc.text)
			require.Positive(t, start)
			prefix := modelLine[:start]
			last := strings.LastIndex(prefix, "\x1b[")
			require.GreaterOrEqual(t, last, 0, "the text has an explicit style transition")
			// Inspect the active SGR at the text, allowing adjacent equal-style
			// segments to be coalesced. A model must follow a default-foreground
			// reset; excluding the dim foreground alone would accept green.
			assert.Regexp(t, tc.sgr, prefix[last:])
		})
	}
}

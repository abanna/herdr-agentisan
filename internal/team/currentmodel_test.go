package team_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// modelAt is when the tests' agents reported their model.
var modelAt = time.Date(2026, 10, 22, 21, 0, 0, 500_000_000, time.UTC)

// reported are a pane's tokens as `report statusline` leaves them.
func reported(model string, at time.Time) map[string]string {
	return map[string]string{report.ModelKey: model, report.ModelAtKey: report.FormatModelAt(at)}
}

// modelPanes builds a ◆ boss space and a ◆ coders space from panes, each
// already placed in wJ or wN, and returns the model's panes by id.
func modelPanes(panes ...herdr.PaneInfo) (team.Model, map[string]team.Pane) {
	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wJ", Label: "◆ boss"}, {WorkspaceID: "wN", Label: "◆ coders"}},
		Panes:      panes,
	}
	m := team.Build(l, team.Spaces{}, nil)
	byID := map[string]team.Pane{}
	for _, pr := range m.Projects {
		for _, w := range pr.Boss {
			byID[w.Pane.ID] = w.Pane
		}
		for _, g := range pr.Groups {
			for _, w := range g.Workers {
				byID[w.Pane.ID] = w.Pane
			}
		}
	}
	return m, byID
}

// TestPaneReadsTheReportedModel: a pane carries its model token and
// model_at only as the report writes them, and only while herdr detects an
// agent in it (A56), as ctx: a Codex's model lives 24 h, past its exit.
func TestPaneReadsTheReportedModel(t *testing.T) {
	t.Parallel()
	_, got := modelPanes(
		pane("wN:p1", "wN", "claude", "working", reported("claude-sonnet-5-5", modelAt)),
		pane("wN:p2", "wN", "", "unknown", reported("gpt-5.5", modelAt)),
		pane("wN:p3", "wN", "codex", "idle", map[string]string{report.ModelKey: "gpt-5.5"}),
		pane("wN:p4", "wN", "codex", "idle", map[string]string{report.ModelKey: "gpt-5.5", report.ModelAtKey: "1761066000.5"}),
		pane("wN:p5", "wN", "claude", "idle", reported("--model", modelAt)),
		pane("wN:p6", "wN", "claude", "idle", map[string]string{report.ModelAtKey: report.FormatModelAt(modelAt)}),
		pane("wJ:p1", "wJ", "claude", "working", reported("claude-opus-5-5[1m]", modelAt)),
	)

	assert.Equal(t, "claude-sonnet-5-5", got["wN:p1"].Model)
	assert.True(t, modelAt.Equal(got["wN:p1"].ModelAt))
	assert.Equal(t, "claude-opus-5-5[1m]", got["wJ:p1"].Model, "the boss's too")
	for id, why := range map[string]string{
		"wN:p2": "no agent in the pane",
		"wN:p3": "no model_at",
		"wN:p4": "a model_at the report never writes",
		"wN:p5": "a model a row may not carry",
		"wN:p6": "model_at alone",
	} {
		assert.Empty(t, got[id].Model, why)
		assert.True(t, got[id].ModelAt.IsZero(), why)
	}
}

// TestCurrentModels is the preference (A56): a reported model wins over the
// launch model while it is not older than the pane's agent process; one
// reported before the agent started is a previous agent's, and gives way to
// the launch model or to none. With no start known, the report wins.
func TestCurrentModels(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)

	tests := map[string]struct {
		tokens  map[string]string
		launch  string
		started bool
		start   time.Time
		want    string
	}{
		"reported after the agent started":   {tokens: reported("claude-sonnet-5-5", modelAt), launch: "opus", started: true, start: start, want: "claude-sonnet-5-5"},
		"reported as the agent started":      {tokens: reported("claude-sonnet-5-5", start), launch: "opus", started: true, start: start, want: "claude-sonnet-5-5"},
		"reported 1 ms before it started":    {tokens: reported("gpt-5", start.Add(-time.Millisecond)), launch: "gpt-5.5", started: true, start: start, want: "gpt-5.5"},
		"a previous agent's, no launch":      {tokens: reported("gpt-5", start.Add(-time.Hour)), started: true, start: start},
		"reported, no start known":           {tokens: reported("claude-haiku-5-5", start.Add(-time.Hour)), launch: "opus", want: "claude-haiku-5-5"},
		"reported, no launch model":          {tokens: reported("gpt-5.5", modelAt), started: true, start: start, want: "gpt-5.5"},
		"nothing reported":                   {launch: "opus", started: true, start: start, want: "opus"},
		"a model with no model_at":           {tokens: map[string]string{report.ModelKey: "claude-haiku-5-5"}, launch: "opus", want: "opus"},
		"neither":                            {},
		"a zero start counts as none known":  {tokens: reported("claude-sonnet-5-5", modelAt), launch: "opus", started: true, want: "claude-sonnet-5-5"},
		"a report far ahead of the start":    {tokens: reported("claude-sonnet-5-5", modelAt.Add(24*time.Hour)), launch: "opus", started: true, start: start, want: "claude-sonnet-5-5"},
		"an invalid model, the launch model": {tokens: reported("op\x1bus", modelAt), launch: "opus", started: true, start: start, want: "opus"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, _ := modelPanes(pane("wN:p1", "wN", "claude", "working", tc.tokens))
			launch := map[string]string{}
			if tc.launch != "" {
				launch["wN:p1"] = tc.launch
			}
			started := map[string]time.Time{}
			if tc.started {
				started["wN:p1"] = tc.start
			}

			got := team.CurrentModels(m, launch, started)
			if tc.want == "" {
				assert.Empty(t, got, "a pane with no model is left out")
				return
			}
			assert.Equal(t, map[string]string{"wN:p1": tc.want}, got)
		})
	}
}

// TestCurrentModelsCoversEveryTeamPane: the boss and every group's workers
// get their model; a pane outside the teams gets none, whatever launch and
// started hold for it, and a shell's leftover token is never its model.
func TestCurrentModelsCoversEveryTeamPane(t *testing.T) {
	t.Parallel()
	start := modelAt.Add(-time.Minute)
	m, _ := modelPanes(
		pane("wJ:p1", "wJ", "claude", "working", reported("claude-opus-5-5", modelAt)),
		pane("wN:p1", "wN", "claude", "idle", nil),
		pane("wN:p2", "wN", "codex", "working", reported("gpt-5.5", modelAt)),
		pane("wN:p3", "wN", "", "unknown", reported("gpt-5", modelAt)),
	)
	launch := map[string]string{"wJ:p1": "opus", "wN:p1": "sonnet", "wN:p2": "gpt-5", "w9:p9": "elsewhere"}
	started := map[string]time.Time{"wJ:p1": start, "wN:p2": start, "w9:p9": start}

	assert.Equal(t, map[string]string{
		"wJ:p1": "claude-opus-5-5",
		"wN:p1": "sonnet",
		"wN:p2": "gpt-5.5",
	}, team.CurrentModels(m, launch, started))
	assert.Empty(t, team.CurrentModels(team.Model{}, launch, started), "no teams, no models")
}

package team_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// placed is where the spaces resolver put one workspace: its group, or
// "(boss)" for the boss space; absent when it is not part of the model.
func placed(m team.Model) map[string]string {
	out := map[string]string{}
	for _, pr := range m.Projects {
		for _, w := range pr.Boss {
			out[w.Pane.ID] = "(boss)"
		}
		for _, g := range pr.Groups {
			for _, t := range g.Targets {
				out[t.WorkspaceID] = g.Name
			}
		}
	}
	return out
}

// TestSpacesResolverLabels: a workspace whose label starts with "◆ " (or v12's
// legacy "agentisan · ") is a group space, its group the rest of the label
// trimmed. "◆ boss" is the boss space, not a group target; "◆ herdr", the
// user's edit shell, is not part of the model at all. Everything else is
// outside the model. The match is exact: case, the space after the prefix
// and the full group name all count.
func TestSpacesResolverLabels(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		label string
		group string // "" when the workspace is not a group target
	}{
		"a group space":                     {label: "◆ coders", group: "coders"},
		"extra spaces are trimmed":          {label: "◆  coders ", group: "coders"},
		"a tab after the prefix is trimmed": {label: "◆ \tqa\t", group: "qa"},
		"the legacy prefix":                 {label: "agentisan · research", group: "research"},
		"a non-ASCII group":                 {label: "◆ código", group: "código"},
		"a group with inner spaces":         {label: "◆ code review", group: "code review"},
		"case is kept":                      {label: "◆ Coders", group: "Coders"},
		"Boss is not boss":                  {label: "◆ Boss", group: "Boss"},
		"a group named like the boss":       {label: "◆ bosses", group: "bosses"},
		"a group named like the shell":      {label: "◆ herdr-dev", group: "herdr-dev"},
		"the boss space":                    {label: "◆ boss"},
		"the boss space, legacy":            {label: "agentisan · boss"},
		"the edit shell":                    {label: "◆ herdr"},
		"the edit shell, padded":            {label: "◆  herdr "},
		"the edit shell, legacy":            {label: "agentisan · herdr"},
		"an empty label":                    {label: ""},
		"the diamond alone":                 {label: "◆"},
		"the diamond and a space":           {label: "◆ "},
		"the diamond and spaces only":       {label: "◆    "},
		"no space after the diamond":        {label: "◆coders"},
		"a leading space":                   {label: " ◆ coders"},
		"another diamond (U+2666)":          {label: "♦ coders"},
		"a white diamond (U+25C7)":          {label: "◇ coders"},
		"an NBSP after the diamond":         {label: "◆ coders"},
		"the legacy prefix without a dot":   {label: "agentisan coders"},
		"the legacy prefix in upper case":   {label: "Agentisan · coders"},
		"a plain workspace":                 {label: "notes"},
		"a diamond later in the label":      {label: "notes ◆ coders"},
		"a label with a newline":            {label: "◆ co\nders", group: "co\nders"},
		"a trailing CRLF is trimmed":        {label: "◆ coders\r\n", group: "coders"},
		"a NUL inside the group":            {label: "◆ co\x00ders", group: "co\x00ders"},
		"a group like a flag":               {label: "◆ --help", group: "--help"},
		"an NFC group":                      {label: "◆ caf\u00e9", group: "caf\u00e9"},
		"an NFD group, kept as given":       {label: "◆ cafe\u0301", group: "cafe\u0301"},
		"a BOM before the diamond":          {label: "\ufeff◆ coders"},
		"a truncated multibyte group":       {label: "◆ c\xe2\x82", group: "c\xe2\x82"},
		"a label with invalid UTF-8":        {label: "◆ c\xffd", group: "c\xffd"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := team.Layout{
				Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "w1", Label: tc.label}},
				Panes:      []herdr.PaneInfo{p("w1:p1", "w1", "working")},
			}
			m := team.Build(l, team.Spaces{Project: "agentisan"}, nil)

			got := placed(m)
			if tc.group != "" {
				assert.Equal(t, map[string]string{"w1": tc.group}, got)
				assert.Len(t, team.Tokens(m), 1)
				return
			}
			assert.Empty(t, team.Tokens(m), "not a group target: no $team")
			if tc.label == "◆ boss" || tc.label == "agentisan · boss" {
				assert.Equal(t, map[string]string{"w1:p1": "(boss)"}, got, "the boss space's panes are the boss's")
				return
			}
			assert.Empty(t, got, "not part of the model")
		})
	}
}

// TestSpacesResolverLayouts: the cases that need more than one workspace or
// more than one poll.
func TestSpacesResolverLayouts(t *testing.T) {
	t.Parallel()

	ws := func(id, label string) herdr.WorkspaceInfo { return herdr.WorkspaceInfo{WorkspaceID: id, Label: label} }
	tests := map[string]struct {
		workspaces []herdr.WorkspaceInfo
		panes      []herdr.PaneInfo
		want       []team.Token
	}{
		// One group, two places: each space counts its own panes.
		"duplicate labels": {
			workspaces: []herdr.WorkspaceInfo{ws("w1", "◆ coders"), ws("w2", "◆ coders")},
			panes:      []herdr.PaneInfo{p("w1:p1", "w1", "working"), p("w2:p1", "w2", "idle"), p("w2:p2", "w2", "idle")},
			want:       []team.Token{{Target: team.Target{WorkspaceID: "w1"}, Value: "1 · ◐1 ●0"}, {Target: team.Target{WorkspaceID: "w2"}, Value: "2 · ◐0 ●2"}},
		},
		"a ◆ space with zero panes": {
			workspaces: []herdr.WorkspaceInfo{ws("w1", "◆ qa")},
			want:       []team.Token{{Target: team.Target{WorkspaceID: "w1"}, Value: "0 · ◐0 ●0"}},
		},
		// herdr never repeats an id; if an answer did, the space is still
		// counted once.
		"a workspace listed twice": {
			workspaces: []herdr.WorkspaceInfo{ws("w1", "◆ qa"), ws("w1", "◆ qa")},
			panes:      []herdr.PaneInfo{p("w1:p1", "w1", "working")},
			want:       []team.Token{{Target: team.Target{WorkspaceID: "w1"}, Value: "1 · ◐1 ●0"}},
		},
		// pane.list and workspace.list are two calls: a pane can name a
		// workspace the other answer does not have yet.
		"a pane in a workspace not listed": {
			workspaces: []herdr.WorkspaceInfo{ws("w1", "◆ qa")},
			panes:      []herdr.PaneInfo{p("w1:p1", "w1", "working"), p("w9:p1", "w9", "working")},
			want:       []team.Token{{Target: team.Target{WorkspaceID: "w1"}, Value: "1 · ◐1 ●0"}},
		},
		"no workspaces": {panes: []herdr.PaneInfo{p("w1:p1", "w1", "working")}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := team.Tokens(team.Build(team.Layout{Workspaces: tc.workspaces, Panes: tc.panes}, team.Spaces{}, nil))
			assert.Equal(t, tc.want, got)
		})
	}
}

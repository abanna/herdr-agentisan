package team_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// TestDescribe: a group gets the description configured under its name,
// matched byte for byte, and only that: a near miss, another group's or none
// leaves it without one, and the rest of the snapshot is untouched.
func TestDescribe(t *testing.T) {
	t.Parallel()

	agents := []snapshot.Agent{{Name: "pee01", PaneID: "wN:p1", Status: snapshot.StatusWorking}}
	groups := func() []snapshot.Group {
		return []snapshot.Group{{Name: "coders", Agents: agents}, {Name: "qa"}, {Name: "codex", Description: "stale"}}
	}
	tests := map[string]struct {
		descriptions map[string]string
		want         []string // each group's description, in order
	}{
		"none":              {want: []string{"", "", ""}},
		"an empty set":      {descriptions: map[string]string{}, want: []string{"", "", ""}},
		"one group's":       {descriptions: map[string]string{"coders": "write fixes"}, want: []string{"write fixes", "", ""}},
		"every group's":     {descriptions: map[string]string{"coders": "a", "qa": "b", "codex": "c"}, want: []string{"a", "b", "c"}},
		"a group not shown": {descriptions: map[string]string{"clerk": "keeps Linear in sync"}, want: []string{"", "", ""}},
		"another case":      {descriptions: map[string]string{"Coders": "x", "QA": "y"}, want: []string{"", "", ""}},
		"singular for plural": {
			descriptions: map[string]string{"coder": "x"}, want: []string{"", "", ""},
		},
		"a trailing space": {descriptions: map[string]string{"qa ": "x"}, want: []string{"", "", ""}},
		"non-ASCII":        {descriptions: map[string]string{"cödex": "x", "codex": "triages Codex · round 5"}, want: []string{"", "", "triages Codex · round 5"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := snapshot.Snapshot{V: snapshot.Version, Team: "agentisan", Groups: groups()}
			team.Describe(&s, tc.descriptions)
			got := make([]string, 0, len(s.Groups))
			for _, g := range s.Groups {
				got = append(got, g.Description)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, agents, s.Groups[0].Agents, "the rows are untouched")
			assert.Equal(t, "agentisan", s.Team)
		})
	}

	empty := snapshot.Snapshot{V: snapshot.Version, Team: "t", Groups: []snapshot.Group{}}
	team.Describe(&empty, map[string]string{"coders": "x"})
	assert.Empty(t, empty.Groups, "no group, nothing to describe")
}

package dashboard_test

import (
	"regexp"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// TestPRMarks: each PR shows one mark, herdr-status.sh's (ADR-001 A29), in
// its colour: ✗ a check failed, ⏳ checks pending, ✓ every check passed with
// a 👍 and a clean merge, … passed but waiting for the 👍 or a clean merge,
// 💬 Codex's findings, ? a state the dashboard cannot read. CI decides
// first, so no 👍 hides a failure. Every value the contract allows is
// covered, and one it does not is never drawn as a pass.
func TestPRMarks(t *testing.T) {
	t.Parallel()
	const (
		green, red, yellow, orange, dim = "#50fa7b", "#ff5555", "#f1fa8c", "#ffb86c", "#6c7086"
	)
	tests := map[string]struct {
		pr         snapshot.PR
		mark, want string
	}{
		"failed, even with a 👍 and clean": {pr: pr(snapshot.CIFailure, snapshot.CodexApproved, snapshot.MergeClean), mark: "✗", want: red},
		"failed with findings":            {pr: pr(snapshot.CIFailure, snapshot.CodexFindings, snapshot.MergeDirty), mark: "✗", want: red},
		"pending, even with a 👍":          {pr: pr(snapshot.CIPending, snapshot.CodexApproved, snapshot.MergeClean), mark: "⏳", want: yellow},
		"pending, nothing else yet":       {pr: pr(snapshot.CIPending, snapshot.CodexPending, snapshot.MergeBlocked), mark: "⏳", want: yellow},
		"CI unknown":                      {pr: pr(snapshot.CIUnknown, snapshot.CodexApproved, snapshot.MergeClean), mark: "?", want: dim},
		"CI not in the contract":          {pr: pr("passed", snapshot.CodexApproved, snapshot.MergeClean), mark: "?", want: dim},
		"ready: passed, a 👍, clean":       {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeClean), mark: "✓", want: green},
		"a 👍 but behind":                  {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeBehind), mark: "…", want: dim},
		"a 👍 but dirty":                   {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeDirty), mark: "…", want: dim},
		"a 👍 but blocked":                 {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeBlocked), mark: "…", want: dim},
		"a 👍 on a draft":                  {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeDraft), mark: "…", want: dim},
		"a 👍, merge unstable":             {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeUnstable), mark: "…", want: dim},
		"a 👍, merge has hooks":            {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeHasHooks), mark: "…", want: dim},
		"a 👍, merge unknown":              {pr: pr(snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeUnknown), mark: "…", want: dim},
		"clean, waiting for the 👍":        {pr: pr(snapshot.CISuccess, snapshot.CodexPending, snapshot.MergeClean), mark: "…", want: dim},
		"findings to answer":              {pr: pr(snapshot.CISuccess, snapshot.CodexFindings, snapshot.MergeClean), mark: "💬", want: orange},
		"findings, blocked":               {pr: pr(snapshot.CISuccess, snapshot.CodexFindings, snapshot.MergeBlocked), mark: "💬", want: orange},
		"Codex unknown":                   {pr: pr(snapshot.CISuccess, snapshot.CodexUnknown, snapshot.MergeClean), mark: "?", want: dim},
		"Codex not in the contract":       {pr: pr(snapshot.CISuccess, "liked", snapshot.MergeClean), mark: "?", want: dim},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snap := &snapshot.Snapshot{V: 1, Team: "t", Project: &snapshot.Project{PRs: []snapshot.PR{tc.pr}}}
			f := dashboard.Frame{Snapshot: snap, Now: at}
			assert.Contains(t, dashboard.Render(f, 200, 30, plain), "│ t · PRs #7 "+tc.mark+" "+string(tc.pr.Merge)+" ", "in full")
			// 16 cells inside the box: too few for the merge state.
			assert.Contains(t, dashboard.Render(f, 20, 30, plain), "│ t · PRs #7"+tc.mark+" ", "compact")
			coloured := dashboard.Render(f, 200, 30, dashboard.Options{Profile: colorprofile.TrueColor})
			assert.Regexp(t, regexp.QuoteMeta(truecolor(tc.want))+`[0-9;]*m`+regexp.QuoteMeta(tc.mark), coloured)
		})
	}
}

func pr(ci snapshot.CIState, codex snapshot.CodexState, merge snapshot.MergeState) snapshot.PR {
	return snapshot.PR{Number: 7, CI: ci, Codex: codex, Merge: merge}
}

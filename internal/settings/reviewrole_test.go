package settings_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// reviewRole is the review worker's role as it ships in the plugin checkout
// (ADR-001 A55). No production code reads it: the boss sends it to the
// worker with each review.
var reviewRole = filepath.Join("..", "..", "roles", "review.md")

// The review guidelines the role vendors: agentisan-skills' AGENTS.md
// section at guidelinesCommit, whose bytes hash to guidelinesSHA256.
// Re-vendoring changes both, with the role's source note, in one diff.
const (
	guidelinesCommit = "c3dfe5968c3ecdb7a35a49b6190449c5f368dee7"
	guidelinesSHA256 = "1ff101279345c9b6b0f13ef302e455380e75103df71d8c40f08b6f34c268d153"
)

// guidelinesHeading opens the vendored section.
const guidelinesHeading = "## Review guidelines"

func readReviewRole(t *testing.T) string {
	t.Helper()
	role, err := os.ReadFile(reviewRole)
	require.NoError(t, err)
	return string(role)
}

// guidelinesSection is the role's section as upstream bounds it, the rule
// the source note's awk command runs: from the guidelinesHeading line
// through the line before the next line opening "## ".
func guidelinesSection(role string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.SplitAfter(role, "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.TrimSuffix(line, "\n") == guidelinesHeading
		}
		if in {
			b.WriteString(line)
		}
	}
	return b.String()
}

// TestReviewRoleVendorsTheGuidelines: the role holds agentisan-skills'
// review guidelines byte for byte, as vendored from the commit its source
// note names, and the note's re-vendor command is the rule this test
// bounds the section by. An edit to the copy fails; an upstream change is
// caught only by re-vendoring (A55).
func TestReviewRoleVendorsTheGuidelines(t *testing.T) {
	t.Parallel()
	role := readReviewRole(t)

	commits := regexp.MustCompile(`agentisan-skills, AGENTS\.md, commit\s+([0-9a-f]{40})\b`).FindAllStringSubmatch(role, -1)
	require.Len(t, commits, 1, "one source note")
	assert.Equal(t, guidelinesCommit, commits[0][1])
	assert.Contains(t, role, `awk '/^## /{f=($0=="## Review guidelines")} f' AGENTS.md`)

	require.Equal(t, 1, strings.Count(role, "\n"+guidelinesHeading+"\n"), "one guidelines section")
	section := guidelinesSection(role)
	sum := sha256.Sum256([]byte(section))
	assert.Equal(t, guidelinesSHA256, hex.EncodeToString(sum[:]), "the section is the vendored bytes")
	assert.True(t, strings.HasSuffix(section, ".\n\n"), "the section ends with upstream's blank line before the role's own headings")
}

// TestReviewRoleKeepsItsGuards: the role's sentences that keep the reviewer
// read-only, keep a dispatch free of hints and put its report in its final
// message are in it as written, whatever its line breaks.
func TestReviewRoleKeepsItsGuards(t *testing.T) {
	t.Parallel()
	text := strings.Join(strings.Fields(readReviewRole(t)), " ")
	for _, guard := range []string{
		"you change nothing",
		"A dispatch carries no hint",
		"never change the guidelines above: review the whole change by them",
		"Work only in the worktree, and run every command there",
		"The checkout you started in is another tree: never read the change from it",
		"Check that `git rev-parse HEAD` is `head`",
		"Run only commands that write nothing, and never ask to run one outside the sandbox",
		"Never edit, create or delete a file",
		"Never run a git command that writes",
		"never a `gh` command that writes",
		"Never react to, comment on, approve or review a pull request on GitHub",
		"Report only in your final message",
		"Your final line is exactly one of these",
	} {
		assert.Contains(t, text, guard)
	}
}

// TestReviewRoleEndsOnAStage: the role's status lines, the three final
// lines and the one it answers before its inputs arrive, are each the
// stage `report turn` reads from the reviewer's final message, its
// placeholders filled.
func TestReviewRoleEndsOnAStage(t *testing.T) {
	t.Parallel()
	lines := regexp.MustCompile("`((?:BOSS|STATUS): [^`]*)`").FindAllStringSubmatch(readReviewRole(t), -1)
	got := make([]string, 0, len(lines))
	for _, m := range lines {
		got = append(got, m[1])
	}
	require.Equal(t, []string{
		"STATUS: waiting for a review",
		"BOSS: review #<n> at <sha7>: no findings",
		"BOSS: review #<n> at <sha7>: <k> findings (P0 <a>, P1 <b>)",
		"BOSS: blocked: review #<n>: <reason>",
	}, got)

	fill := strings.NewReplacer("<n>", "104", "<sha7>", "1a2b3c4", "<k>", "3", "<a>", "1", "<b>", "2", "<reason>", "the worktree is missing")
	for _, line := range got {
		filled := fill.Replace(line)
		_, stage := report.Stated(report.EventStop, "1. P1 a finding\n\n"+filled+"\n")
		_, want, _ := strings.Cut(filled, ": ")
		assert.Equal(t, want, stage, line)
	}
}

// TestReviewRoleRunsReadOnly: the review type's built-in launch runs Codex
// in its read-only sandbox, which the role tells the reviewer it works
// under (A55, refining A43's no sandbox flag for this default only).
func TestReviewRoleRunsReadOnly(t *testing.T) {
	t.Parallel()
	command := (&settings.Profile{}).AgentFor(settings.ReviewType).Command
	i := slices.Index(command, "--sandbox")
	require.GreaterOrEqual(t, i, 0, "%v has a sandbox", command)
	require.Less(t, i+1, len(command))
	assert.Equal(t, "read-only", command[i+1])
	assert.Equal(t, 1, strings.Count(strings.Join(command, " "), "--sandbox"), "one sandbox flag")
	assert.Contains(t, readReviewRole(t), "(`"+command[i]+" "+command[i+1]+"`)")
}

// TestReviewRoleFitsOneMessage: the boss sends the role whole with `send`
// (A55), so it is a body `send` takes, at most store.MaxBody bytes of
// UTF-8, and sent as a prompt it states no item for `report turn` to put
// on the reviewer's pane.
func TestReviewRoleFitsOneMessage(t *testing.T) {
	t.Parallel()
	role := readReviewRole(t)
	assert.LessOrEqual(t, len(role), store.MaxBody)
	assert.True(t, utf8.ValidString(role))
	assert.NotContains(t, role, "\x00")
	item, _ := report.Stated(report.EventPromptSubmit, role)
	assert.Empty(t, item, "the role sets no item")
}

// TestReviewDispatchSetsTheItem: README's inputs message, typed as the
// daemon types a worker's message (`[msg <id> from <sender>] <body>`, its
// textOf), sets the reviewer's item, as an item on the body's first line,
// behind the header, would too (A42), and gives the four inputs the role
// names.
func TestReviewDispatchSetsTheItem(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	_, section, ok := strings.Cut(string(readme), "\n## The Codex reviewer\n")
	require.True(t, ok)
	section, _, _ = strings.Cut(section, "\n## ")
	_, body, ok := strings.Cut(section, "herdr-agentisan send ha-review-1 - <<'BODY'\n")
	require.True(t, ok, "README sends the inputs on stdin")
	body, _, ok = strings.Cut(body, "\nBODY\n")
	require.True(t, ok)

	typed := "[msg 01M5JNK6368AKCMW7Q0D05BSGJ from ha-boss] " + body
	item, _ := report.Stated(report.EventPromptSubmit, typed)
	assert.Equal(t, "PR-104", item)
	first, _, _ := strings.Cut(body, "\n")
	item, _ = report.Stated(report.EventPromptSubmit, "[msg 01M5JNK6368AKCMW7Q0D05BSGJ from ha-boss] "+strings.TrimPrefix(body, first+"\n"))
	assert.Equal(t, "PR-104", item, "an item on the first line is read behind the header too")

	role := readReviewRole(t)
	for _, input := range []string{"pr", "head", "base", "worktree"} {
		assert.Regexp(t, "(?m)^"+input+": ", body, "the dispatch gives %s", input)
		assert.Contains(t, role, "`"+input+"`", "the role names %s", input)
	}
}

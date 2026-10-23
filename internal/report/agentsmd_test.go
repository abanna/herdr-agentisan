package report_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// reportingHeading opens AGENTS.md's section that tells every agent, Claude
// or Codex, how to state its item and stage.
const reportingHeading = "## Reporting (in a herdr team)"

// TestAgentsReportingSection: AGENTS.md's Reporting section, the rule every
// agent in this repo reads (Codex directly, Claude through CLAUDE.md's
// include), names the prefixes `report turn` reads and says the status line
// is the very last one. Its example is an item line, then a status line,
// last; at the end of a final message, report turn reads exactly the two
// values those lines state. So a parser change that stops reading the
// documented form fails here (NERD-5502).
func TestAgentsReportingSection(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "AGENTS.md"))
	require.NoError(t, err)
	section := sectionOf(t, string(raw), reportingHeading)
	for _, p := range append([]string{report.ItemPrefix}, report.StatusPrefixes...) {
		assert.Contains(t, section, "`"+p+"`", "the section names %s", p)
	}
	assert.Contains(t, section, "very last line", "F6: the status line ends the message")

	example := fencedBlock(t, section)
	lines := strings.Split(example, "\n")
	require.Len(t, lines, 2, "an item line, then the status line")
	wantItem, ok := strings.CutPrefix(lines[0], report.ItemPrefix+" ")
	require.True(t, ok, "the example's item line opens with %s: %q", report.ItemPrefix, lines[0])
	var wantStage string
	for _, p := range report.StatusPrefixes {
		if v, found := strings.CutPrefix(lines[1], p+" "); found {
			wantStage = v
		}
	}
	require.NotEmpty(t, wantStage, "the example's last line is a status line: %q", lines[1])

	item, stage := report.Stated(report.EventStop, "The fix is pushed and QA has it.\n\n"+example)
	assert.Equal(t, wantItem, item)
	assert.Equal(t, wantStage, stage)
}

// sectionOf is doc's section under heading, up to the next "## " heading;
// the heading must open exactly one line.
func sectionOf(t *testing.T, doc, heading string) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if l == heading {
			require.Equal(t, -1, start, "one %q heading", heading)
			start = i + 1
		}
	}
	require.NotEqual(t, -1, start, "AGENTS.md has a %q heading", heading)
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// fencedBlock is the one fenced code block in section, without its fences.
func fencedBlock(t *testing.T, section string) string {
	t.Helper()
	var blocks [][]string
	var open []string
	inside := false
	for l := range strings.SplitSeq(section, "\n") {
		switch {
		case strings.HasPrefix(l, "```") && !inside:
			inside, open = true, nil
		case strings.HasPrefix(l, "```"):
			inside = false
			blocks = append(blocks, open)
		case inside:
			open = append(open, l)
		}
	}
	require.False(t, inside, "every fence is closed")
	require.Len(t, blocks, 1, "one example")
	return strings.Join(blocks[0], "\n")
}

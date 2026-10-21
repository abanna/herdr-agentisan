package dashboard_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// TestStageColour: a stage takes v12's colours (ADR-001 A38), case folded
// and by whole words, tried in v12's order: a word "ready" is bold green;
// else "red" or "blocked" is bold red; else "waiting" or "queued" is
// yellow; anything else stays dim. A word is a run of letters, marks and
// digits. It holds in an agent's row and on the boss line, under TrueColor.
func TestStageColour(t *testing.T) {
	t.Parallel()

	const (
		green  = "#50fa7b"
		red    = "#ff5555"
		yellow = "#f1fa8c"
		dim    = "#6c7086"
	)
	tests := map[string]struct {
		stage string
		// drawn is the stage as the frame shows it, when it differs.
		drawn string
		hex   string
		bold  bool
	}{
		"READY":          {stage: "READY", hex: green, bold: true},
		"ready_to_merge": {stage: "ready_to_merge", hex: green, bold: true},
		"ready-to-merge": {stage: "ready-to-merge", hex: green, bold: true},
		"already":        {stage: "already", hex: dim},
		"unready":        {stage: "unready", hex: dim},
		"ready2":         {stage: "ready2", hex: dim},
		"tests RED":      {stage: "tests RED", hex: red, bold: true},
		// v12's `"red" in stage` drew these red.
		"restored":                 {stage: "restored", hex: dim},
		"unblocked":                {stage: "unblocked", hex: dim},
		"Blocked: 1Password agent": {stage: "Blocked: 1Password agent", hex: red, bold: true},
		"waiting: boss":            {stage: "waiting: boss", hex: yellow},
		"WAITING":                  {stage: "WAITING", hex: yellow},
		"J1 re-queued":             {stage: "J1 re-queued", hex: yellow},
		"queue empty":              {stage: "queue empty", hex: dim},
		"review":                   {stage: "review", hex: dim},
		// v12's order: ready wins over blocked, and red over waiting.
		"blocked until ready": {stage: "blocked until ready", hex: green, bold: true},
		"waiting, red":        {stage: "waiting, red", hex: red, bold: true},
		// A control character is drawn as a space, and separates words.
		"a bell after ready": {stage: "ready\x07", drawn: "ready", hex: green, bold: true},
		"CRLF after READY":   {stage: "READY\r\n", drawn: "READY", hex: green, bold: true},
		"a NUL splits ready": {stage: "re\x00ady", drawn: "re ady", hex: dim},
		// A byte-order mark is no letter, so it separates words too.
		"a BOM before READY": {stage: "\ufeffREADY", hex: green, bold: true},
		// A combining mark is part of its word: "readý" is not "ready".
		"a combining mark joins the word": {stage: "readý", hex: dim},
		"another script":                  {stage: "готово", hex: dim},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := snapshot.Agent{Name: "pee01", Status: snapshot.StatusWorking, Model: "opus", Ctx: new(10), Stage: tc.stage}
			boss := snapshot.Boss{Agent: a}
			boss.Name = "boss"
			snap := &snapshot.Snapshot{V: 1, Team: "t", Boss: &boss, Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{a}}}}
			// Select nothing: reverse video would change the row's SGR.
			f := dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at}
			out := dashboard.Render(f, 80, 24, dashboard.Options{Profile: colorprofile.TrueColor})

			drawn := tc.stage
			if tc.drawn != "" {
				drawn = tc.drawn
			}
			found := regexp.MustCompile(`\x1b\[([0-9;:]*)m`+regexp.QuoteMeta(drawn)).FindAllStringSubmatch(out, -1)
			require.Len(t, found, 2, "the row and the boss line each draw the stage")
			for _, m := range found {
				assert.Contains(t, m[1], truecolor(tc.hex), "colour")
				assert.Equal(t, tc.bold, hasParam(m[1], "1"), "bold")
			}
		})
	}
}

// TestStageColourWhenCut: a stage too long for its column is coloured by
// what the whole of it says, so a cut stage keeps its colour.
func TestStageColourWhenCut(t *testing.T) {
	t.Parallel()

	stage := strings.Repeat("x", 80) + " READY"
	snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "g", Agents: []snapshot.Agent{
		{Name: "pee01", Status: snapshot.StatusWorking, Stage: stage},
	}}}}
	out := dashboard.Render(dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at}, 80, 24, dashboard.Options{Profile: colorprofile.TrueColor})
	assert.NotContains(t, out, "READY", "the word itself is cut off")
	assert.Regexp(t, `\x1b\[[0-9;]*`+regexp.QuoteMeta(truecolor("#50fa7b"))+`[0-9;]*mxxx`, out)
}

// TestDataAge: once the snapshot drawn is more than 15 whole seconds older
// than the clock, the footer says "data Ns old", in seconds, as v12 does
// (ADR-001 A38). It comes after a search query and before everything else,
// so a long error cannot push it off the line.
func TestDataAge(t *testing.T) {
	t.Parallel()

	// hints are the footer's keys with an agent to act on, bare its keys
	// with none, as a frame with no snapshot has.
	const (
		hints = "enter jump · ↑↓ move · ? help · q quit"
		bare  = "? help · q quit"
	)
	// aged has one agent, so its frames' hints are the full ones.
	aged := func(d time.Duration) *snapshot.Snapshot {
		return &snapshot.Snapshot{V: 1, At: at.Add(-d), Team: "t", Groups: []snapshot.Group{{Name: "g", Agents: []snapshot.Agent{
			{Name: "a", PaneID: "w1:p1", Status: snapshot.StatusIdle},
		}}}}
	}
	tests := map[string]struct {
		frame dashboard.Frame
		want  string
	}{
		"fresh":                         {frame: dashboard.Frame{Snapshot: aged(0), Now: at}, want: " " + hints},
		"15 s old is not yet stale":     {frame: dashboard.Frame{Snapshot: aged(15 * time.Second), Now: at}, want: " " + hints},
		"15.9 s counts as 15":           {frame: dashboard.Frame{Snapshot: aged(15*time.Second + 900*time.Millisecond), Now: at}, want: " " + hints},
		"16 s old":                      {frame: dashboard.Frame{Snapshot: aged(16 * time.Second), Now: at}, want: " data 16s old · " + hints},
		"an hour old is still seconds":  {frame: dashboard.Frame{Snapshot: aged(time.Hour), Now: at}, want: " data 3600s old · " + hints},
		"a snapshot from the future":    {frame: dashboard.Frame{Snapshot: aged(-time.Hour), Now: at}, want: " " + hints},
		"no snapshot":                   {frame: dashboard.Frame{Now: at}, want: " " + bare},
		"no team here":                  {frame: dashboard.Frame{Now: at, NoTeam: dashboard.ErrNotATeamsWorkspace}, want: " " + bare},
		"a snapshot that reports no at": {frame: dashboard.Frame{Snapshot: &snapshot.Snapshot{V: 1, Team: "t"}, Now: at}, want: " " + bare},
		"before the errors and the note": {
			frame: dashboard.Frame{Snapshot: aged(20 * time.Second), Now: at, Note: "focus off", Errors: []error{errors.New("e1")}},
			want:  " data 20s old · ✖ e1 · focus off · " + hints,
		},
		"after the search query": {
			frame: dashboard.Frame{Snapshot: aged(20 * time.Second), Now: at, Search: true, Query: "x"},
			want:  " /x▏ · data 20s old · enter jump · ↑↓ move · esc clear",
		},
		"a long error cuts the hints, not the age": {
			frame: dashboard.Frame{Snapshot: aged(20 * time.Second), Now: at, Errors: []error{errors.New(strings.Repeat("x", 200))}},
			want:  " data 20s old · ✖ " + strings.Repeat("x", 60) + "…",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ls := lines(dashboard.Render(tc.frame, 80, 24, plain))
			assert.Equal(t, tc.want, ls[len(ls)-1])
		})
	}

	t.Run("in the note's yellow", func(t *testing.T) {
		t.Parallel()
		out := dashboard.Render(dashboard.Frame{Snapshot: aged(16 * time.Second), Now: at}, 80, 24, dashboard.Options{Profile: colorprofile.TrueColor})
		assert.Regexp(t, regexp.QuoteMeta(truecolor("#f1fa8c"))+`[0-9;]*mdata 16s old`, out)
	})
	t.Run("while searching a stale snapshot", func(t *testing.T) {
		t.Parallel()
		// Filter keeps the snapshot's time, so the age stays right.
		f := dashboard.Frame{Snapshot: dashboard.Filter(fixture(t, "full"), "pee"), Now: at.Add(time.Minute), Search: true, Query: "pee"}
		ls := lines(dashboard.Render(f, 120, 40, plain))
		assert.Contains(t, ls[len(ls)-1], " · data 60s old · ")
	})
}

// TestStageColourInTheGroupView: a group's view (ADR-001 A37) draws a box's
// stage in the colour the whole stage takes, as a row does (A38), on both of
// its wrapped lines; a stage not reported stays dim.
func TestStageColourInTheGroupView(t *testing.T) {
	t.Parallel()

	long := "READY for review: " + strings.Repeat("word ", 20)
	snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{
		{Name: "pee01", Status: snapshot.StatusWorking, Stage: long},
		{Name: "pee02", Status: snapshot.StatusBlocked, Stage: "blocked: 1Password agent"},
		{Name: "pee03", Status: snapshot.StatusIdle, Stage: "restored"},
	}}}}
	f := dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at, Group: "coders"}
	out := dashboard.Render(f, 80, 40, dashboard.Options{Profile: colorprofile.TrueColor})

	styled := func(text string) *regexp.Regexp {
		return regexp.MustCompile(`\x1b\[([0-9;:]*)m` + regexp.QuoteMeta(text))
	}
	for _, tc := range []struct {
		hex, text string
		bold      bool
	}{
		{"#50fa7b", "READY for review: word", true},
		{"#50fa7b", "word word", true}, // the second line keeps the colour
		{"#ff5555", "blocked: 1Password agent", true},
		{"#6c7086", "restored", false},
	} {
		found := styled(tc.text).FindAllStringSubmatch(out, -1)
		require.NotEmpty(t, found, "%q is drawn", tc.text)
		for _, m := range found {
			assert.Contains(t, m[1], truecolor(tc.hex), "%q's colour", tc.text)
			assert.Equal(t, tc.bold, hasParam(m[1], "1"), "%q's weight", tc.text)
		}
	}
}

// TestShellGlyphColour: a plain shell's ▸ is drawn in unknown's style, which
// is no colour at all, in its row, in its group's title count and in its
// box in the group's view (ADR-001 A38), so it never reads as a status
// colour. A mutant that draws the count plain (QA's SD10) is equivalent:
// unknown's style is plain.
func TestShellGlyphColour(t *testing.T) {
	t.Parallel()

	snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{
		{Name: "notes", Unnamed: true, Shell: true, Status: snapshot.StatusUnknown},
		{Name: "pee01", Status: snapshot.StatusWorking},
	}}}}
	tc := dashboard.Options{Profile: colorprofile.TrueColor}
	coloured := regexp.MustCompile(`\x1b\[[0-9;:]*38;[0-9;:]*m▸`)
	for name, f := range map[string]dashboard.Frame{
		"the overview":     {Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at},
		"the group's view": {Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at, Group: "coders"},
	} {
		out := dashboard.Render(f, 80, 24, tc)
		assert.Contains(t, ansi.Strip(out), "▸1", "%s: the title counts the shell", name)
		assert.Contains(t, ansi.Strip(out), "▸ notes", "%s: the shell's own glyph", name)
		assert.NotRegexp(t, coloured, out, "%s: no ▸ carries a colour", name)
		// The working agent's glyph is coloured in the same frame, so a
		// colour, had ▸ one, would show.
		assert.Regexp(t, regexp.MustCompile(regexp.QuoteMeta(truecolor("#50fa7b"))+`[0-9;]*m◐`), out, name)
	}
}

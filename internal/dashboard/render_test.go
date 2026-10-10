package dashboard_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// at is the frame clock in every test. It is UTC and formatted as given, so a
// golden does not depend on the machine's timezone.
var at = time.Date(2026, 10, 9, 17, 19, 0, 0, time.UTC)

// plain renders text only: NoTTY strips colour AND attributes, so a golden
// holds exactly what a reader sees, and nothing a terminal interprets.
var plain = dashboard.Options{Profile: colorprofile.NoTTY}

// attrs keeps attributes such as reverse video and drops colour, so the
// selected line can be found.
var attrs = dashboard.Options{Profile: colorprofile.ASCII}

// fixtures are every snapshot in testdata.
var fixtures = []string{"full", "empty", "blocked", "crowd", "wide"}

// fixture loads testdata/<name>.json through the same source the CLI uses.
func fixture(t *testing.T, name string) *snapshot.Snapshot {
	t.Helper()
	s, err := snapshot.FileSource{Path: filepath.Join("testdata", name+".json")}.Snapshot(t.Context())
	require.NoError(t, err)
	return &s
}

// groupsOf returns the groups in the dashboard's display order.
func groupsOf(s *snapshot.Snapshot) []snapshot.Group {
	return dashboard.Order(s.Groups)
}

// selections is every agent of s, in display order.
func selections(s *snapshot.Snapshot) []dashboard.Selection {
	var out []dashboard.Selection
	for g, grp := range groupsOf(s) {
		for a := range grp.Agents {
			out = append(out, dashboard.Selection{Group: g, Agent: a})
		}
	}
	return out
}

// TestRenderGolden pins whole frames. Regenerate with
// `go test ./internal/dashboard/ -update` (this package only: -update is a
// flag of the golden package, unknown to every other test binary), then read
// every changed golden: -update makes anything pass.
func TestRenderGolden(t *testing.T) {
	t.Parallel()

	frame := func(name string, sel dashboard.Selection, btop bool) func(t *testing.T) dashboard.Frame {
		return func(t *testing.T) dashboard.Frame {
			t.Helper()
			return dashboard.Frame{Snapshot: fixture(t, name), Selection: sel, Now: at, Btop: btop}
		}
	}
	first := dashboard.Selection{}
	tests := map[string]struct {
		frame func(t *testing.T) dashboard.Frame
		w, h  int
	}{
		"full_80x24":  {frame: frame("full", first, true), w: 80, h: 24},
		"full_120x40": {frame: frame("full", first, true), w: 120, h: 40},
		"full_200x60": {frame: frame("full", first, true), w: 200, h: 60},
		// Narrow: time, stage and item have dropped, right to left.
		"full_50x30": {frame: frame("full", first, true), w: 50, h: 30},
		// Short: the header loses its box and the gap.
		"full_80x8": {frame: frame("full", first, true), w: 80, h: 8},
		// Overflow: the selection is low in the list, so the groups scroll.
		"full_selected_clerk_80x24":  {frame: frame("full", dashboard.Selection{Group: 5}, true), w: 80, h: 24},
		"crowd_selected_pee20_80x24": {frame: frame("crowd", dashboard.Selection{Agent: 19}, true), w: 80, h: 24},
		"crowd_120x40":               {frame: frame("crowd", first, true), w: 120, h: 40},
		"blocked_80x24":              {frame: frame("blocked", first, true), w: 80, h: 24},
		"blocked_120x40":             {frame: frame("blocked", first, true), w: 120, h: 40},
		"empty_80x24":                {frame: frame("empty", first, true), w: 80, h: 24},
		"wide_120x40":                {frame: frame("wide", first, true), w: 120, h: 40},
		"waiting_80x24": {
			frame: func(*testing.T) dashboard.Frame { return dashboard.Frame{Now: at, Btop: true} }, w: 80, h: 24,
		},
		"help_80x24": {
			frame: func(t *testing.T) dashboard.Frame {
				t.Helper()
				return dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Help: true, Btop: true}
			},
			w: 80, h: 24,
		},
		// No socket: no btop button, no btop hint, and the footer says why.
		"footer_notes_80x24": {
			frame: func(t *testing.T) dashboard.Frame {
				t.Helper()
				return dashboard.Frame{
					Snapshot: fixture(t, "full"), Now: at,
					Note:   "focus off: HERDR_SOCKET_PATH is not set",
					Errors: []error{errors.New("snapshot: snapshot source is unavailable")},
				}
			},
			w: 80, h: 24,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The trailing newline matches what end-of-file tooling expects
			// of a text file, so a golden never differs by its last byte.
			golden.RequireEqual(t, dashboard.Render(tc.frame(t), tc.w, tc.h, plain)+"\n")
		})
	}
}

// widths and heights are swept in full: every frame of every fixture, at
// each pair, with and without help and the btop button, the selection first
// and last.
var (
	widths  = []int{1, 2, 3, 4, 5, 8, 12, 16, 20, 26, 30, 34, 40, 46, 50, 60, 80, 100, 120, 200}
	heights = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 12, 16, 24, 40, 60}
	// huge are a terminal's 16-bit limits, one side at a time.
	huge = [][2]int{{65535, 30}, {80, 65535}}
)

// checkFrame fails unless frame is exactly h lines, none wider than w and
// none ending in a space, so bubbletea never wraps or scrolls it.
func checkFrame(t *testing.T, frame string, w, h int, label string) bool {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) != h {
		t.Errorf("%s: %d lines, want %d", label, len(lines), h)
		return false
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Errorf("%s: line %d is %d wide: %q", label, i, ansi.StringWidth(l), l)
			return false
		}
		if strings.TrimRight(l, " ") != l {
			t.Errorf("%s: line %d has trailing spaces: %q", label, i, l)
			return false
		}
	}
	return true
}

// TestRenderFillsTheFrameExactly: whatever the size, a frame is exactly h
// lines and no line is wider than the terminal.
func TestRenderFillsTheFrameExactly(t *testing.T) {
	t.Parallel()

	for _, name := range append([]string{"no snapshot"}, fixtures...) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var snap *snapshot.Snapshot
			sels := []dashboard.Selection{{}}
			if name != "no snapshot" {
				snap = fixture(t, name)
				if all := selections(snap); len(all) > 0 {
					sels = append(sels, all[len(all)-1])
				}
			}
			frame := func(sel dashboard.Selection, on bool) dashboard.Frame {
				return dashboard.Frame{
					Snapshot: snap, Selection: sel, Now: at, Help: on, Btop: on,
					Note: "focus off", Errors: []error{errors.New(strings.Repeat("a long error ", 20))},
				}
			}
			for _, w := range widths {
				for _, h := range heights {
					for _, sel := range sels {
						for _, on := range []bool{false, true} {
							label := fmt.Sprintf("%dx%d sel=%+v help,btop=%v", w, h, sel, on)
							if !checkFrame(t, dashboard.Render(frame(sel, on), w, h, plain), w, h, label) {
								return
							}
						}
					}
				}
			}
			for _, size := range huge {
				checkFrame(t, dashboard.Render(frame(sels[len(sels)-1], true), size[0], size[1], plain), size[0], size[1], fmt.Sprint(size))
			}
		})
	}
}

func TestRenderNothingWithoutASize(t *testing.T) {
	t.Parallel()

	for _, size := range [][2]int{{0, 24}, {80, 0}, {-1, -1}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: at}, size[0], size[1], plain))
		})
	}
}

// sgr matches one SGR sequence and captures its parameters.
var sgr = regexp.MustCompile(`\x1b\[([0-9;:]*)m`)

// selectedLine returns the text of the one line drawn in reverse video.
func selectedLine(t *testing.T, frame string) string {
	t.Helper()
	var found []string
	for line := range strings.SplitSeq(frame, "\n") {
		for _, m := range sgr.FindAllStringSubmatch(line, -1) {
			if hasParam(m[1], "7") {
				found = append(found, ansi.Strip(line))
				break
			}
		}
	}
	require.Len(t, found, 1, "exactly one line must be in reverse video")
	return found[0]
}

func hasParam(params, want string) bool {
	for p := range strings.SplitSeq(params, ";") {
		if p == want {
			return true
		}
	}
	return false
}

// TestRenderKeepsTheSelectionVisible: whichever agent is selected, whatever
// the previous scroll and however short the terminal, the selected agent's
// row is on screen, in reverse video, and it is the only such row.
func TestRenderKeepsTheSelectionVisible(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"full", "crowd", "blocked", "wide"} {
		for _, size := range [][2]int{{80, 24}, {120, 40}, {80, 14}, {60, 15}, {40, 10}, {200, 12}, {80, 9}, {80, 8}, {80, 4}} {
			t.Run(fmt.Sprintf("%s_%dx%d", name, size[0], size[1]), func(t *testing.T) {
				t.Parallel()
				snap := fixture(t, name)
				for _, sel := range selections(snap) {
					agent := groupsOf(snap)[sel.Group].Agents[sel.Agent]
					for _, scroll := range []int{0, 7, -5, 1 << 30} {
						f := dashboard.Frame{Snapshot: snap, Selection: sel, Scroll: scroll, Now: at, Btop: true}
						line := selectedLine(t, dashboard.Render(f, size[0], size[1], attrs))
						assert.Contains(t, line, agent.Name, "selection %+v, scroll %d", sel, scroll)
					}
				}
			})
		}
	}
}

// lines is frame split into lines.
func lines(frame string) []string {
	return strings.Split(frame, "\n")
}

// lineWith returns the first line of frame holding s.
func lineWith(t *testing.T, frame, s string) string {
	t.Helper()
	for _, l := range lines(frame) {
		if strings.Contains(l, s) {
			return l
		}
	}
	require.Failf(t, "not on screen", "%q in\n%s", s, frame)
	return ""
}

// TestRenderLayout: the header is a box across the terminal, one blank line
// separates it from the groups, and the groups stack under it, each a box
// as wide as the terminal and as tall as its agents, in display order.
func TestRenderLayout(t *testing.T) {
	t.Parallel()

	out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Btop: true}, 120, 40, plain)
	ls := lines(out)
	assert.Equal(t, "╭"+strings.Repeat("─", 118)+"╮", ls[0])
	assert.True(t, strings.HasPrefix(ls[1], "│ agentisan · PRs "), ls[1])
	assert.True(t, strings.HasPrefix(ls[2], "│ boss ▸ "), ls[2])
	assert.Equal(t, "╰"+strings.Repeat("─", 118)+"╯", ls[3])
	assert.Empty(t, ls[4], "one blank line between the header and the groups")
	assert.True(t, strings.HasPrefix(ls[5], "╭─ CODERS · 9 "), ls[5])
	assert.Equal(t, "│ write the code"+strings.Repeat(" ", 102)+" │", ls[6], "the description follows the title")

	// The boxes stack with no gap and no stretching: each bottom border is
	// followed at once by the next group's top.
	var tops []string
	for i, l := range ls {
		if strings.HasPrefix(l, "╭─ ") {
			tops = append(tops, strings.Fields(l)[1])
			assert.True(t, strings.HasPrefix(ls[i-1], "╰") || i == 5, "line %d: a box follows a box", i)
		}
		if strings.HasPrefix(l, "╭") || strings.HasPrefix(l, "╰") || strings.HasPrefix(l, "│") {
			assert.Equal(t, 120, ansi.StringWidth(l), "line %d spans the terminal: %q", i, l)
		}
	}
	assert.Equal(t, []string{"CODERS", "PRECHECK", "QA", "CODEX", "RESEARCH", "CLERK"}, tops)
	// 5 header lines, 30 lines of groups, then blank lines down to the footer.
	assert.True(t, strings.HasPrefix(ls[34], "╰"), ls[34])
	for _, l := range ls[35:39] {
		assert.Empty(t, l)
	}
}

// TestRenderContents: what a frame must and must not show, by scenario.
// want and not are substrings; match are regular expressions.
func TestRenderContents(t *testing.T) {
	t.Parallel()

	from := func(name string, w, h int) func(t *testing.T) (dashboard.Frame, int, int) {
		return func(t *testing.T) (dashboard.Frame, int, int) {
			t.Helper()
			return dashboard.Frame{Snapshot: fixture(t, name), Now: at, Btop: true}, w, h
		}
	}
	of := func(s *snapshot.Snapshot, w, h int) func(*testing.T) (dashboard.Frame, int, int) {
		return func(*testing.T) (dashboard.Frame, int, int) { return dashboard.Frame{Snapshot: s, Now: at}, w, h }
	}
	one := func(team string, a snapshot.Agent) *snapshot.Snapshot {
		return &snapshot.Snapshot{V: 1, Team: team, Groups: []snapshot.Group{{Name: "g", Agents: []snapshot.Agent{a}}}}
	}
	working := func(name string, ctx *int) snapshot.Agent {
		return snapshot.Agent{Name: name, Status: snapshot.StatusWorking, Model: "opus", Ctx: ctx}
	}
	withProject := func(p *snapshot.Project) *snapshot.Snapshot {
		return &snapshot.Snapshot{V: 1, Team: "t", Project: p}
	}
	stageAt := func(ts time.Time) *snapshot.Snapshot {
		a := working("x", new(1))
		a.Stage, a.StageStartedAt = "s", &ts
		return one("t", a)
	}
	tests := map[string]struct {
		frame func(t *testing.T) (dashboard.Frame, int, int)
		want  []string
		not   []string
		match []string
	}{
		// The project line: PRs in full where they fit, then compact (no
		// merge state), then a count; issues and slots after them.
		"PRs in full at 200": {
			frame: from("full", 200, 60),
			want:  []string{"│ agentisan · PRs #878 ⏳… blocked  #882 ✓💬 blocked  #884 ✗… dirty  #886 ✓👍 clean  #887 ✓👍 behind · issues 14 · slots 2/2 "},
		},
		"PRs compact at 120": {
			frame: from("full", 120, 40),
			want:  []string{"│ agentisan · PRs #878⏳… #882✓💬 #884✗… #886✓👍 #887✓👍 · issues 14 · slots 2/2 "},
			not:   []string{"#878 ⏳… blocked", "dirty"},
		},
		"PRs counted at 80": {frame: from("full", 80, 24), want: []string{"│ agentisan · PRs 5 · issues 14 · slots 2/2 "}, not: []string{"#878"}},
		"no open PRs":       {frame: from("crowd", 120, 40), want: []string{"│ crowd · PRs none · issues 0 "}, not: []string{"slots"}},
		// Nothing reported, nothing shown: no "PRs ?", no "issues 0".
		"no project shows the name alone": {frame: from("blocked", 120, 40), match: []string{`│ assay +\[ btop \] │`}, not: []string{"PRs", "issues", "slots"}},
		"a colour and slots alone":        {frame: from("wide", 120, 40), want: []string{"チーム​ · slots 0/3 "}, not: []string{"PRs", "issues"}},
		"every PR state has a glyph": {
			frame: of(withProject(&snapshot.Project{PRs: []snapshot.PR{
				{Number: 1, CI: snapshot.CIUnknown, Codex: snapshot.CodexUnknown, Merge: snapshot.MergeUnknown},
				{Number: 2, CI: snapshot.CIPending, Codex: snapshot.CodexPending, Merge: snapshot.MergeDraft},
				{Number: 3, CI: snapshot.CIFailure, Codex: snapshot.CodexFindings, Merge: snapshot.MergeUnstable},
				{Number: 4, CI: snapshot.CISuccess, Codex: snapshot.CodexApproved, Merge: snapshot.MergeHasHooks},
			}}), 200, 30),
			want: []string{"PRs #1 ?? unknown  #2 ⏳… draft  #3 ✗💬 unstable  #4 ✓👍 has_hooks"},
		},
		// Render draws what it is given, validated or not: a colour that is
		// not #rrggbb is no colour, never a broken frame.
		"a colour that is not #rrggbb": {frame: of(withProject(&snapshot.Project{Color: "\x1b[31mred"}), 80, 24), want: []string{"│ t "}, not: []string{"\x1b"}},

		// The boss line: state, item, stage, watchers, ctx and runtime, the
		// clock on the right. Watchers then runtime give way first.
		"the boss line in full": {
			frame: from("full", 120, 40),
			match: []string{`│ boss ▸ ◐ working · #867 release · review · watchers 4 · ctx ▓▓░░░░░░ 23% · up 2h14m +17:19 │`},
		},
		"the boss line at 80": {
			frame: from("full", 80, 24),
			match: []string{`│ boss ▸ ◐ working · #867 release · review · ctx ▓▓░░░░░░ 23% +17:19 │`},
			not:   []string{"watchers", "up 2h"},
		},
		"no boss":                         {frame: from("blocked", 80, 24), match: []string{`│ boss ▸ not running +17:19 │`}},
		"a boss with no watchers counted": {frame: from("crowd", 120, 40), want: []string{"watchers 0"}, not: []string{" up "}},
		"a missing ctx is an empty gauge": {
			frame: of(&snapshot.Snapshot{V: 1, Team: "t", Boss: &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", Status: snapshot.StatusIdle}}, Groups: []snapshot.Group{
				{Name: "coders", Agents: []snapshot.Agent{{Name: "pee01", Status: snapshot.StatusIdle, Model: "opus"}}},
			}}, 80, 24),
			want:  []string{"boss ▸ ● idle · ctx ░░░░░░░░ --"},
			match: []string{`│ ● pee01 +idle +opus +░░░░░   -- +│`},
		},

		// Group boxes.
		"a title counts its non-zero statuses": {frame: from("full", 120, 40), want: []string{"╭─ CODERS · 9 ✔1 ◐4 ●2 ⚠1 ⟳1 ─", "╭─ CLERK · 1 ?1 ─", "╭─ QA · 1 ✔1 ─"}},
		"an empty group says so": {
			frame: of(&snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "qa"}}}, 80, 24),
			want:  []string{"╭─ QA · 0 ─", "│ no agents "},
		},
		// Render draws what it is given: a status the contract does not
		// know is drawn and counted as unknown.
		"an invalid status counts as unknown": {
			frame: of(one("t", snapshot.Agent{Name: "x", Status: "asleep"}), 80, 24),
			want:  []string{"╭─ G · 1 ?1 ─", "│ ? x  unknown "},
			not:   []string{"asleep"},
		},
		"no groups":       {frame: from("empty", 80, 24), want: []string{"no agents in this team"}, not: []string{"╭─ "}},
		"no snapshot yet": {frame: func(*testing.T) (dashboard.Frame, int, int) { return dashboard.Frame{Now: at}, 80, 24 }, want: []string{"waiting for the daemon…"}, not: []string{"╭─ "}},

		// One dense line per agent: glyph and name, state, model, ctx,
		// item, stage, and time in the stage.
		"an agent row":                {frame: from("full", 120, 40), match: []string{`│ ◐ pee04 +working +opus +▓▓▓░░  61% +NERD-5255 +go-review +3m │`}},
		"a row without item or stage": {frame: from("full", 120, 40), match: []string{`│ ● pee06 +idle +son +▓░░░░  12% +│`}},
		"a stage time in days":        {frame: from("full", 120, 40), match: []string{`✔ pee07 .* merged +2d08h │`}},
		"a stage started now":         {frame: from("full", 120, 40), match: []string{`⟳ pee08 .* handoff read +0m │`}},
		// A stage start after the frame's clock (a clock ahead of the
		// daemon's) is no time at all, never a negative one.
		"a stage start in the future":       {frame: from("full", 120, 40), match: []string{`◐ pee09 .* resolver +│`}},
		"a long stage is cut at its column": {frame: from("full", 80, 24), match: []string{`◐ pee09 .* precheck: tdaddy …  +│`}},
		"the time column aligns":            {frame: of(stageAt(at.Add(-90*time.Minute)), 80, 24), match: []string{`│ ◐ x +working +opus +░░░░░   1% +s +1h30m │`}},
		"a name that exactly fits": {
			frame: of(one("t", working(strings.Repeat("n", 16), nil)), 80, 24),
			want:  []string{"│ ◐ " + strings.Repeat("n", 16) + "  working"},
		},
		"a name one cell past it": {
			frame: of(one("t", working(strings.Repeat("n", 17), nil)), 80, 24),
			want:  []string{"│ ◐ " + strings.Repeat("n", 15) + "…  working"},
		},
		"ctx above 100 fills the gauge": {frame: of(one("t", working("x", new(500))), 80, 24), want: []string{"▓▓▓▓▓ 500%"}},
		"ctx below 0 empties the gauge": {frame: of(one("t", working("x", new(-5))), 80, 24), want: []string{"░░░░░  -5%"}},

		// Snapshot text reaches the terminal: an escape sequence in it must
		// arrive as plain text, never as a control sequence.
		"control characters in snapshot text are neutralised": {
			frame: of(&snapshot.Snapshot{V: 1, Team: "t\x1b[2Jx", Groups: []snapshot.Group{
				{Name: "qa", Description: "d\x07", Agents: []snapshot.Agent{{Name: "a\nb", Status: snapshot.StatusIdle, Stage: "s\x1b]0;pwn\x07"}}},
			}}, 80, 24),
			want: []string{"t [2Jx", "a b", "s ]0;pwn ", "│ d "},
			not:  []string{"\x1b", "\x07"},
		},
		"invalid UTF-8 in text is replaced": {frame: of(one("a\xffb", working("x", nil)), 80, 24), want: []string{"a�b"}},
		"a truncated multibyte is replaced": {frame: of(one("x\xe2\x82", working("x", nil)), 80, 24), want: []string{"x�"}},
		"NUL, CR and CRLF become spaces":    {frame: of(one("a\x00b\rc\r\nd", working("x", nil)), 80, 24), want: []string{"a b c  d"}},
		"empty text fields":                 {frame: of(one("", snapshot.Agent{Name: "x", Status: snapshot.StatusIdle}), 80, 24), want: []string{"│ ● x  idle  ░░░░░   -- "}},
		"wide and combining text keeps place": {
			frame: from("wide", 120, 40),
			want:  []string{"エージェント", "👍👍👍", "nfd-é", "日本 ", "とても長い段階名がカードからはみ出します"},
		},
		// Upper-cased as written: the fixture's decomposed é stays two
		// code points, never normalized.
		"a group name is upper-cased": {frame: from("wide", 120, 40), want: []string{"╭─ CAFE\u0301 · 1 ⚠1 ─"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, w, h := tc.frame(t)
			out := dashboard.Render(f, w, h, plain)
			for _, s := range tc.want {
				assert.Contains(t, out, s)
			}
			for _, s := range tc.not {
				assert.NotContains(t, out, s)
			}
			for _, re := range tc.match {
				assert.Regexp(t, re, out)
			}
		})
	}
}

// columnsOf names the optional columns drawn in pee04's row of the full
// fixture at width w: state, model, item, stage and time.
func columnsOf(t *testing.T, w int) []string {
	t.Helper()
	out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: at}, w, 60, plain)
	// pee04 is the fourth row under the coders' description: found by
	// place, because at the narrowest widths its name is cut too.
	ls := lines(out)
	desc := slices.IndexFunc(ls, func(l string) bool { return strings.HasPrefix(l, "│ write the") })
	require.GreaterOrEqual(t, desc, 0, "at %d:\n%s", w, out)
	row := ls[desc+4]
	cols := []string{}
	// The stage is at least 8 cells wide, so "go-rev" survives its cut.
	for _, c := range [][2]string{{"state", "working"}, {"model", "opus"}, {"item", "NERD-5255"}, {"stage", "go-rev"}, {"time", " 3m "}} {
		if strings.Contains(row, c[1]) {
			cols = append(cols, c[0])
		}
	}
	assert.Contains(t, row, "◐ p", "glyph and name always stay, at %d", w)
	if w >= 30 {
		assert.Contains(t, row, "61%", "ctx always stays, at %d", w)
	}
	return cols
}

// TestColumnsDropRightToLeft: as the terminal narrows, the optional columns
// drop in a fixed order, time, stage, item, model, state, and never come
// back at a narrower width. Glyph, name and ctx stay.
func TestColumnsDropRightToLeft(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		w    int
		want []string
	}{
		"every column at 80":               {w: 80, want: []string{"state", "model", "item", "stage", "time"}},
		"time drops first":                 {w: 64, want: []string{"state", "model", "item", "stage"}},
		"then the stage":                   {w: 60, want: []string{"state", "model", "item"}},
		"then the item":                    {w: 50, want: []string{"state", "model"}},
		"then the model":                   {w: 40, want: []string{"state"}},
		"then the state: glyph, name, ctx": {w: 34, want: []string{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, columnsOf(t, tc.w))
		})
	}

	t.Run("never back at a narrower width", func(t *testing.T) {
		t.Parallel()
		order := []string{"state", "model", "item", "stage", "time"}
		prev := order
		for w := 200; w >= 20; w-- {
			cols := columnsOf(t, w)
			assert.Equal(t, order[:len(cols)], cols, "at %d the columns are a prefix of the order", w)
			assert.LessOrEqual(t, len(cols), len(prev), "at %d a column came back", w)
			prev = cols
		}
	})
}

// TestElapsed: the time in a stage, and the boss's runtime, as one short
// token. Not reported, or after the clock, is no time at all.
func TestElapsed(t *testing.T) {
	t.Parallel()

	ago := func(d time.Duration) *time.Time { return new(at.Add(-d)) }
	tests := map[string]struct {
		from *time.Time
		want string
	}{
		"not reported":      {from: nil, want: ""},
		"now":               {from: ago(0), want: "0m"},
		"59 seconds":        {from: ago(59 * time.Second), want: "0m"},
		"60 seconds":        {from: ago(time.Minute), want: "1m"},
		"59m59s":            {from: ago(59*time.Minute + 59*time.Second), want: "59m"},
		"an hour":           {from: ago(time.Hour), want: "1h00m"},
		"23h59m59s":         {from: ago(23*time.Hour + 59*time.Minute + 59*time.Second), want: "23h59m"},
		"24 hours":          {from: ago(24 * time.Hour), want: "1d00h"},
		"99d23h59m":         {from: ago(99*24*time.Hour + 23*time.Hour + 59*time.Minute), want: "99d23h"},
		"100 days":          {from: ago(100 * 24 * time.Hour), want: ">99d"},
		"years":             {from: new(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)), want: ">99d"},
		"the zero time":     {from: new(time.Time{}), want: ">99d"},
		"one second ahead":  {from: ago(-time.Second), want: ""},
		"years ahead":       {from: new(time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)), want: ""},
		"another time zone": {from: new(at.Add(-90 * time.Minute).In(time.FixedZone("UTC+13", 13*3600))), want: "1h30m"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, dashboard.Elapsed(tc.from, at))
		})
	}
	assert.Empty(t, dashboard.Elapsed(ago(time.Hour), time.Time{}), "a zero clock is before every time")
}

// TestRenderScroll: when the groups are taller than the pane they scroll,
// just far enough to keep the selection, its group's title when it is the
// group's first agent and its bottom border when it is the last. The footer
// counts the agents out of view.
func TestRenderScroll(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		sel    dashboard.Selection
		scroll int
		want   []string
		not    []string
		footer string
	}{
		"the first agent: no scroll":             {sel: dashboard.Selection{}, want: []string{"╭─ CODERS", "pee01", "qa"}, not: []string{"codex", "CLERK"}, footer: "↓3"},
		"the last agent: scrolled to the end":    {sel: dashboard.Selection{Group: 5}, want: []string{"╭─ CLERK", "? clerk"}, not: []string{"pee05", "CODERS"}, footer: "↑9"},
		"a kept scroll stays while it can":       {sel: dashboard.Selection{Agent: 8}, scroll: 4, want: []string{"pee03", "pee09", "PRECHECK"}, not: []string{"pee02"}, footer: "↑2 ↓2"},
		"a scroll that hides the selection":      {sel: dashboard.Selection{}, scroll: 9, want: []string{"╭─ CODERS", "pee01"}, footer: "↓3"},
		"a negative scroll is none":              {sel: dashboard.Selection{}, scroll: -3, want: []string{"╭─ CODERS"}, footer: "↓3"},
		"a scroll past the end stops at the end": {sel: dashboard.Selection{Group: 5}, scroll: 1 << 30, want: []string{"╰" + strings.Repeat("─", 78) + "╯"}, footer: "↑9"},
		"a group's first agent shows its title":  {sel: dashboard.Selection{Group: 3}, want: []string{"╭─ CODEX", "Codex review triage"}, not: []string{"│ ◐ pee01"}, footer: "↑3 ↓2"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := dashboard.Frame{Snapshot: fixture(t, "full"), Selection: tc.sel, Scroll: tc.scroll, Now: at}
			out := dashboard.Render(f, 80, 24, plain)
			ls := lines(out)
			body := strings.Join(ls[5:23], "\n")
			for _, s := range tc.want {
				assert.Contains(t, body, s)
			}
			for _, s := range tc.not {
				assert.NotContains(t, body, s)
			}
			assert.True(t, strings.HasSuffix(ls[23], " "+tc.footer), "footer %q", ls[23])
			assert.Equal(t, "", ls[4], "the gap under the header never scrolls away")
		})
	}
}

// TestHeaderDegradesWithHeight: the header is a box with a gap under it
// from 9 lines up; below that it is two bare lines, then one, and at one
// line the frame is that line alone.
func TestHeaderDegradesWithHeight(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		h     int
		first []string // the frame's first lines, as prefixes
		last  string   // the last line's prefix
	}{
		"9: box and gap": {h: 9, first: []string{"╭───", "│ agentisan", "│ boss ▸", "╰───", "", "╭─ CODERS"}, last: " enter jump"},
		"8: bare":        {h: 8, first: []string{" agentisan", " boss ▸", "╭─ CODERS"}, last: " enter jump"},
		// One line for the groups: it is the selected agent's.
		"4: bare":      {h: 4, first: []string{" agentisan", " boss ▸", "│ ◐ pee01"}, last: " enter jump"},
		"3: one line":  {h: 3, first: []string{" agentisan", "│ ◐ pee01"}, last: " enter jump"},
		"2: no groups": {h: 2, first: []string{" agentisan"}, last: " enter jump"},
		"1: the name":  {h: 1, first: []string{" agentisan"}, last: " agentisan"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ls := lines(dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: at}, 80, tc.h, plain))
			require.Len(t, ls, tc.h)
			for i, p := range tc.first {
				if p == "" {
					assert.Empty(t, ls[i])
					continue
				}
				assert.True(t, strings.HasPrefix(ls[i], p), "line %d %q, want %q", i, ls[i], p)
			}
			assert.True(t, strings.HasPrefix(ls[tc.h-1], tc.last), "last line %q", ls[tc.h-1])
		})
	}
}

// TestBtopButton: the button sits at the right edge of the header's first
// line, only when herdr can open btop and the line has room for it, and it
// is never drawn in reverse video (that is the selection's).
func TestBtopButton(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		btop bool
		w, h int
		want string // the line's suffix; "" for no button
	}{
		"boxed":                {btop: true, w: 80, h: 24, want: "  [ btop ] │"},
		"bare":                 {btop: true, w: 80, h: 6, want: "  [ btop ]"},
		"no opener, no button": {btop: false, w: 80, h: 24},
		"too narrow for it":    {btop: true, w: 20, h: 24},
		"just wide enough":     {btop: true, w: 24, h: 24, want: "[ btop ] │"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Btop: tc.btop, Selection: dashboard.Selection{Agent: 1}}
			out := dashboard.Render(f, tc.w, tc.h, plain)
			if tc.want == "" {
				assert.NotContains(t, out, "btop")
				return
			}
			first := lines(out)[1]
			if tc.h < 9 {
				first = lines(out)[0]
			}
			assert.True(t, strings.HasSuffix(first, tc.want), "%q", first)
			assert.Contains(t, selectedLine(t, dashboard.Render(f, tc.w, tc.h, attrs)), "pee02", "the button is not the reverse-video line")
		})
	}
}

// TestRenderClock: the header shows the frame's clock as HH:MM in the
// clock's own location, at the right of the boss line.
func TestRenderClock(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		now  time.Time
		want string
	}{
		"the zero time":                    {now: time.Time{}, want: "00:00"},
		"midnight":                         {now: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), want: "00:00"},
		"one minute to midnight":           {now: time.Date(2026, 10, 9, 23, 59, 59, 0, time.UTC), want: "23:59"},
		"a fixed zone keeps its wall time": {now: time.Date(2026, 10, 9, 23, 59, 0, 0, time.FixedZone("UTC-7", -7*3600)), want: "23:59"},
		"a zone east of UTC":               {now: time.Date(2026, 10, 9, 8, 5, 0, 0, time.FixedZone("UTC+13", 13*3600)), want: "08:05"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: tc.now}, 120, 40, plain)
			boss := lineWith(t, out, "boss ▸")
			assert.True(t, strings.HasSuffix(boss, " "+tc.want+" │"), "%q", boss)
		})
	}
}

// TestRenderIgnoresAnInvalidSelection: a selection that names no agent draws
// no highlight and still a whole frame, never a panic.
func TestRenderIgnoresAnInvalidSelection(t *testing.T) {
	t.Parallel()

	tests := map[string]dashboard.Selection{
		"a group past the last":  {Group: 99},
		"an agent past the last": {Agent: 99},
		"a negative group":       {Group: -1},
		"a negative agent":       {Agent: -1},
		"a huge group":           {Group: 1 << 62},
	}
	for name, sel := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Selection: sel, Now: at}, 80, 24, attrs)
			ls := lines(out)
			require.Len(t, ls, 24)
			for _, l := range ls {
				for _, m := range sgr.FindAllStringSubmatch(l, -1) {
					assert.False(t, hasParam(m[1], "7"), "reverse video on %q", ansi.Strip(l))
				}
			}
		})
	}
}

// TestOrder: the known groups come in the team's working order, any other
// group follows alphabetically.
func TestOrder(t *testing.T) {
	t.Parallel()

	names := func(gs []snapshot.Group) []string {
		out := make([]string, 0, len(gs))
		for _, g := range gs {
			out = append(out, g.Name)
		}
		return out
	}
	in := []snapshot.Group{
		{Name: "zeta"},
		{Name: "debugger"},
		{Name: "alpha"},
		{Name: "coders"},
		{Name: "qa"},
		{Name: "clerk"},
		{Name: "research"},
		{Name: "codex"},
		{Name: "precheck"},
		{Name: "Beta"},
	}
	got := dashboard.Order(in)
	assert.Equal(t, []string{"coders", "precheck", "qa", "codex", "research", "clerk", "debugger", "alpha", "Beta", "zeta"}, names(got))
	assert.Equal(t, "zeta", in[0].Name, "Order must not reorder its argument")
}

func TestShortModel(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"opus":              "opus",
		"claude-opus-4-1":   "opus",
		"Opus 4.1":          "opus",
		"sonnet":            "son",
		"claude-sonnet-4-5": "son",
		"Sonnet 4.5":        "son",
		"haiku":             "hai",
		"claude-haiku-4-5":  "hai",
		"gpt-5-codex":       "gpt-",
		"Codex":             "code",
		"o3":                "o3",
		"  Gemini 2.5 Pro":  "gemi",
		"ÉCLAIR":            "écla",
		// Four cells, not four runes: a wide model never overflows its column.
		"日本モデル": "日本",
		"👍👍👍":   "👍👍",
		"":      "",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, dashboard.ShortModel(in))
		})
	}
}

// truecolor is the SGR fragment lipgloss writes for a 24-bit foreground.
func truecolor(hex string) string {
	var r, g, b int
	_, _ = fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
	return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
}

// TestCtxColour: ctx is coloured by the rotation thresholds (ADR-001 D7),
// in the agent's row and on the boss line, under a TrueColor profile.
func TestCtxColour(t *testing.T) {
	t.Parallel()

	const (
		green  = "#50fa7b"
		yellow = "#f1fa8c"
		orange = "#ffb86c"
		red    = "#ff5555"
	)
	tests := map[string]struct {
		ctx  int
		th   dashboard.Thresholds
		want string
	}{
		"0 is green":                   {ctx: 0, want: green},
		"54 is green":                  {ctx: 54, want: green},
		"55, soft, is yellow":          {ctx: 55, want: yellow},
		"64 is yellow":                 {ctx: 64, want: yellow},
		"65, hard, is orange":          {ctx: 65, want: orange},
		"74 is orange":                 {ctx: 74, want: orange},
		"75, ceiling, is red":          {ctx: 75, want: red},
		"80 is red":                    {ctx: 80, want: red},
		"100 is red":                   {ctx: 100, want: red},
		"custom thresholds: 30 warns":  {ctx: 30, th: dashboard.Thresholds{Soft: 20, Hard: 40, Ceiling: 60}, want: yellow},
		"custom thresholds: 60 is red": {ctx: 60, th: dashboard.Thresholds{Soft: 20, Hard: 40, Ceiling: 60}, want: red},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := snapshot.Agent{Name: "pee01", PaneID: "w2:p1", Status: snapshot.StatusWorking, Model: "opus", Ctx: new(tc.ctx)}
			boss := snapshot.Boss{Agent: a}
			boss.Name = "boss"
			snap := &snapshot.Snapshot{V: 1, Team: "t", Boss: &boss, Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{a}}}}
			opts := dashboard.Options{Profile: colorprofile.TrueColor, Thresholds: tc.th}

			// Select nothing: reverse video would change the row's SGR.
			out := dashboard.Render(dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at}, 80, 24, opts)
			pct := regexp.MustCompile(regexp.QuoteMeta(truecolor(tc.want)) + `[0-9;]*m\s*` + fmt.Sprint(tc.ctx) + `%`)
			assert.Len(t, pct.FindAllString(out, -1), 2, "the row and the boss line each colour %d%% %s", tc.ctx, tc.want)
		})
	}
}

// TestRenderStatusIcons: each status has its own icon in the agent's row.
func TestRenderStatusIcons(t *testing.T) {
	t.Parallel()

	tests := map[snapshot.Status]string{
		// ADR-001's frozen $team token: ◐ is working, ● is idle.
		snapshot.StatusWorking: "◐",
		snapshot.StatusIdle:    "●",
		snapshot.StatusBlocked: "⚠",
		snapshot.StatusDone:    "✔",
		snapshot.StatusReady:   "⟳",
		snapshot.StatusUnknown: "?",
	}
	for status, icon := range tests {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{
				{Name: "g", Agents: []snapshot.Agent{{Name: "a1", Status: status, Model: "opus", Ctx: new(1)}}},
			}}
			out := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at}, 80, 24, plain)
			assert.Contains(t, out, "│ "+icon+" a1 ")
			assert.Contains(t, out, "╭─ G · 1 "+icon+"1 ─", "the title counts it with the same glyph")
		})
	}
}

// TestRenderStatusColours: under TrueColor each status icon carries its
// colour.
func TestRenderStatusColours(t *testing.T) {
	t.Parallel()

	tests := map[snapshot.Status]string{
		snapshot.StatusWorking: "#50fa7b",
		snapshot.StatusIdle:    "#f1fa8c",
		snapshot.StatusBlocked: "#ff5555",
		snapshot.StatusDone:    "#6c7086",
		snapshot.StatusReady:   "#8be9fd",
	}
	for status, hex := range tests {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			snap := &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{
				{Name: "g", Agents: []snapshot.Agent{{Name: "a1", Status: status, Model: "opus"}, {Name: "a2", Status: status}}},
			}}
			// Select the second agent: reverse video on the first would
			// change its SGR.
			f := dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Agent: 1}, Now: at}
			out := dashboard.Render(f, 80, 24, dashboard.Options{Profile: colorprofile.TrueColor})
			icon := regexp.MustCompile(regexp.QuoteMeta(truecolor(hex)) + `[0-9;]*m` + regexp.QuoteMeta(dashboard.StatusIcon(status)))
			assert.Regexp(t, icon, out)
		})
	}
}

// TestRenderProjectColour: the project's name is drawn in its colour, and
// the header box's border with it.
func TestRenderProjectColour(t *testing.T) {
	t.Parallel()

	out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: at}, 120, 40, dashboard.Options{Profile: colorprofile.TrueColor})
	assert.Regexp(t, regexp.QuoteMeta(truecolor("#bd93f9"))+`[0-9;]*magentisan`, out)
	assert.Regexp(t, `^\x1b\[[0-9;]*`+regexp.QuoteMeta(truecolor("#bd93f9"))+`[0-9;]*m╭`, out)
}

// TestRenderSelectionIsReverseVideo: the selected agent's row, and only that
// row, is drawn in reverse video.
func TestRenderSelectionIsReverseVideo(t *testing.T) {
	t.Parallel()

	f := dashboard.Frame{Snapshot: fixture(t, "full"), Selection: dashboard.Selection{Agent: 1}, Now: at}
	line := selectedLine(t, dashboard.Render(f, 120, 40, attrs))
	assert.Contains(t, line, "◐ pee02")
	assert.Contains(t, line, "J1 re-queued", "the whole row, stage included")
	assert.NotContains(t, line, "pee03")
}

func TestRenderFooter(t *testing.T) {
	t.Parallel()

	const hints = "enter jump · ↑↓ move · ? help · q quit"
	tests := map[string]struct {
		frame dashboard.Frame
		want  string
	}{
		"hints alone": {frame: dashboard.Frame{Now: at}, want: " " + hints},
		// ctrl-b m is herdr's popup key for btop in this setup; it is only
		// advertised where herdr is reachable.
		"the btop hint with herdr": {frame: dashboard.Frame{Now: at, Btop: true}, want: " enter jump · ↑↓ move · ctrl-b m btop · ? help · q quit"},
		"a note leads the hints": {
			frame: dashboard.Frame{Now: at, Note: "focus off"},
			want:  " focus off · " + hints,
		},
		"errors lead the note": {
			frame: dashboard.Frame{Now: at, Note: "focus off", Errors: []error{errors.New("e1"), nil, errors.New("e2")}},
			want:  " ✖ e1 · ✖ e2 · focus off · " + hints,
		},
		"a long error is cut to one line": {
			frame: dashboard.Frame{Now: at, Errors: []error{errors.New(strings.Repeat("x", 200))}},
			want:  " ✖ " + strings.Repeat("x", 75) + "…",
		},
		"an error's newlines stay on one line": {
			frame: dashboard.Frame{Now: at, Errors: []error{errors.New("a\nb")}},
			want:  " ✖ a b · " + hints,
		},
		"the agents out of view, on the right": {
			frame: dashboard.Frame{Snapshot: &snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "g", Agents: slices.Repeat([]snapshot.Agent{{Name: "a", Status: snapshot.StatusIdle}}, 40)}}}, Now: at},
			want:  " " + hints + strings.Repeat(" ", 80-1-len([]rune(hints))-1-4) + " ↓23",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ls := lines(dashboard.Render(tc.frame, 80, 24, plain))
			assert.Equal(t, tc.want, ls[len(ls)-1])
		})
	}
}

// TestHelpOverlay: the help names every key that works and every status
// glyph, and mentions btop only where it can be opened.
func TestHelpOverlay(t *testing.T) {
	t.Parallel()

	for _, btop := range []bool{false, true} {
		t.Run(fmt.Sprint("btop=", btop), func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Help: true, Btop: btop}, 120, 40, plain)
			for _, s := range []string{"╭ help", "↑ ↓", "enter", "click", "?", "q", "◐ working", "● idle", "⚠ blocked", "✔ done", "⟳ ready", "? unknown"} {
				assert.Contains(t, out, s)
			}
			assert.Equal(t, btop, strings.Contains(out, "[ btop ]  btop in a herdr popup"))
			for _, s := range []string{"← →", "/ ", "b back"} {
				assert.NotContains(t, out, s, "keys that do nothing are not advertised")
			}
		})
	}
}

// TestSampleSnapshotMirrorsTheFixture: docs/dashboard/sample-snapshot.json is
// what a reader copies to try the dashboard. It must stay the snapshot the
// goldens were drawn from.
func TestSampleSnapshotMirrorsTheFixture(t *testing.T) {
	t.Parallel()

	sample, err := snapshot.FileSource{Path: filepath.Join("..", "..", "docs", "dashboard", "sample-snapshot.json")}.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, *fixture(t, "full"), sample)
}

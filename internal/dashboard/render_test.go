package dashboard_test

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
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

// fixture loads testdata/<name>.json through the same source the CLI uses.
func fixture(t *testing.T, name string) *snapshot.Snapshot {
	t.Helper()
	s, err := snapshot.FileSource{Path: filepath.Join("testdata", name+".json")}.Snapshot(t.Context())
	require.NoError(t, err)
	return &s
}

// TestRenderGolden pins whole frames. Regenerate with
// `go test ./internal/dashboard/ -update` (this package only: -update is a
// flag of the golden package, unknown to every other test binary).
func TestRenderGolden(t *testing.T) {
	t.Parallel()

	frame := func(name string, sel dashboard.Selection) func(t *testing.T) dashboard.Frame {
		return func(t *testing.T) dashboard.Frame {
			t.Helper()
			return dashboard.Frame{Snapshot: fixture(t, name), Selection: sel, Now: at}
		}
	}
	tests := map[string]struct {
		frame func(t *testing.T) dashboard.Frame
		w, h  int
	}{
		"full_80x24":                 {frame: frame("full", dashboard.Selection{}), w: 80, h: 24},
		"full_120x40":                {frame: frame("full", dashboard.Selection{}), w: 120, h: 40},
		"empty_80x24":                {frame: frame("empty", dashboard.Selection{}), w: 80, h: 24},
		"empty_120x40":               {frame: frame("empty", dashboard.Selection{}), w: 120, h: 40},
		"blocked_80x24":              {frame: frame("blocked", dashboard.Selection{}), w: 80, h: 24},
		"blocked_120x40":             {frame: frame("blocked", dashboard.Selection{}), w: 120, h: 40},
		"crowd_80x24":                {frame: frame("crowd", dashboard.Selection{}), w: 80, h: 24},
		"crowd_120x40":               {frame: frame("crowd", dashboard.Selection{}), w: 120, h: 40},
		"crowd_selected_pee16_80x24": {frame: frame("crowd", dashboard.Selection{Card: 0, Agent: 15}), w: 80, h: 24},
		"full_selected_qa_80x24":     {frame: frame("full", dashboard.Selection{Card: 2, Agent: 0}), w: 80, h: 24},
		"waiting_80x24": {
			frame: func(*testing.T) dashboard.Frame { return dashboard.Frame{Now: at} }, w: 80, h: 24,
		},
		"help_80x24": {
			frame: func(t *testing.T) dashboard.Frame {
				t.Helper()
				return dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Help: true}
			},
			w: 80, h: 24,
		},
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

// sizes run from one cell to the 16-bit limit of a terminal's window size.
var sizes = [][2]int{{80, 24}, {120, 40}, {60, 20}, {40, 12}, {26, 8}, {20, 6}, {200, 60}, {80, 3}, {80, 2}, {80, 1}, {1, 1}, {1, 24}, {65535, 30}, {80, 65535}}

// TestRenderFillsTheFrameExactly: whatever the size, a frame is exactly h
// lines and no line is wider than the terminal, so bubbletea never wraps or
// scrolls it.
func TestRenderFillsTheFrameExactly(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"full", "empty", "blocked", "crowd", "wide"} {
		for _, size := range sizes {
			for _, help := range []bool{false, true} {
				w, h := size[0], size[1]
				t.Run(fmt.Sprintf("%s_%dx%d_help=%v", name, w, h, help), func(t *testing.T) {
					t.Parallel()
					f := dashboard.Frame{
						Snapshot: fixture(t, name), Now: at, Help: help,
						Errors: []error{errors.New(strings.Repeat("a long error ", 20))},
					}
					lines := strings.Split(dashboard.Render(f, w, h, plain), "\n")
					require.Len(t, lines, h)
					for i, l := range lines {
						assert.LessOrEqual(t, ansi.StringWidth(l), w, "line %d is wider than the terminal: %q", i, l)
						assert.Equal(t, strings.TrimRight(l, " "), l, "line %d has trailing spaces", i)
					}
				})
			}
		}
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

// TestRenderKeepsTheSelectionVisible: whichever agent is selected and however
// small the terminal, the selected agent's first line is on screen in
// reverse video. Other cards collapse first; the selected card scrolls.
func TestRenderKeepsTheSelectionVisible(t *testing.T) {
	t.Parallel()

	attrs := dashboard.Options{Profile: colorprofile.ASCII}
	for _, name := range []string{"full", "crowd", "blocked", "wide"} {
		for _, size := range [][2]int{{80, 24}, {120, 40}, {80, 14}, {60, 15}, {40, 10}, {200, 12}} {
			t.Run(fmt.Sprintf("%s_%dx%d", name, size[0], size[1]), func(t *testing.T) {
				t.Parallel()
				snap := fixture(t, name)
				for _, sel := range selections(snap) {
					agent := cardsOf(snap)[sel.Card].Agents[sel.Agent]
					f := dashboard.Frame{Snapshot: snap, Selection: sel, Now: at}
					line := selectedLine(t, dashboard.Render(f, size[0], size[1], attrs))
					assert.Contains(t, line, agent.Name, "selection %+v", sel)
				}
			})
		}
	}
}

// cardsOf returns the groups in the dashboard's display order.
func cardsOf(s *snapshot.Snapshot) []snapshot.Group {
	return dashboard.Order(s.Groups)
}

func selections(s *snapshot.Snapshot) []dashboard.Selection {
	var out []dashboard.Selection
	for c, g := range cardsOf(s) {
		for a := range g.Agents {
			out = append(out, dashboard.Selection{Card: c, Agent: a})
		}
	}
	return out
}

// TestRenderContents: what a frame must and must not show, by scenario.
// want and not are substrings; match are regular expressions.
func TestRenderContents(t *testing.T) {
	t.Parallel()

	from := func(name string, sel dashboard.Selection) func(t *testing.T) dashboard.Frame {
		return func(t *testing.T) dashboard.Frame {
			t.Helper()
			return dashboard.Frame{Snapshot: fixture(t, name), Selection: sel, Now: at}
		}
	}
	of := func(s *snapshot.Snapshot, errs ...error) func(*testing.T) dashboard.Frame {
		return func(*testing.T) dashboard.Frame { return dashboard.Frame{Snapshot: s, Now: at, Errors: errs} }
	}
	idle := func(name string) snapshot.Agent { return snapshot.Agent{Name: name, Status: snapshot.StatusIdle} }
	tests := map[string]struct {
		frame func(t *testing.T) dashboard.Frame
		w, h  int
		want  []string
		not   []string
		match []string
	}{
		// 21 lines for cards: the one card shows 9 of 20 and says so.
		"one tall card at the top": {frame: from("crowd", dashboard.Selection{}), w: 80, h: 24, want: []string{"pee01", "pee09", "…11 more"}, not: []string{"pee10"}},
		// The selection scrolls the card: pee16 is the last visible row.
		"one tall card scrolled": {frame: from("crowd", dashboard.Selection{Agent: 15}), w: 80, h: 24, want: []string{"pee08", "pee16", "…11 more"}, not: []string{"pee07", "pee17"}},
		"everything fits":        {frame: from("full", dashboard.Selection{}), w: 120, h: 40, want: []string{"pee09", "precheck2", "clerk"}, not: []string{"more"}},
		// Stretched to 39, a card leaves 34 cells for "item · stage" after
		// the indent and the right margin.
		"the detail line is cut to the card": {frame: from("full", dashboard.Selection{}), w: 120, h: 40, want: []string{"│  NERD-5268 · precheck: tdaddy impa… │"}, not: []string{"resolver"}},
		"the help names every status glyph": {
			frame: func(t *testing.T) dashboard.Frame {
				t.Helper()
				return dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Help: true}
			},
			w: 120, h: 40,
			want: []string{"◐ working", "● idle", "⚠ blocked", "✔ done", "⟳ ready", "? unknown"},
		},
		"a missing ctx is an empty gauge": {
			frame: of(&snapshot.Snapshot{V: 1, Team: "t", Boss: &snapshot.Agent{Name: "boss", Status: snapshot.StatusIdle}, Groups: []snapshot.Group{
				{Name: "coders", Agents: []snapshot.Agent{{Name: "pee01", Status: snapshot.StatusIdle, Model: "opus"}}},
			}}),
			w: 80, h: 24,
			want:  []string{"boss ▸ ctx ░░░░░░░░ --"},
			match: []string{`● pee01 +opus ░░░░░   --`},
		},
		"no snapshot yet": {
			frame: of(nil, errors.New("dial: refused")), w: 80, h: 24,
			want: []string{"waiting for the daemon…", "dial: refused"}, not: []string{"╭"},
		},
		"cards run in display order, row by row": {
			frame: of(&snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{
				{Name: "zeta", Agents: []snapshot.Agent{idle("z1")}},
				{Name: "qa", Agents: []snapshot.Agent{idle("q1")}},
				{Name: "coders", Agents: []snapshot.Agent{idle("c1")}},
			}}),
			w: 200, h: 30,
			match: []string{`(?s)╭ coders 1.*╭ qa 1.*╭ zeta 1`},
		},
		"an empty group is a card that says so": {
			frame: of(&snapshot.Snapshot{V: 1, Team: "t", Groups: []snapshot.Group{{Name: "qa"}}}),
			w:     80, h: 24,
			want: []string{"╭ qa 0", "│  no agents"},
		},
		// Snapshot text reaches the terminal: an escape sequence in it must
		// arrive as plain text, never as a control sequence.
		"control characters in snapshot text are neutralised": {
			frame: of(&snapshot.Snapshot{V: 1, Team: "t\x1b[2Jx", Groups: []snapshot.Group{
				{Name: "qa", Agents: []snapshot.Agent{{Name: "a\nb", Status: snapshot.StatusIdle, Stage: "s\x1b]0;pwn\x07"}}},
			}}),
			w: 80, h: 24,
			want: []string{"t [2Jx", "a b", "s ]0;pwn "},
			not:  []string{"\x1b", "\x07"},
		},
	}
	text := func(team string, a snapshot.Agent) func(*testing.T) dashboard.Frame {
		return of(&snapshot.Snapshot{V: 1, Team: team, Groups: []snapshot.Group{{Name: "g", Agents: []snapshot.Agent{a}}}})
	}
	working := func(name string, ctx *int) snapshot.Agent {
		return snapshot.Agent{Name: name, Status: snapshot.StatusWorking, Model: "opus", Ctx: ctx}
	}
	more := map[string]struct {
		frame func(t *testing.T) dashboard.Frame
		w, h  int
		want  []string
		not   []string
		match []string
	}{
		"invalid UTF-8 in text is replaced":   {frame: text("a\xffb", working("x", nil)), w: 80, h: 24, want: []string{"a\ufffdb"}},
		"a truncated multibyte is replaced":   {frame: text("x\xe2\x82", working("x", nil)), w: 80, h: 24, want: []string{"x\ufffd"}},
		"NUL, CR and CRLF become spaces":      {frame: text("a\x00b\rc\r\nd", working("x", nil)), w: 80, h: 24, want: []string{"a b c  d"}},
		"empty text fields":                   {frame: text("", snapshot.Agent{Name: "x", Status: snapshot.StatusIdle}), w: 80, h: 24, want: []string{" ◆  · 1 agent", "│● x "}},
		"wide and combining text keeps place": {frame: from("wide", dashboard.Selection{}), w: 120, h: 40, want: []string{"エージェント", "👍👍👍", "nfd-e\u0301", "日本 ", "とても長い段階名が"}, not: []string{"はみ出します"}},
		// At 40 columns the one card is 40 wide: 19 cells of name fit
		// beside the right-hand block, 20 do not.
		"a name that exactly fits":      {frame: text("t", working(strings.Repeat("n", 19), nil)), w: 40, h: 10, want: []string{"│◐ " + strings.Repeat("n", 19) + " opus"}},
		"a name one cell past it":       {frame: text("t", working(strings.Repeat("n", 20), nil)), w: 40, h: 10, want: []string{"│◐ " + strings.Repeat("n", 18) + "… opus"}},
		"ctx above 100 fills the gauge": {frame: text("t", working("x", new(500))), w: 80, h: 24, want: []string{"▓▓▓▓▓ 500%"}},
		"ctx below 0 empties the gauge": {frame: text("t", working("x", new(-5))), w: 80, h: 24, want: []string{"░░░░░  -5%"}},
	}
	maps.Copy(tests, more)
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(tc.frame(t), tc.w, tc.h, plain)
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

// cardWidths measures the cards on the first row of cards: every top border,
// "╭ … ╮", on the frame's first line that has one.
func cardWidths(t *testing.T, frame string) []int {
	t.Helper()
	for line := range strings.SplitSeq(frame, "\n") {
		tops := regexp.MustCompile(`╭[^╮]*╮`).FindAllString(line, -1)
		if len(tops) == 0 {
			continue
		}
		widths := make([]int, len(tops))
		for i, top := range tops {
			widths[i] = ansi.StringWidth(top)
		}
		return widths
	}
	require.Fail(t, "no card on screen")
	return nil
}

// TestCardsStretchToFillTheWidth: the column count comes from the cards'
// natural width; the cards then share the terminal's width, one cell apart,
// at most 40 wide each. A row never has more columns than there are cards.
func TestCardsStretchToFillTheWidth(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name      string
		w, h      int
		cols, wid int
	}{
		"full at 120: three columns of 39":        {name: "full", w: 120, h: 40, cols: 3, wid: 39},
		"full at 80: two columns of 39":           {name: "full", w: 80, h: 24, cols: 2, wid: 39},
		"full at 60: one column, capped at 40":    {name: "full", w: 60, h: 40, cols: 1, wid: 40},
		"full at 200: six columns of 32":          {name: "full", w: 200, h: 40, cols: 6, wid: 32},
		"full at 250: no more columns than cards": {name: "full", w: 250, h: 40, cols: 6, wid: 40},
		"crowd at 80: a lone card caps at 40":     {name: "crowd", w: 80, h: 24, cols: 1, wid: 40},
		"blocked at 26: the minimum card":         {name: "blocked", w: 26, h: 40, cols: 1, wid: 26},
		"full at 20: narrower than any card":      {name: "full", w: 20, h: 40, cols: 1, wid: 20},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, tc.name), Now: at}, tc.w, tc.h, plain)
			widths := cardWidths(t, out)
			assert.Len(t, widths, tc.cols)
			for _, w := range widths {
				assert.Equal(t, tc.wid, w)
			}
		})
	}
}

// TestRenderClock: the header shows the frame's clock as HH:MM in the
// clock's own location, whatever that is.
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
			header, _, _ := strings.Cut(out, "\n")
			assert.True(t, strings.HasSuffix(header, tc.want), "%q", header)
		})
	}
}

// TestRenderIgnoresAnInvalidSelection: a selection that names no agent draws
// no highlight and still a whole frame, never a panic.
func TestRenderIgnoresAnInvalidSelection(t *testing.T) {
	t.Parallel()

	tests := map[string]dashboard.Selection{
		"a card past the last":   {Card: 99},
		"an agent past the last": {Agent: 99},
		"a negative card":        {Card: -1},
		"a negative agent":       {Agent: -1},
		"a huge card":            {Card: 1 << 62},
	}
	for name, sel := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Selection: sel, Now: at}, 80, 24, dashboard.Options{Profile: colorprofile.ASCII})
			lines := strings.Split(out, "\n")
			require.Len(t, lines, 24)
			for _, l := range lines {
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
// in the card and on the boss line, under a TrueColor profile.
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
			boss := a
			boss.Name = "boss"
			snap := &snapshot.Snapshot{V: 1, Team: "t", Boss: &boss, Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{a}}}}
			opts := dashboard.Options{Profile: colorprofile.TrueColor, Thresholds: tc.th}

			out := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at}, 80, 24, opts)
			pct := regexp.MustCompile(regexp.QuoteMeta(truecolor(tc.want)) + `[0-9;]*m\s*` + fmt.Sprint(tc.ctx) + `%`)
			assert.Len(t, pct.FindAllString(out, -1), 2, "the card and the boss line each colour %d%% %s", tc.ctx, tc.want)
		})
	}
}

// TestRenderHeaderCounts: working, idle and blocked are always counted; done,
// ready and unknown only when an agent has them. The boss is not one of the
// team's N agents.
func TestRenderHeaderCounts(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name string
		w    int
		want string
		not  []string
	}{
		"full":    {name: "full", w: 120, want: "◆ agentisan · 15 agents   ◐6 working ●4 idle ⚠1 blocked ✔2 done ⟳1 ready ?1 unknown"},
		"blocked": {name: "blocked", w: 120, want: "◆ assay · 3 agents   ◐2 working ●0 idle ⚠1 blocked", not: []string{"done", "ready", "unknown"}},
		"empty":   {name: "empty", w: 120, want: "◆ agentisan · 0 agents   ◐0 working ●0 idle ⚠0 blocked"},
		// Too narrow for the labels: the counts drop them rather than cut one.
		"full, narrow": {name: "full", w: 80, want: "◆ agentisan · 15 agents   ◐6 ●4 ⚠1 ✔2 ⟳1 ?1 ", not: []string{"working", "…"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, tc.name), Now: at}, tc.w, 40, plain)
			header, _, _ := strings.Cut(out, "\n")
			assert.Contains(t, header, tc.want)
			assert.True(t, strings.HasSuffix(header, "17:19"), "the clock is right-aligned: %q", header)
			for _, s := range tc.not {
				assert.NotContains(t, header, s)
			}
		})
	}
}

// TestRenderStatusIcons: each status has its own icon in the card.
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
			assert.Contains(t, out, "│"+icon+" a1 ")
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

// TestRenderSelectionIsReverseVideo: the selected agent's first line, and
// only that line, is drawn in reverse video.
func TestRenderSelectionIsReverseVideo(t *testing.T) {
	t.Parallel()

	f := dashboard.Frame{Snapshot: fixture(t, "full"), Selection: dashboard.Selection{Card: 0, Agent: 1}, Now: at}
	line := selectedLine(t, dashboard.Render(f, 80, 24, dashboard.Options{Profile: colorprofile.ASCII}))
	assert.Contains(t, line, "◐ pee02")
	assert.NotContains(t, line, "J1 re-queued", "the second line of the agent is not reversed")
}

func TestRenderFooter(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		frame dashboard.Frame
		want  string
	}{
		"hints alone": {frame: dashboard.Frame{Now: at}, want: " ←↑↓→ move · enter focus · ? help"},
		"a note leads the hints": {
			frame: dashboard.Frame{Now: at, Note: "focus off"},
			want:  " focus off · ←↑↓→ move · enter focus · ? help",
		},
		"errors lead the note": {
			frame: dashboard.Frame{Now: at, Note: "focus off", Errors: []error{errors.New("e1"), nil, errors.New("e2")}},
			want:  " ✖ e1 · ✖ e2 · focus off · ←↑↓→ move · enter focus · ? help",
		},
		"a long error is cut to one line": {
			frame: dashboard.Frame{Now: at, Errors: []error{errors.New(strings.Repeat("x", 200))}},
			want:  " ✖ " + strings.Repeat("x", 75) + "…",
		},
		"an error's newlines stay on one line": {
			frame: dashboard.Frame{Now: at, Errors: []error{errors.New("a\nb")}},
			want:  " ✖ a b · ←↑↓→ move · enter focus · ? help",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lines := strings.Split(dashboard.Render(tc.frame, 80, 24, plain), "\n")
			assert.Equal(t, tc.want, lines[len(lines)-1])
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

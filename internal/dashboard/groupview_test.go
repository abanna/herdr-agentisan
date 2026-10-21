package dashboard_test

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// polling is config polling every millisecond, for tests that poll.
func polling(src snapshot.Source, f dashboard.Focuser) dashboard.Config {
	cfg := config(src, f)
	cfg.Interval = time.Millisecond
	return cfg
}

// digit is the key press of a digit, as a terminal sends it.
func digit(d rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: d, Text: string(d)}
}

// TestGroupViewGolden pins the group view's frames (ADR-001 A37): one box
// per agent, one column, or two from 110 cells; a ready agent's badge, an
// unnamed shell, an agent with no stage and no model, a stage wrapped to two
// lines and one cut after them; and a group with no agents. Regenerate with
// `go test ./internal/dashboard/ -update`, then read every changed golden.
func TestGroupViewGolden(t *testing.T) {
	t.Parallel()

	view := func(fix, group string, sel dashboard.Selection) func(t *testing.T) dashboard.Frame {
		return func(t *testing.T) dashboard.Frame {
			t.Helper()
			return dashboard.Frame{Snapshot: fixture(t, fix), Selection: sel, Now: at, Btop: true, Group: group}
		}
	}
	first := dashboard.Selection{}
	tests := map[string]struct {
		frame func(t *testing.T) dashboard.Frame
		w, h  int
	}{
		"coders_80x24":  {frame: view("group", "coders", first), w: 80, h: 24},
		"coders_120x40": {frame: view("group", "coders", first), w: 120, h: 40},
		"coders_60x20":  {frame: view("group", "coders", first), w: 60, h: 20},
		// The last agent is selected: the boxes scroll to show it whole.
		"coders_selected_pee06_80x24": {frame: view("group", "coders", dashboard.Selection{Agent: 5}), w: 80, h: 24},
		"empty_group_80x24":           {frame: view("group", "research", first), w: 80, h: 24},
		// No description: the boxes start under the title.
		"qa_80x24": {frame: view("group", "qa", dashboard.Selection{Group: 1}), w: 80, h: 24},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			golden.RequireEqual(t, dashboard.Render(tc.frame(t), tc.w, tc.h, plain)+"\n")
		})
	}
}

// TestGroupViewFillsTheFrameExactly sweeps every group of every fixture in
// its view, as TestRenderFillsTheFrameExactly sweeps the overview: exactly h
// lines, none wider than w, none ending in a space, with the selection on
// the group's first and last agent, with and without help.
func TestGroupViewFillsTheFrameExactly(t *testing.T) {
	t.Parallel()

	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snap := fixture(t, name)
			for g, grp := range snap.Groups {
				sels := []dashboard.Selection{{Group: g}, {Group: g, Agent: max(0, len(grp.Agents)-1)}}
				for _, w := range widths {
					for _, h := range heights {
						for _, sel := range sels {
							for _, on := range []bool{false, true} {
								f := dashboard.Frame{
									Snapshot: snap, Selection: sel, Now: at, Help: on, Btop: on, Group: grp.Name,
									Note: "focus off", Errors: []error{errors.New(strings.Repeat("a long error ", 20))},
								}
								label := fmt.Sprintf("%s %dx%d sel=%+v help,btop=%v", grp.Name, w, h, sel, on)
								if !checkFrame(t, dashboard.Render(f, w, h, plain), w, h, label) {
									return
								}
							}
						}
					}
				}
				for _, size := range huge {
					f := dashboard.Frame{Snapshot: snap, Selection: sels[1], Now: at, Help: true, Btop: true, Group: grp.Name}
					checkFrame(t, dashboard.Render(f, size[0], size[1], plain), size[0], size[1], fmt.Sprint(grp.Name, size))
				}
			}
		})
	}
}

// TestGroupViewContents: what each box says, from the snapshot alone.
func TestGroupViewContents(t *testing.T) {
	t.Parallel()

	frame := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "group"), Now: at, Group: "coders"}, 80, 60, plain)

	assert.Contains(t, lineWith(t, frame, "◀ back"), "CODERS · 6", "the title is the overview's group title")
	assert.Contains(t, frame, " write the code", "the description sits under the title")
	assert.Contains(t, lineWith(t, frame, "◐ pee01 · working"), "╭─", "a box's title is its top border")
	assert.Contains(t, lineWith(t, frame, "⟳ pee02 · ready"), "⟳ READY ─╮", "a ready agent's badge is at the top right")
	assert.Contains(t, frame, "opus  ctx ▓▓▓▓░░░░░░ 38%")
	assert.Contains(t, frame, "│ ctx ▓▓░░░░░░░░ 22%", "no model reported (G1): the gauge leads")
	assert.Contains(t, frame, "│ ctx ░░░░░░░░░░ --", "no ctx reported: an empty gauge")
	assert.Contains(t, frame, "in stage 12m", "time in stage, from stage_started_at")
	assert.Contains(t, frame, "in stage 4m · idle 3m", "and at rest, from idle_since")
	assert.Contains(t, frame, "│ idle 39m ", "an idle agent with no stage: its rest alone")
	assert.Contains(t, frame, "pane w2:p3 · tab w2:t3", "the ids, an unnamed shell's included")
	assert.Equal(t, 2, strings.Count(frame, "no stage reported"), "a stage not reported shows as such")
	assert.NotContains(t, frame, "focus", "no line reads a screen: there is nothing like a title or a tail")

	// The stage wraps at words to two lines, and the rest is cut with "…".
	assert.Contains(t, frame, "│ precheck: tdaddy impact on the stale pane id resolver, then the 7b")
	assert.Contains(t, frame, "│ enumeration for every changed function, then stage 8, then the commit and t… │")
	assert.Contains(t, frame, "│ waiting on boss: the review loop wants a size waiver for the generation ")
}

// TestGroupViewTwoColumns: from 110 cells the boxes go two to a row, in the
// group's order, left to right; a group of one stays one column.
func TestGroupViewTwoColumns(t *testing.T) {
	t.Parallel()

	snap := fixture(t, "group")
	wide := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at, Group: "coders"}, 110, 40, plain)
	row := lineWith(t, wide, "pee01 · working")
	assert.Contains(t, row, "pee02 · ready", "pee01 and pee02 share a row")
	x1, _ := locate(t, wide, "pee01")
	x2, _ := locate(t, wide, "pee02")
	assert.Less(t, x1, x2)

	narrow := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at, Group: "coders"}, 109, 40, plain)
	assert.NotContains(t, lineWith(t, narrow, "pee01 · working"), "pee02")

	one := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at, Group: "qa", Selection: dashboard.Selection{Group: 1}}, 200, 40, plain)
	assert.Contains(t, lineWith(t, one, "qa · working"), strings.Repeat("─", 150), "one agent takes the whole width")
}

// TestGroupViewSelectionIsReverseVideo: the selected box's title is in
// reverse video, and nothing else is.
func TestGroupViewSelectionIsReverseVideo(t *testing.T) {
	t.Parallel()

	f := dashboard.Frame{Snapshot: fixture(t, "group"), Now: at, Group: "coders", Selection: dashboard.Selection{Agent: 3}}
	assert.Contains(t, selectedLine(t, dashboard.Render(f, 80, 60, attrs)), "● pee04 · idle")
}

// TestGroupViewFallsBackToTheOverview: a group the snapshot does not have,
// or a query being typed, draws the overview.
func TestGroupViewFallsBackToTheOverview(t *testing.T) {
	t.Parallel()

	snap := fixture(t, "group")
	overview := dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at}, 80, 24, plain)
	assert.Equal(t, overview, dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at, Group: "gone"}, 80, 24, plain))
	searching := dashboard.Frame{Snapshot: snap, Now: at, Search: true}
	inView := searching
	inView.Group = "coders"
	assert.Equal(t, dashboard.Render(searching, 80, 24, plain), dashboard.Render(inView, 80, 24, plain))
	assert.NotContains(t, overview, "◀ back")
}

// TestGroupViewFooter: the view's footer names its own keys, and counts the
// agents whose boxes are out of view.
func TestGroupViewFooter(t *testing.T) {
	t.Parallel()

	snap := fixture(t, "group")
	top := lines(dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at, Group: "coders"}, 80, 24, plain))
	assert.True(t, strings.HasPrefix(top[23], " enter jump · ↑↓ move · esc back · ? help · q quit "), top[23])
	assert.True(t, strings.HasSuffix(top[23], " ↓4"), "two boxes fit, four are below: %q", top[23])

	bottom := lines(dashboard.Render(dashboard.Frame{
		Snapshot: snap, Now: at, Group: "coders", Selection: dashboard.Selection{Agent: 5}, Btop: true,
	}, 80, 24, plain))
	assert.True(t, strings.HasPrefix(bottom[23], " enter jump · ↑↓ move · esc back · ctrl-b m btop · ? help · q quit "), bottom[23])
	assert.True(t, strings.HasSuffix(bottom[23], " ↑4"), "the last two show, four are above: %q", bottom[23])
}

// TestDigitsOpenAGroup: 1-9 open the n-th group's view in the snapshot's
// order. The selection stays when it is in that group and moves to its first
// agent otherwise; a digit past the last group does nothing.
func TestDigitsOpenAGroup(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	early, _ := update(t, dashboard.New(t.Context(), config(src, nil)), digit('1'))
	assert.Empty(t, early.Frame().Group, "before the first snapshot there is no group to open")

	m := started(t, config(src, nil), 80, 24)
	m, _ = update(t, m, keyDown) // pee02

	m, _ = update(t, m, digit('1'))
	assert.Equal(t, "coders", m.Frame().Group)
	assert.Equal(t, "pee02", selected(t, m), "the selection is in the group: it stays")

	m, _ = update(t, m, digit('3'))
	assert.Equal(t, "qa", m.Frame().Group, "a digit in a view opens another")
	assert.Equal(t, "qa", selected(t, m), "the group's first agent")
	assert.Contains(t, m.View().Content, "◀ back  QA · 1")

	m, _ = update(t, m, digit('9'))
	assert.Equal(t, "qa", m.Frame().Group, "full has six groups: 9 does nothing")
	m, _ = update(t, m, digit('0'))
	assert.Equal(t, "qa", m.Frame().Group, "0 is no group")
}

// TestLeavingTheGroupView: Esc and ← return to the overview with the
// selection where the view left it; Esc closes the help first.
func TestLeavingTheGroupView(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, nil), 80, 24)

	for name, leave := range map[string]tea.KeyPressMsg{"esc": keyEsc, "left": keyLeft} {
		v, _ := update(t, m, digit('2'))
		v, _ = update(t, v, keyDown)
		require.Equal(t, "precheck2", selected(t, v))
		v, _ = update(t, v, leave)
		assert.Empty(t, v.Frame().Group, name)
		assert.Equal(t, "precheck2", selected(t, v), "%s keeps the selection", name)
		assert.Contains(t, v.View().Content, "╭─ CODERS", "%s: the overview is back", name)
	}

	v, _ := update(t, m, digit('2'))
	v, _ = update(t, v, keyHelp)
	v, _ = update(t, v, keyEsc)
	assert.False(t, v.Frame().Help, "esc closes the help")
	assert.Equal(t, "precheck", v.Frame().Group, "and only the help")
	v, _ = update(t, v, keyEsc)
	assert.Empty(t, v.Frame().Group)
}

// TestArrowsStayInTheGroup: in a view, up and down walk the group's agents
// and stop at its ends, where on the overview they cross into the next
// group.
func TestArrowsStayInTheGroup(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, nil), 80, 24)
	m, _ = update(t, m, digit('2'))
	for _, k := range keys(repeat(keyDown, 5)) {
		m, _ = update(t, m, k)
	}
	assert.Equal(t, "precheck2", selected(t, m))
	for _, k := range keys(repeat(keyUp, 5)) {
		m, _ = update(t, m, k)
	}
	assert.Equal(t, "precheck", selected(t, m))
	m, _ = update(t, m, keyRight)
	assert.Equal(t, "precheck", m.Frame().Group, "right does nothing")
}

// TestClicksInTheGroupView: a click on a group's title opens its view, a
// click on a box jumps to that agent, and a click on ◀ back returns.
func TestClicksInTheGroupView(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "group"))}}
	m := started(t, config(src, rec), 80, 24)
	click := func(m dashboard.Model, text string, dx int) (dashboard.Model, tea.Cmd) {
		t.Helper()
		x, y := locate(t, m.View().Content, text)
		return update(t, m, tea.MouseClickMsg{X: x + dx, Y: y, Button: tea.MouseLeft})
	}

	m, cmd := click(m, "QA · 1", 0)
	assert.Nil(t, cmd)
	assert.Equal(t, "qa", m.Frame().Group, "a click on a title opens its group")

	m, _ = click(m, "◀ back", 2)
	assert.Empty(t, m.Frame().Group, "◀ back returns")

	m, _ = click(m, "CODERS", -3)
	require.Equal(t, "coders", m.Frame().Group, "anywhere on the title row")
	m, cmd = click(m, "pee02 · ready", 20)
	assert.Equal(t, "pee02", selected(t, m))
	m = run(t, m, cmd)
	m, cmd = click(m, "NERD-5255", 0)
	m = run(t, m, cmd)
	assert.Equal(t, []string{"pee02", "pee01"}, rec.names(), "a click anywhere in a box jumps")

	m, cmd = click(m, "◀ back", -1)
	assert.Nil(t, cmd)
	assert.Equal(t, "coders", m.Frame().Group, "the space beside ◀ back is not it")
	m, cmd = click(m, "agentisan", 0)
	assert.Nil(t, cmd)
	assert.Equal(t, "coders", m.Frame().Group, "the header does nothing new")
}

// TestAClickOnATitleEndsTheSearch: during a search a click on a group's
// title opens that group, by name, with every agent back.
func TestAClickOnATitleEndsTheSearch(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, nil), 80, 24)
	for _, k := range []tea.KeyPressMsg{{Code: '/', Text: "/"}, digit('q'), digit('a')} {
		m, _ = update(t, m, k)
	}
	require.True(t, m.Frame().Search)
	x, y := locate(t, m.View().Content, "QA · 1")
	m, _ = update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.False(t, m.Frame().Search)
	assert.Equal(t, "qa", m.Frame().Group)
	assert.Equal(t, "qa", selected(t, m))
}

// TestSearchFromTheGroupView: / leaves the view and starts the overview's
// search, and while it runs digits are query text, not groups.
func TestSearchFromTheGroupView(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, nil), 80, 24)
	m, _ = update(t, m, digit('3'))
	m, _ = update(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	assert.True(t, m.Frame().Search)
	assert.Empty(t, m.Frame().Group)
	m, _ = update(t, m, digit('2'))
	assert.Equal(t, "2", m.Frame().Query)
	assert.Empty(t, m.Frame().Group)
}

// TestGroupViewFollowsRefreshes: the view follows its group by name across
// snapshots, and the selection its agent. A group that is gone returns to
// the overview with a note, which the next key clears; a selected agent
// gone from the group leaves the selection in its place.
func TestGroupViewFollowsRefreshes(t *testing.T) {
	t.Parallel()

	base := fixture(t, "group")
	moved := *base
	moved.Groups = slices.Clone(base.Groups)
	slices.Reverse(moved.Groups) // research, qa, coders
	coders := moved.Groups[2]
	coders.Agents = slices.Concat(coders.Agents[3:], coders.Agents[:3]) // pee04 first
	moved.Groups[2] = coders
	shrunk := moved
	shrunk.Groups = slices.Clone(moved.Groups)
	shrunk.Groups[2].Agents = moved.Groups[2].Agents[:2] // pee04 and pee05
	gone := moved
	gone.Groups = moved.Groups[:2]

	src := &script{steps: []func() (snapshot.Snapshot, error){
		serve(base), serve(&moved), serve(&shrunk), serve(&gone), serve(&gone),
	}}
	m, tick := startedPolling(t, polling(src, nil), 80, 24)
	m, _ = update(t, m, digit('1'))
	m, _ = update(t, m, keyDown)
	require.Equal(t, "pee02", selected(t, m))

	m, tick = pollOnce(t, m, tick)
	assert.Equal(t, "coders", m.Frame().Group, "the view follows the group by name")
	assert.Equal(t, dashboard.Selection{Group: 2, Agent: 4}, m.Frame().Selection, "and the selection its agent")
	assert.Equal(t, "pee02", selected(t, m))

	m, tick = pollOnce(t, m, tick)
	assert.Equal(t, "coders", m.Frame().Group)
	assert.Equal(t, "pee05", selected(t, m), "pee02 is gone: its place, clamped to the group")

	m, _ = pollOnce(t, m, tick)
	assert.Empty(t, m.Frame().Group, "the group is gone: back to the overview")
	assert.Contains(t, m.Frame().Note, `group "coders" is gone`)
	assert.Contains(t, m.View().Content, `group "coders" is gone`)
	m, _ = update(t, m, keyDown)
	assert.NotContains(t, m.Frame().Note, "gone", "the next key clears the note")
}

// TestNoTeamHereClosesTheView: when the source says there is no team here,
// the view closes with the board, and no note says a group is gone.
func TestNoTeamHereClosesTheView(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){
		serve(fixture(t, "group")), fail(fmt.Errorf("w9: %w", dashboard.ErrNoTeamHere)),
	}}
	m, tick := startedPolling(t, polling(src, nil), 80, 24)
	m, _ = update(t, m, digit('2'))
	require.Equal(t, "qa", m.Frame().Group)
	m, _ = pollOnce(t, m, tick)
	assert.Empty(t, m.Frame().Group)
	assert.NotContains(t, m.Frame().Note, "gone")
}

// TestAnErrorKeepsTheGroupView: any other source error keeps the view and
// its last frame, with the error in the footer.
func TestAnErrorKeepsTheGroupView(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){
		serve(fixture(t, "group")), fail(snapshot.ErrUnavailable),
	}}
	m, tick := startedPolling(t, polling(src, nil), 80, 24)
	m, _ = update(t, m, digit('1'))
	m, _ = pollOnce(t, m, tick)
	assert.Equal(t, "coders", m.Frame().Group)
	assert.Contains(t, m.View().Content, "◀ back")
	assert.Contains(t, lines(ansi.Strip(m.View().Content))[23], "✖")
}

// TestGroupViewScrollsToTheSelection: moving down a group taller than the
// screen scrolls the boxes so the selected one is whole on screen.
func TestGroupViewScrollsToTheSelection(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "group"))}}
	m := started(t, config(src, nil), 80, 24)
	m, _ = update(t, m, digit('1'))
	for range 5 {
		m, _ = update(t, m, keyDown)
	}
	require.Equal(t, "pee06", selected(t, m))
	frame := ansi.Strip(m.View().Content)
	assert.Contains(t, frame, "pee06 · done")
	assert.Contains(t, frame, "pane w2:p6 · tab w2:t6", "the box is whole: its last line shows")
	assert.NotContains(t, frame, "pee01 · working", "the first boxes scrolled away")

	m, _ = update(t, m, keyLeft)
	assert.Contains(t, ansi.Strip(m.View().Content), "pee06", "the overview shows the selection too")
}

// TestGroupViewReadsNoScreen (ADR-001 D4, A37): opening a group, moving in
// it and jumping from it never ask herdr for a pane's screen; the only calls
// are the jump's own.
func TestGroupViewReadsNoScreen(t *testing.T) {
	t.Parallel()

	srv := herdrtest.Start(t, focusHerdr(false))
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "group"))}}
	m, tick := startedPolling(t, polling(src, dashboard.HerdrFocuser{Client: herdr.Client{SocketPath: srv.Path}}), 120, 40)
	m, _ = update(t, m, digit('1'))
	m, _ = update(t, m, keyDown)
	m, _ = pollOnce(t, m, tick)
	_, cmd := update(t, m, keyEnter)
	run(t, m, cmd)

	var methods []string
	for _, r := range requests(srv) {
		methods = append(methods, r.Method)
	}
	assert.Equal(t, []string{"agent.focus", "pane.zoom"}, methods)
	assert.NotContains(t, methods, "pane.read")
	assert.NotContains(t, methods, "agent.read")
}

// TestGroupViewClicksOnNothing: in a view, a click between the two
// columns, below the last box, on the empty half of a last row, or on the
// title line beside ◀ back selects and focuses nothing.
func TestGroupViewClicksOnNothing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		fixture string
		key     rune   // the group's digit; '1' when 0
		group   string // its name; "coders" when ""
		x, y    func(frame string) (int, int)
	}{
		"between the columns": {fixture: "group", x: func(frame string) (int, int) {
			// "╭─ ⟳ " precedes the name, and two cells precede the box.
			x, y := locate(t, frame, "pee02 · ready")
			return x - 6, y + 2
		}},
		"below the last box": {fixture: "group", x: func(frame string) (int, int) {
			_, y := locate(t, frame, "pane w2:p6")
			return 10, y + 3
		}},
		"the empty half of the last row": {fixture: "full", x: func(frame string) (int, int) {
			_, y := locate(t, frame, "pee09 · working")
			return 100, y + 2
		}},
		"the title beside ◀ back": {fixture: "group", x: func(frame string) (int, int) {
			x, y := locate(t, frame, "CODERS ·")
			return x + 3, y
		}},
		"a group with no agents": {fixture: "group", key: '3', group: "research", x: func(frame string) (int, int) {
			x, y := locate(t, frame, "no agents")
			return x, y
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, tc.fixture))}}
			m := started(t, config(src, rec), 120, 60)
			key, group := cmp.Or(tc.key, '1'), cmp.Or(tc.group, "coders")
			m, _ = update(t, m, digit(key))
			require.Equal(t, group, m.Frame().Group)
			before := m.Frame().Selection
			x, y := tc.x(ansi.Strip(m.View().Content))
			m, cmd := update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			assert.Nil(t, cmd)
			assert.Equal(t, before, m.Frame().Selection)
			assert.Equal(t, group, m.Frame().Group)
			assert.Empty(t, rec.names())
		})
	}
}

// TestGroupViewMatchesNamesExactly: a group is opened, followed and missed
// by its name exactly as the snapshot spells it: names that differ only in
// case, or in Unicode normalization, are different groups; non-ASCII and
// white space are kept; and a name that cannot draw on one line is drawn
// cleaned, in the title and in the note that says it is gone.
func TestGroupViewMatchesNamesExactly(t *testing.T) {
	t.Parallel()

	// Two teams, as digits reach nine groups: spellings, then encodings.
	for _, names := range [][]string{
		{"coders", "Coders", "caf\u00e9", "cafe\u0301", "a b\nc", "日本語"},
		{"bad\xffname", "cut\xe2\x82", "\ufeffbom", "nul\x00name", "cr\r\nlf", "esc\x1b[2J"},
	} {
		matchNamesExactly(t, names)
	}
}

// matchNamesExactly runs TestGroupViewMatchesNamesExactly over one team
// whose groups are names, at most nine.
func matchNamesExactly(t *testing.T, names []string) {
	t.Helper()
	team := snapshot.Snapshot{V: 1, At: at, Team: "t"}
	for i, n := range names {
		team.Groups = append(team.Groups, snapshot.Group{Name: n, Agents: []snapshot.Agent{
			{Name: fmt.Sprintf("agent%d", i), PaneID: fmt.Sprintf("w%d:p1", i), Status: snapshot.StatusIdle},
		}})
	}
	for i, n := range names {
		t.Run(fmt.Sprintf("%q", n), func(t *testing.T) {
			t.Parallel()
			reordered := team
			reordered.Groups = slices.Clone(team.Groups)
			slices.Reverse(reordered.Groups)
			without := reordered
			without.Groups = slices.DeleteFunc(slices.Clone(reordered.Groups), func(g snapshot.Group) bool { return g.Name == n })

			src := &script{steps: []func() (snapshot.Snapshot, error){serve(&team), serve(&reordered), serve(&without)}}
			m, tick := startedPolling(t, polling(src, nil), 80, 24)
			m, _ = update(t, m, digit(rune('1'+i)))
			require.Equal(t, n, m.Frame().Group)
			assert.Equal(t, fmt.Sprintf("agent%d", i), selected(t, m))
			frame := m.View().Content
			assert.Equal(t, 24, strings.Count(frame, "\n")+1, "the name cannot add a line")

			m, tick = pollOnce(t, m, tick)
			assert.Equal(t, n, m.Frame().Group, "followed by its exact name")
			assert.Equal(t, fmt.Sprintf("agent%d", i), selected(t, m))

			m, _ = pollOnce(t, m, tick)
			assert.Empty(t, m.Frame().Group, "no other spelling stands in for it")
			assert.Contains(t, m.Frame().Note, "is gone")
			assert.NotContains(t, m.Frame().Note, "\n")
			assert.Equal(t, 24, strings.Count(m.View().Content, "\n")+1)
		})
	}
}

// TestGroupViewDrawsAnyText: whatever an agent reports, its box stays in
// the frame (h lines, none wider than w, none ending in a space) and draws
// no control byte: invalid and truncated UTF-8, NUL, escape sequences, CRLF,
// a byte-order mark, wide and combining characters, a word longer than the
// box, and a stage of white space only, which is no stage.
func TestGroupViewDrawsAnyText(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"invalid UTF-8":             "go\xffreview",
		"a truncated multibyte":     "review \xe2\x82",
		"a NUL":                     "go\x00review",
		"an escape sequence":        "go\x1b[2Jreview\x1b]0;title\x07",
		"CRLF and a lone CR":        "line one\r\nline two\rthree",
		"a byte-order mark":         "\ufeffreview",
		"wide characters":           strings.Repeat("レビュー中です", 12),
		"combining marks":           strings.Repeat("cafe\u0301 ", 30),
		"one word wider than a box": strings.Repeat("x", 300),
		"white space only":          " \t\r\n ",
	}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			agent := snapshot.Agent{
				Name: text, Unnamed: true, PaneID: text, TabID: text, Status: snapshot.StatusWorking,
				Model: text, Item: text, Stage: text,
			}
			s := &snapshot.Snapshot{V: 1, At: at, Team: "t", Groups: []snapshot.Group{
				{Name: "g", Description: text, Agents: []snapshot.Agent{agent, agent, agent}},
			}}
			for _, size := range [][2]int{{80, 24}, {120, 40}, {30, 12}, {12, 6}} {
				w, h := size[0], size[1]
				frame := dashboard.Render(dashboard.Frame{Snapshot: s, Now: at, Group: "g"}, w, h, plain)
				label := fmt.Sprintf("%dx%d", w, h)
				if !checkFrame(t, frame, w, h, label) {
					return
				}
				for _, r := range strings.ReplaceAll(frame, "\n", "") {
					require.False(t, r < 0x20 || r == 0x7f, "%s: a control character %U is drawn", label, r)
				}
			}
			wide := dashboard.Render(dashboard.Frame{Snapshot: s, Now: at, Group: "g"}, 80, 60, plain)
			if strings.TrimSpace(text) == "" {
				assert.Contains(t, wide, "no stage reported", "white space is no stage")
			} else {
				assert.NotContains(t, wide, "no stage reported")
			}
		})
	}
}

// TestGroupViewTimes: "in stage" and "idle" are drawn only when known: a
// time not reported, or ahead of the frame's clock, draws nothing; a long
// rest is capped like every Elapsed.
func TestGroupViewTimes(t *testing.T) {
	t.Parallel()

	ago := func(d time.Duration) *time.Time { v := at.Add(-d); return &v }
	tests := map[string]struct {
		stage, idle *time.Time
		want        string // the times line, "" for none
	}{
		"neither":                   {want: ""},
		"in stage only":             {stage: ago(12 * time.Minute), want: "in stage 12m"},
		"idle only":                 {idle: ago(3 * time.Minute), want: "idle 3m"},
		"both":                      {stage: ago(time.Hour), idle: ago(90 * time.Second), want: "in stage 1h00m · idle 1m"},
		"a rest ahead of the clock": {stage: ago(time.Minute), idle: ago(-time.Minute), want: "in stage 1m"},
		"a rest of 100 days":        {idle: ago(100 * 24 * time.Hour), want: "idle >99d"},
		"at the clock":              {idle: ago(0), want: "idle 0m"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &snapshot.Snapshot{V: 1, At: at, Team: "t", Groups: []snapshot.Group{{Name: "g", Agents: []snapshot.Agent{
				{Name: "a", PaneID: "w1:p1", Status: snapshot.StatusIdle, Stage: "s", StageStartedAt: tc.stage, IdleSince: tc.idle},
			}}}}
			ls := lines(dashboard.Render(dashboard.Frame{Snapshot: s, Now: at, Group: "g"}, 80, 24, plain))
			_, y := locate(t, strings.Join(ls, "\n"), "pane w1:p1")
			assert.Equal(t, strings.TrimRight("│ "+tc.want, " "), strings.TrimRight(strings.TrimSuffix(strings.TrimRight(ls[y-1], " "), "│"), " "))
		})
	}
}

// TestGroupViewClampsItsInputs: a scroll before the first line or past the
// last, and a selection outside the group, draw a frame as a valid one
// would: the scroll clamped, and no box selected.
func TestGroupViewClampsItsInputs(t *testing.T) {
	t.Parallel()

	snap := fixture(t, "group")
	render := func(sel dashboard.Selection, scroll int) string {
		return dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at, Group: "coders", Selection: sel, Scroll: scroll}, 80, 24, attrs)
	}
	none := dashboard.Selection{Group: 1}
	top, bottom := render(none, 0), render(none, 1<<40)
	assert.Contains(t, ansi.Strip(top), "pee01 · working")
	assert.Contains(t, ansi.Strip(bottom), "pee06 · done", "past the end: the last boxes")
	assert.NotContains(t, ansi.Strip(bottom), "pee01 · working")

	tests := map[string]struct {
		sel    dashboard.Selection
		scroll int
		want   string
	}{
		"a negative scroll":         {sel: none, scroll: -5, want: top},
		"the largest scroll":        {sel: none, scroll: math.MaxInt, want: bottom},
		"a negative agent":          {sel: dashboard.Selection{Agent: -1}, want: top},
		"an agent past the group":   {sel: dashboard.Selection{Agent: 6}, want: top},
		"another group's agent":     {sel: dashboard.Selection{Group: 1}, want: top},
		"a group past the snapshot": {sel: dashboard.Selection{Group: 9}, want: top},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, render(tc.sel, tc.scroll))
		})
	}
}

// TestGroupViewStageFillsTwoLines: a stage that fills its two lines
// exactly is drawn whole; one character more ends the second in "…".
func TestGroupViewStageFillsTwoLines(t *testing.T) {
	t.Parallel()

	const inner = 76 // an 80-cell box's text
	a, b := strings.Repeat("a", inner), strings.Repeat("b", inner)
	tests := map[string]struct {
		stage    string
		one, two string
	}{
		"exactly two lines":      {stage: a + " " + b, one: a, two: b},
		"one character more":     {stage: a + " " + b + "c", one: a, two: b[1:] + "…"},
		"one word more":          {stage: a + " " + b + " c", one: a, two: b[1:] + "…"},
		"exactly one line":       {stage: a, one: a},
		"one word past one line": {stage: a + " b", one: a, two: "b"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &snapshot.Snapshot{V: 1, At: at, Team: "t", Groups: []snapshot.Group{{Name: "g", Agents: []snapshot.Agent{
				{Name: "a", PaneID: "w1:p1", Status: snapshot.StatusWorking, Stage: tc.stage},
			}}}}
			ls := lines(dashboard.Render(dashboard.Frame{Snapshot: s, Now: at, Group: "g"}, 80, 24, plain))
			_, y := locate(t, strings.Join(ls, "\n"), "a · working")
			body := func(i int) string { return strings.TrimSpace(strings.Trim(ls[y+i], "│")) }
			assert.Equal(t, tc.one, body(3))
			assert.Equal(t, tc.two, body(4))
		})
	}
}

package dashboard_test

import (
	"fmt"
	"image/color"
	"math"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

func TestDashboardPaletteContrast(t *testing.T) {
	t.Parallel()

	luminance := func(c color.Color) float64 {
		r, g, b, _ := c.RGBA()
		linear := func(channel uint32) float64 {
			v := float64(channel) / 65535
			if v <= 0.04045 {
				return v / 12.92
			}
			return math.Pow((v+0.055)/1.055, 2.4)
		}
		return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
	}
	for i, fg := range dashboard.PaletteForTest() {
		_, _, _, alpha := fg.RGBA()
		for _, bg := range []string{"#1e1e2e", "#000000"} {
			light, dark := luminance(fg), luminance(lipgloss.Color(bg))
			ratio := (max(light, dark) + 0.05) / (min(light, dark) + 0.05)
			if alpha != 65535 || ratio < 4.5 {
				t.Errorf("text colour %d (%v) on %s has contrast %.2f:1, want opaque with at least 4.5:1", i, fg, bg, ratio)
			}
		}
	}
}

func TestSelectionWithoutColour(t *testing.T) {
	t.Parallel()

	profile := colorprofile.Env([]string{"TERM=xterm-256color", "NO_COLOR=1"})
	require.Equal(t, colorprofile.ASCII, profile)
	snap := fixture(t, "full")
	frame := dashboard.Frame{Snapshot: snap, Now: at, Selection: dashboard.Selection{Agent: 1}}
	opts := dashboard.Options{Profile: profile}
	check := func(out, prefix, selected, other string) {
		t.Helper()
		require.NotRegexp(t, `\x1b\[(?:[0-9;]*;)?(?:38|48)[;:]`, out)
		var marked []string
		for line := range strings.SplitSeq(ansi.Strip(out), "\n") {
			if strings.HasPrefix(line, prefix) {
				marked = append(marked, line)
			}
		}
		require.Len(t, marked, 1)
		require.Contains(t, marked[0], selected)
		require.NotContains(t, marked[0], other)
	}
	check(dashboard.Render(frame, 120, 40, opts), "│▸", "pee02", "pee01")
	frame.Group = "coders"
	check(dashboard.Render(frame, 55, 60, opts), "╭▸", "pee02", "pee01")
	frame.Board = dashboard.BoardFrame{
		Open: true, Ready: true, Expanded: true, Selected: 1,
		Posts: []daemon.BoardPostView{{Body: "unselected post"}, {Body: "selected topic\nextra line"}},
	}
	check(dashboard.Render(frame, 120, 40, opts), "│▸", "selected topic", "unselected post")

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(snap)}}
	model := started(t, config(src, &recorder{}), 120, 40)
	model, _ = update(t, model, tea.ColorProfileMsg{Profile: profile})
	check(model.View().Content, "│▸", "pee01", "pee02")
}

func TestSelectionUsesCoolBarInColour(t *testing.T) {
	t.Parallel()

	const background = "48;2;54;71;107"
	snap := fixture(t, "full")
	frame := dashboard.Frame{Snapshot: snap, Now: at, Selection: dashboard.Selection{Agent: 1}}
	opts := dashboard.Options{Profile: colorprofile.TrueColor}
	check := func(out, selected, other string) {
		t.Helper()
		var match, unselected string
		for line := range strings.SplitSeq(out, "\n") {
			plain := ansi.Strip(line)
			if strings.Contains(plain, selected) {
				match = line
			}
			if strings.Contains(plain, other) {
				unselected = line
			}
		}
		require.Contains(t, match, background)
		require.NotEmpty(t, unselected)
		require.NotContains(t, unselected, background)
	}
	check(dashboard.Render(frame, 120, 40, opts), "pee02", "pee01")
	frame.Group = "coders"
	check(dashboard.Render(frame, 55, 60, opts), "pee02", "pee01")
	frame.Board = dashboard.BoardFrame{
		Open: true, Ready: true, Expanded: true, Selected: 1,
		Posts: []daemon.BoardPostView{{Body: "earlier topic"}, {Body: "current topic\nextra body"}},
	}
	board := dashboard.Render(frame, 120, 40, opts)
	check(board, "current topic", "earlier topic")
	check(board, "extra body", "earlier topic")
}

func TestRenderWideFrameAllocations(t *testing.T) {
	frame := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at}
	const width, height = 8192, 16
	allocations := testing.AllocsPerRun(3, func() {
		dashboard.Render(frame, width, height, fullColour)
	})
	require.Less(t, allocations, float64(2600), "fitting lines should reach the writer without allocating copies of their segments")
}

func paletteHex(role int) string {
	r, g, b, _ := dashboard.PaletteForTest()[role].RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

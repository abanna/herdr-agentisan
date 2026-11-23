package dashboard_test

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

func contrastRatio(fg, bg color.Color) float64 {
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
	a, b := luminance(fg), luminance(bg)
	return (max(a, b) + 0.05) / (min(a, b) + 0.05)
}

func TestDashboardPaletteContrast(t *testing.T) {
	t.Parallel()

	check := func(fg, bg color.Color) {
		t.Helper()
		_, _, _, alpha := fg.RGBA()
		ratio := contrastRatio(fg, bg)
		if alpha != 65535 || ratio < 4.5 {
			t.Errorf("foreground %v on %v has contrast %.2f:1, want opaque with at least 4.5:1", fg, bg, ratio)
		}
	}
	for _, tc := range []struct {
		name   string
		light  bool
		scenes []string
	}{
		{name: "dark", scenes: []string{"#1e1e2e", "#000000"}},
		{name: "light", light: true, scenes: []string{"#ffffff", "#eaeaea"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bar, buttonInk, buttonFill := dashboard.SelectionColours(tc.light)
			for _, fg := range dashboard.PaletteForBackground(tc.light) {
				for _, bg := range tc.scenes {
					check(fg, lipgloss.Color(bg))
				}
				check(fg, bar)
			}
			check(buttonInk, buttonFill)
			if tc.light {
				for _, bg := range tc.scenes {
					check(dashboard.ProjectColour(true, "#ffffff"), lipgloss.Color(bg))
					check(dashboard.ProjectColour(true, "#bd93f9"), lipgloss.Color(bg))
				}
			}
		})
	}
}

func checkFrameGolden(t *testing.T, frame dashboard.Frame, w, h int) {
	t.Helper()
	golden.RequireEqual(t, dashboard.Render(frame, w, h, plain)+"\n")
	t.Run("light", func(t *testing.T) {
		// Quote keeps the complete coloured frame, including escape codes
		// and newlines, in a text file that Git can display and diff.
		golden.RequireEqual(t, strconv.Quote(dashboard.Render(frame, w, h, lightColour()))+"\n")
	})
}

func lightColour() dashboard.Options {
	return dashboard.Options{Profile: colorprofile.TrueColor, LightBackground: true}
}

func TestLightSelectionForeground(t *testing.T) {
	t.Parallel()

	frame := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Selection: dashboard.Selection{Agent: 1}}
	out := dashboard.Render(frame, 120, 40, lightColour())
	require.Regexp(t, "\x1b"+`\[[0-9;]*38;2;23;35;59;48;2;182;199;236mpee02`, out)
	require.NotContains(t, out, "38;2;238;242;255")
}

func TestModelUsesTerminalBackground(t *testing.T) {
	t.Parallel()

	m := dashboard.New(t.Context(), dashboard.Config{})
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Contains(t, m.View().Content, "38;2;190;203;227", "dark before a report")
	m, _ = update(t, m, tea.BackgroundColorMsg{})
	require.Contains(t, m.View().Content, "38;2;190;203;227", "dark after a nil colour report")
	m, _ = update(t, m, tea.BackgroundColorMsg{Color: lipgloss.Color("#eaeaea")})
	require.Contains(t, m.View().Content, "38;2;60;76;105", "light after a light report")
	m, _ = update(t, m, tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	require.Contains(t, m.View().Content, "38;5;239", "light uses the detected 256-colour profile")
	m, _ = update(t, m, tea.BackgroundColorMsg{Color: lipgloss.Color("#1e1e2e")})
	require.Contains(t, m.View().Content, "38;2;190;203;227", "dark after a dark report")
}

func TestTerminalBackgroundPalette(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		background string
		light      bool
		reason     string
	}{
		{background: "#0000ff"},
		{background: "#ff0000", light: true, reason: "pure red gives 3.92:1 with the light palette and 3.58:1 with the dark palette"},
		{background: "#ffffff", light: true},
		{background: "#eaeaea", light: true},
		{background: "#fbf1c7", light: true},
		{background: "#1e1e2e"},
	} {
		t.Run(tc.background, func(t *testing.T) {
			m := dashboard.New(t.Context(), dashboard.Config{})
			m, _ = update(t, m, tea.BackgroundColorMsg{Color: lipgloss.Color(tc.background)})
			opts := fullColour
			opts.LightBackground = tc.light
			require.Equal(t, dashboard.Render(m.Frame(), m.Width(), m.Height(), opts), m.View().Content, tc.reason)
		})
	}
}

func TestTerminalBackgroundChoosesHigherContrast(t *testing.T) {
	t.Parallel()

	m := dashboard.New(t.Context(), dashboard.Config{Options: fullColour})
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 40, Height: 4})
	darkFrame := m.View().Content
	lightFrame := dashboard.Render(m.Frame(), m.Width(), m.Height(), lightColour())
	require.NotEqual(t, darkFrame, lightFrame)
	darkText := dashboard.PaletteForBackground(false)[dashboard.PaletteText]
	lightText := dashboard.PaletteForBackground(true)[dashboard.PaletteText]
	channels := [...]uint8{0, 17, 34, 51, 68, 85, 102, 119, 136, 153, 170, 187, 204, 221, 238, 255}
	for _, r := range channels {
		for _, g := range channels {
			for _, b := range channels {
				background := color.RGBA{R: r, G: g, B: b, A: 255}
				m, _ = update(t, m, tea.BackgroundColorMsg{Color: background})
				chosen, other := contrastRatio(darkText, background), contrastRatio(lightText, background)
				switch got := m.View().Content; got {
				case lightFrame:
					chosen, other = other, chosen
				case darkFrame:
				default:
					t.Fatalf("unexpected palette on #%02x%02x%02x: %q", r, g, b, got)
				}
				require.GreaterOrEqualf(t, chosen, other, "ordinary text on #%02x%02x%02x", r, g, b)
			}
		}
	}
}

func TestTerminalBackgroundContrastTieKeepsDark(t *testing.T) {
	t.Parallel()

	// Adding 0.05 gives 0.25, 0.5 and 0.125, so both ratios are exactly 2:1.
	require.False(t, dashboard.LightTextHasHigherContrast(0.20, 0.45, 0.075))
}

func TestLightPaletteColourProfiles(t *testing.T) {
	t.Parallel()

	frame := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Btop: true}
	opts := lightColour()
	want := ansi.Strip(dashboard.Render(frame, 120, 40, opts))
	opts.Profile = colorprofile.ANSI256
	out := dashboard.Render(frame, 120, 40, opts)
	require.Equal(t, want, ansi.Strip(out))
	require.NotContains(t, out, "48;2;")
	require.Contains(t, out, "48;5;")
	opts.Profile = colorprofile.ASCII
	require.Equal(t, dashboard.Render(frame, 120, 40, attrs), dashboard.Render(frame, 120, 40, opts))
}

func TestLightPaletteKeepsStatusAndSelectionAtReducedDepth(t *testing.T) {
	t.Parallel()

	frame := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at}
	opts := lightColour()
	opts.Profile = colorprofile.ANSI256
	indexed := dashboard.Render(frame, 120, 40, opts)
	require.Regexp(t, `\x1b\[38;5;88m⚠`, indexed, "blocked is dark red")
	require.Regexp(t, `\x1b\[1;38;5;17;48;5;153mpee01`, indexed, "selected name keeps the indexed bar")
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
	r, g, b, _ := dashboard.PaletteForBackground(false)[role].RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

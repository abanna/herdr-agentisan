package dashboard_test

import (
	"crypto/sha256"
	"fmt"
	"image/color"
	"os"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
)

const ansi256MainCapture = "testdata/ansi256-main-f38351e.golden"

func TestANSIMatchesMain(t *testing.T) {
	t.Parallel()
	checkCompatibilityFrames(t, colorprofile.ANSI, "testdata/ansi-main-f38351e.golden")
}

func TestANSI256MatchesMainWithoutBackground(t *testing.T) {
	t.Parallel()
	checkCompatibilityFrames(t, colorprofile.ANSI256, ansi256MainCapture)
}

func TestANSI256BackgroundReply(t *testing.T) {
	t.Parallel()

	m := paletteModel(t, fixture(t, "full"))
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	legacy, _ := update(t, m, tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	capture, err := os.ReadFile(ansi256MainCapture)
	require.NoError(t, err)
	_, overview, ok := strings.Cut(string(capture), "\noverview ")
	require.True(t, ok)
	quoted, _, _ := strings.Cut(overview, "\n")
	want, err := strconv.Unquote(quoted)
	require.NoError(t, err)
	require.Equal(t, want, legacy.View().Content)

	for _, invalid := range []color.Color{ansi.XParseColor("invalid"), color.Transparent, color.RGBA{R: 30, G: 30, B: 46, A: 128}} {
		for _, profileFirst := range []bool{false, true} {
			got := reportProfile(t, m, colorprofile.ANSI256, invalid, profileFirst)
			require.Equal(t, want, got.View().Content, "invalid reply %v, profile first %v", invalid, profileFirst)
		}
	}

	for _, bg := range []string{"#1e1e2e", "#eaeaea"} {
		reply := lipgloss.Color(bg)
		late, _ := update(t, legacy, tea.BackgroundColorMsg{Color: reply})
		early := reportProfile(t, m, colorprofile.ANSI256, reply, false)
		require.Equal(t, early.View().Content, late.View().Content)
		require.NotEqual(t, want, late.View().Content)
		require.NotNil(t, paintedCell(t, lineWith(t, late.View().Content, "pee01"), "pee01").fill)
		invalidated, _ := update(t, late, tea.BackgroundColorMsg{Color: color.Transparent})
		require.Equal(t, want, invalidated.View().Content)
	}
}

func TestANSI256Frames(t *testing.T) {
	t.Parallel()

	for _, variant := range []struct{ name, background string }{{"dark", "#1e1e2e"}, {"light", "#eaeaea"}} {
		t.Run(variant.name, func(t *testing.T) {
			opts := dashboard.Options{Profile: colorprofile.ANSI256, Background: lipgloss.Color(variant.background)}
			golden.RequireEqual(t, captureCompatibilityFrames(t, opts))
		})
	}
}

func checkCompatibilityFrames(t *testing.T, profile colorprofile.Profile, path string) {
	t.Helper()

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), captureCompatibilityFrames(t, dashboard.Options{Profile: profile}))
}

// captureCompatibilityFrames quotes examples and hashes a length-framed
// stream of raw ANSI frames for every fixture, view and selectable position.
func captureCompatibilityFrames(t *testing.T, opts dashboard.Options) string {
	t.Helper()
	board, err := (&dashboard.FileBoard{Path: "../../docs/dashboard/sample-board.json"}).Board(t.Context())
	require.NoError(t, err)
	var capture strings.Builder
	for _, name := range fixtures {
		snap := fixture(t, name)
		digest := sha256.New()
		for _, size := range [][2]int{{3, 9}, {55, 14}, {97, 24}, {120, 40}} {
			w, h := size[0], size[1]
			frame := dashboard.Frame{Snapshot: snap, Now: at, Btop: true}
			record := func(key string) {
				out := dashboard.Render(frame, w, h, opts)
				fmt.Fprintf(digest, "%d:%d:%s:%d:%s", w, h, key, len(out), out)
				switch {
				case name == "full" && w == 120 && key == "overview/0":
					fmt.Fprintln(&capture, "overview", strconv.Quote(out))
				case name == "aged" && w == 120 && key == "group/0/0":
					fmt.Fprintln(&capture, "group", strconv.Quote(out))
				case name == "full" && w == 97 && key == "board/1/true":
					fmt.Fprintln(&capture, "board", strconv.Quote(out))
				}
			}
			frame.Selection = dashboard.Selection{Group: -1}
			record("overview/unselected")
			for i, sel := range selections(snap) {
				frame.Selection = sel
				record(fmt.Sprintf("overview/%d", i))
			}
			for g, group := range snap.Groups {
				frame.Group = group.Name
				for a := range max(1, len(group.Agents)) {
					frame.Selection = dashboard.Selection{Group: g, Agent: a}
					record(fmt.Sprintf("group/%d/%d", g, a))
				}
			}
			frame.Group = ""
			frame.Board = dashboard.BoardFrame{Open: true, Available: true, Ready: true, Posts: board.Posts, OpenMark: board.Mark}
			for selected := range board.Posts {
				frame.Board.Selected = selected
				for _, expanded := range []bool{false, true} {
					frame.Board.Expanded = expanded
					record(fmt.Sprintf("board/%d/%t", selected, expanded))
				}
			}
		}
		fmt.Fprintf(&capture, "%s %x\n", name, digest.Sum(nil))
	}
	return capture.String()
}

func TestSolarizedDarkANSISelection(t *testing.T) {
	t.Parallel()

	frame := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at}
	opts := dashboard.Options{Profile: colorprofile.ANSI}
	out := dashboard.Render(frame, 120, 40, opts)
	for row := range strings.SplitSeq(out, "\n") {
		if strings.Contains(row, "pee01") {
			require.NotRegexp(t, `\x1b\[(?:[0-9]+;)*(?:4[0-9]|10[0-7])[;m]`, row)
			require.Contains(t, row, "\x1b[1;7mpee01")
			const reset = "\x1b[m"
			runs := strings.Split(strings.TrimSuffix(row, reset), reset)
			require.GreaterOrEqual(t, len(runs), 3)
			require.Equal(t, "│ ", ansi.Strip(runs[0]))
			require.Equal(t, " │", ansi.Strip(runs[len(runs)-1]))
			for _, run := range runs[1 : len(runs)-1] {
				require.Regexp(t, `^\x1b\[(?:[0-9]+;)*7(?:;[0-9]+)*m`, run, "every selected cell between the borders uses reverse video")
			}
			return
		}
	}
	t.Fatal("selected worker pee01 is missing")
}

func TestSolarizedLightANSISecondaryText(t *testing.T) {
	t.Parallel()

	board, err := (&dashboard.FileBoard{Path: "../../docs/dashboard/sample-board.json"}).Board(t.Context())
	require.NoError(t, err)
	opts := dashboard.Options{Profile: colorprofile.ANSI, Background: lipgloss.Color("#fdf6e3")}
	frame := dashboard.Frame{Snapshot: fixture(t, "aged"), Now: at}
	const hidden = `\x1b\[(?:[0-9]+;)*90[;m]`
	out := dashboard.Render(frame, 120, 40, opts)
	require.NotRegexp(t, hidden, out, "SGR 90 matches Solarized's background")
	require.Contains(t, out, "\x1b[3;2;34m", "main's slot and attributes for stale stages")
	frame.Board = dashboard.BoardFrame{Open: true, Ready: true, Posts: board.Posts, OpenMark: board.Mark, Selected: 1}
	out = dashboard.Render(frame, 120, 40, opts)
	require.NotRegexp(t, hidden, out, "SGR 90 matches Solarized's background")
	require.Contains(t, out, "\x1b[34mha-coder-5", "main's slot for board authors")
}

func TestANSIModelIgnoresBackground(t *testing.T) {
	t.Parallel()

	m := dashboard.New(t.Context(), dashboard.Config{})
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = update(t, m, tea.ColorProfileMsg{Profile: colorprofile.ANSI})
	want := dashboard.Render(m.Frame(), m.Width(), m.Height(), dashboard.Options{Profile: colorprofile.ANSI})
	require.Equal(t, want, m.View().Content, "before a background reply")
	for _, bg := range []string{"#002b36", "#fdf6e3"} {
		m, _ = update(t, m, tea.BackgroundColorMsg{Color: lipgloss.Color(bg)})
		require.Equal(t, want, m.View().Content, "after a reply of %s", bg)
	}
}

package dashboard_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
)

const ansiMainGolden = "testdata/ansi-main-f38351e.golden"

func TestANSIMatchesMain(t *testing.T) {
	t.Parallel()

	want, err := os.ReadFile(ansiMainGolden)
	require.NoError(t, err)
	for _, light := range []bool{false, true} {
		opts := dashboard.Options{Profile: colorprofile.ANSI, LightBackground: light}
		require.Equal(t, string(want), captureCompatibilityFrames(t, opts), "light background: %v", light)
	}
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
	opts := dashboard.Options{Profile: colorprofile.ANSI, LightBackground: true}
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

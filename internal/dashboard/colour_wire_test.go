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
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

const readableContrast = 4.5

type cellPaint struct {
	ink, fill color.Color
}

func paintedCell(t *testing.T, line, text string) cellPaint {
	t.Helper()
	before, _, ok := strings.Cut(line, text)
	require.Truef(t, ok, "missing %q in %q", text, line)
	var paint cellPaint
	for _, match := range sgr.FindAllStringSubmatch(before, -1) {
		parts := strings.Split(match[1], ";")
		for i := 0; i < len(parts); i++ {
			switch parts[i] {
			case "", "0":
				paint = cellPaint{}
			case "39":
				paint.ink = nil
			case "49":
				paint.fill = nil
			case "38", "48":
				c, read := extendedColour(t, parts[i+1:])
				if parts[i] == "38" {
					paint.ink = c
				} else {
					paint.fill = c
				}
				i += read
			}
		}
	}
	return paint
}

func extendedColour(t *testing.T, parts []string) (color.Color, int) {
	t.Helper()
	require.NotEmpty(t, parts)
	read := func(s string) uint8 {
		n, err := strconv.ParseUint(s, 10, 8)
		require.NoError(t, err)
		return uint8(n)
	}
	switch parts[0] {
	case "5":
		require.GreaterOrEqual(t, len(parts), 2)
		return ansi.IndexedColor(read(parts[1])), 2
	case "2":
		require.GreaterOrEqual(t, len(parts), 4)
		return color.RGBA{R: read(parts[1]), G: read(parts[2]), B: read(parts[3]), A: 255}, 4
	default:
		t.Fatalf("unsupported SGR colour %v", parts)
		return nil, 0
	}
}

func paletteModel(t *testing.T, snap *snapshot.Snapshot) dashboard.Model {
	t.Helper()
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(snap)}}
	cfg := config(src, nil)
	cfg.Btop = &opener{}
	cfg.Options = dashboard.Options{}
	m := dashboard.New(t.Context(), cfg)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 24})
	batch, ok := m.Init()().(tea.BatchMsg)
	require.True(t, ok, "startup fetch and request")
	require.Len(t, batch, 2)
	m, _ = update(t, m, batch[0]())
	return m
}

func reportProfile(t *testing.T, m dashboard.Model, profile colorprofile.Profile, bg color.Color, profileFirst bool) dashboard.Model {
	t.Helper()
	msg := tea.ColorProfileMsg{Profile: profile}
	reply := tea.BackgroundColorMsg{Color: bg}
	if profileFirst {
		m, _ = update(t, m, msg)
		m, _ = update(t, m, reply)
	} else {
		m, _ = update(t, m, reply)
		m, _ = update(t, m, msg)
	}
	return m
}

func TestANSI256ClippedBlockedStageStaysRed(t *testing.T) {
	t.Parallel()

	snap := &snapshot.Snapshot{V: 1, Team: "crew", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{
		{Name: "worker", Status: snapshot.StatusWorking, Stage: strings.Repeat("x", 80) + " blocked"},
	}}}}
	frame := dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at}
	out := dashboard.Render(frame, 80, 24, dashboard.Options{Profile: colorprofile.ANSI256})
	require.NotContains(t, ansi.Strip(out), "blocked", "the trigger word is clipped")
	stage := paintedCell(t, lineWith(t, out, "worker"), "xxx").ink
	requireRedInk(t, stage)
	require.Equal(t, ansi.IndexedColor(210), stage)
}

func TestANSI256SelectionStaysVisible(t *testing.T) {
	t.Parallel()

	snap := fixture(t, "full")
	for _, tc := range []struct {
		bg   string
		fill ansi.IndexedColor
	}{
		{"#1e1e2e", 20},
		{"#4e4e4e", 20},
		{"#eaeaea", 189},
		{"#afd7ff", 189},
	} {
		t.Run(tc.bg, func(t *testing.T) {
			check := func(out, selected, other string) {
				t.Helper()
				require.Equal(t, tc.fill, paintedCell(t, lineWith(t, out, selected), selected).fill)
				require.Nil(t, paintedCell(t, lineWith(t, out, other), other).fill)
				require.NotContains(t, lineWith(t, out, selected), "\x1b[1;7m")
			}
			m := paletteModel(t, snap)
			a := reportProfile(t, m, colorprofile.ANSI256, lipgloss.Color(tc.bg), true)
			b := reportProfile(t, m, colorprofile.ANSI256, lipgloss.Color(tc.bg), false)
			check(a.View().Content, "pee01", "pee02")
			require.Equal(t, a.View().Content, b.View().Content, "terminal messages in either order")
			f := a.Frame()
			// Use the model to negotiate the live frame. The pure render
			// variations exercise the same selection surface in other views.
			opts := dashboard.Options{Profile: colorprofile.ANSI256, LightBackground: tc.fill == 189}
			f.Group = "coders"
			check(dashboard.Render(f, 55, 60, opts), "pee01", "pee02")
			f.Board = dashboard.BoardFrame{Open: true, Ready: true, Selected: 1, Posts: []daemon.BoardPostView{
				{Body: "earlier topic"}, {Body: "current topic"},
			}}
			check(dashboard.Render(f, 120, 40, opts), "current topic", "earlier topic")
		})
	}
}

func TestReportedGreyKeepsTeamNameReadable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		bg, rgb string
		index   ansi.IndexedColor
	}{
		{"#808080", "#171717", 233},
		{"#000000", "#ffffff", 231},
		{"#1e1e2e", "#ffffff", 231},
		{"#eaeaea", "#696969", 241},
		{"#ffffff", "#767676", 243},
		{"#ff0000", "#161616", 233},
		{"#4e4e4e", "#ffffff", 231},
		{"#afd7ff", "#5b5b5b", 240},
	} {
		t.Run(tc.bg, func(t *testing.T) {
			snap := &snapshot.Snapshot{V: 1, Team: "crew", Project: &snapshot.Project{Color: "#ffffff"}}
			m := paletteModel(t, snap)
			bg := lipgloss.Color(tc.bg)
			for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256} {
				out := reportProfile(t, m, p, bg, p == colorprofile.TrueColor).View().Content
				ink := paintedCell(t, lineWith(t, out, snap.Team), snap.Team).ink
				border := paintedCell(t, strings.SplitN(out, "\n", 2)[0], "╭").ink
				require.NotNil(t, ink)
				require.Equal(t, ink, border, "%s name and border", p)
				require.GreaterOrEqualf(t, contrastRatio(ink, bg), readableContrast, "%s name on %s", p, tc.bg)
				if p == colorprofile.TrueColor {
					require.Equal(t, rgbBytes(lipgloss.Color(tc.rgb)), rgbBytes(ink))
				} else {
					require.Equal(t, tc.index, ink)
				}
			}
		})
	}
}

func TestPassingTeamColourIsPreserved(t *testing.T) {
	t.Parallel()

	const accent = "#870000"
	snap := &snapshot.Snapshot{V: 1, Team: "crew", Project: &snapshot.Project{Color: accent}}
	m := paletteModel(t, snap)
	bg := lipgloss.Color("#eaeaea")
	for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256} {
		out := reportProfile(t, m, p, bg, false).View().Content
		ink := paintedCell(t, lineWith(t, out, snap.Team), snap.Team).ink
		require.NotNil(t, ink)
		require.Equal(t, rgbBytes(lipgloss.Color(accent)), rgbBytes(ink), "%s", p)
	}
}

var cueSamples = []struct {
	role, worker, text string
}{
	{"green", "doing", "◐"},
	{"orange", "doing", "70%"},
	{"yellow", "idling", "●"},
	{"red", "stopped", "⚠"},
	{"cyan", "readying", "⟳"},
}

func paletteSpecimen() *snapshot.Snapshot {
	return &snapshot.Snapshot{V: 1, Team: "crew", Groups: []snapshot.Group{{
		Name: "agents",
		Agents: []snapshot.Agent{
			{Name: "doing", Status: snapshot.StatusWorking, Ctx: new(70)},
			{Name: "idling", Status: snapshot.StatusIdle},
			{Name: "stopped", Status: snapshot.StatusBlocked},
			{Name: "readying", Status: snapshot.StatusReady},
		},
	}}}
}

func TestReadableInkKeepsAnAvailablePaletteShade(t *testing.T) {
	t.Parallel()

	const backgroundLuminance = 0.22
	preferred := ansi.IndexedColor(244)
	alternate := ansi.IndexedColor(17)
	require.Equal(t, alternate, dashboard.ReadableInk(preferred, alternate, backgroundLuminance, true))
}

func TestDashboardContrastSweep(t *testing.T) {
	t.Parallel()

	for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256} {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			snap := &snapshot.Snapshot{V: 1, Team: "crew", Project: &snapshot.Project{Color: "#a060d0"}}
			m := paletteModel(t, snap)
			m, _ = update(t, m, tea.ColorProfileMsg{Profile: p})
			roleNames := map[int]string{
				dashboard.PaletteText: "text", dashboard.PaletteSecondary: "secondary",
				dashboard.PaletteGreen: "green", dashboard.PaletteYellow: "yellow",
				dashboard.PaletteOrange: "orange", dashboard.PaletteRed: "red",
				dashboard.PaletteCyan: "cyan", dashboard.PaletteAccent: "accent",
			}
			indexed := p == colorprofile.ANSI256
			darkBar, dark, _ := dashboard.PalettePaints(color.Black, indexed)
			lightBar, light, _ := dashboard.PalettePaints(color.White, indexed)
			neutral := make(map[int]int, len(roleNames))
			seen := 0
			for r := 0; r <= 255; r += 17 {
				for g := 0; g <= 255; g += 17 {
					for b := 0; b <= 255; b += 17 {
						bg := color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
						live, _ := update(t, m, tea.BackgroundColorMsg{Color: bg})
						out := live.View().Content
						name := paintedCell(t, lineWith(t, out, "crew"), "crew").ink
						require.NotNil(t, name)
						require.GreaterOrEqualf(t, contrastRatio(name, bg), readableContrast, "team name on %v", bg)
						button := paintedCell(t, lineWith(t, out, "[ btop ]"), "[ btop ]")
						require.NotNil(t, button.ink)
						require.NotNil(t, button.fill)
						require.GreaterOrEqual(t, contrastRatio(button.ink, button.fill), readableContrast)

						bar, terminal, selected := dashboard.PalettePaints(bg, indexed)
						wantBar := darkBar
						if contrastRatio(light[dashboard.PaletteText], bg) > contrastRatio(dark[dashboard.PaletteText], bg) {
							wantBar = lightBar
						}
						require.Equalf(t, wantBar, bar, "palette with better primary text on %v", bg)
						for role, ink := range terminal {
							require.GreaterOrEqualf(t, contrastRatio(ink, bg), readableContrast, "%s on terminal %v", roleNames[role], bg)
							require.GreaterOrEqualf(t, contrastRatio(selected[role], bar), readableContrast, "%s selected on %v", roleNames[role], bg)
							switch rgbBytes(ink) {
							case [3]uint8{}, [3]uint8{255, 255, 255}:
								neutral[role]++
							}
						}
						seen++
					}
				}
			}
			for role := range len(roleNames) {
				t.Logf("%s neutral fallback: %d/%d (%.2f%%)", roleNames[role], neutral[role], seen, 100*float64(neutral[role])/float64(seen))
			}
		})
	}
}

func rgbBytes(c color.Color) [3]uint8 {
	r, g, b, _ := c.RGBA()
	return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}
}

func requireRedInk(t *testing.T, c color.Color) {
	t.Helper()
	require.NotNil(t, c)
	rgb := rgbBytes(c)
	// Require about a sixth of the channel range above both other channels.
	require.GreaterOrEqualf(t, int(rgb[0])-max(int(rgb[1]), int(rgb[2])), 40, "%v must read red", rgb)
}

// OKLab approximates perceptual difference across brightnesses and hues.
// The 0.060 floor leaves a margin above the first draft's 0.047 yellow/orange
// and 0.053 orange/red pairs, while the adopted pairs are at least 0.070.
func okLab(c color.Color) [3]float64 {
	rgb := rgbBytes(c)
	linear := func(v uint8) float64 {
		x := float64(v) / 255
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	r, g, b := linear(rgb[0]), linear(rgb[1]), linear(rgb[2])
	l := math.Cbrt(0.4122214708*r + 0.5363325363*g + 0.0514459929*b)
	m := math.Cbrt(0.2119034982*r + 0.6806995451*g + 0.1073969566*b)
	s := math.Cbrt(0.0883024619*r + 0.2817188376*g + 0.6299787005*b)
	return [3]float64{
		0.2104542553*l + 0.793617785*m - 0.0040720468*s,
		1.9779984951*l - 2.428592205*m + 0.4505937099*s,
		0.0259040371*l + 0.7827717662*m - 0.808675766*s,
	}
}

func TestANSI256StatusColoursStayDistinct(t *testing.T) {
	t.Parallel()

	for _, light := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("light=%t/selected=%t", light, selected), func(t *testing.T) {
				f := paletteSpecimen()
				opts := dashboard.Options{Profile: colorprofile.ANSI256, LightBackground: light}
				roles := cueSamples
				colours := make([]color.Color, 0, len(roles))
				for _, cue := range roles {
					frame := dashboard.Frame{Snapshot: f, Now: at, Selection: dashboard.Selection{Group: -1}}
					if selected {
						for i, agent := range f.Groups[0].Agents {
							if agent.Name == cue.worker {
								frame.Selection = dashboard.Selection{Agent: i}
								break
							}
						}
					}
					out := dashboard.Render(frame, 120, 24, opts)
					paint := paintedCell(t, lineWith(t, out, cue.worker), cue.text)
					require.IsType(t, ansi.IndexedColor(0), paint.ink)
					rgb := rgbBytes(paint.ink)
					require.NotEqual(t, max(rgb[0], rgb[1], rgb[2]), min(rgb[0], rgb[1], rgb[2]), "%s has a hue", cue.role)
					if cue.role == "red" {
						requireRedInk(t, paint.ink)
					}
					colours = append(colours, paint.ink)
				}
				for i, a := range colours {
					for j, b := range colours[:i] {
						x, y := okLab(a), okLab(b)
						d := math.Hypot(math.Hypot(x[0]-y[0], x[1]-y[1]), x[2]-y[2])
						require.GreaterOrEqualf(t, d, 0.060, "%s and %s OKLab distance", roles[i].role, roles[j].role)
					}
				}
			})
		}
	}
}

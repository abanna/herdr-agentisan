package dashboard_test

import (
	"fmt"
	"image/color"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
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
	const reset = "\x1b[m"
	if i := strings.LastIndex(before, reset); i >= 0 {
		before = before[i+len(reset):]
	}
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
	out := dashboard.Render(frame, 80, 24, dashboard.Options{Profile: colorprofile.ANSI256, Background: lipgloss.Color("#1e1e2e")})
	require.NotContains(t, ansi.Strip(out), "blocked", "the trigger word is clipped")
	stage := paintedCell(t, lineWith(t, out, "worker"), "xxx").ink
	requireRedInk(t, stage)
	require.Equal(t, ansi.IndexedColor(210), stage)
}

// A clipped stage keeps only its colour (ADR-001 A38). On #a0a0a0 neither
// configured red nor configured green reaches 4.5:1, so each falls back along
// its own hue: red to slot 52, and green, which has no slot that helps there,
// to the shared neutral.
func TestANSI256ClippedStagesKeepTheirHueOnGrey(t *testing.T) {
	t.Parallel()

	const bg = "#a0a0a0"
	prefix := strings.Repeat("x", 80)
	snap := &snapshot.Snapshot{V: 1, Team: "crew", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{
		{Name: "alpha1", Status: snapshot.StatusWorking, Stage: prefix + " blocked"},
		{Name: "alpha2", Status: snapshot.StatusWorking, Stage: prefix + " ready"},
	}}}}
	frame := dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at}
	out := dashboard.Render(frame, 80, 24, dashboard.Options{Profile: colorprofile.ANSI256, Background: lipgloss.Color(bg)})
	plain := ansi.Strip(out)
	require.NotContains(t, plain, "blocked", "the trigger word is clipped")
	require.NotContains(t, plain, "ready", "the trigger word is clipped")
	blocked := paintedCell(t, lineWith(t, out, "alpha1"), "xxx").ink
	ready := paintedCell(t, lineWith(t, out, "alpha2"), "xxx").ink
	require.NotNil(t, blocked)
	require.NotNil(t, ready)
	require.NotEqual(t, blocked, ready, "a blocked stage must not draw as a ready one")
	requireRedInk(t, blocked)
	for name, ink := range map[string]color.Color{"blocked": blocked, "ready": ready} {
		require.GreaterOrEqualf(t, contrastRatio(ink, lipgloss.Color(bg)), readableContrast, "%s stage on %s", name, bg)
	}
	require.Equal(t, ansi.IndexedColor(52), blocked)
}

// The same clipped stages at TrueColor. On #a0a0a0 neither configured red
// (#870000, 3.96:1) nor configured green (#14532d, 3.48:1) reaches 4.5:1. Red
// falls back along its ladder to #5f0000 (5.40:1). Green has no shade that
// reaches the floor, so it keeps the ink main drew, #14532d at 3.48:1, below
// the floor but never the shared neutral: blocked and ready stay apart.
func TestTrueColorClippedStagesKeepTheirHueOnGrey(t *testing.T) {
	t.Parallel()

	const bg = "#a0a0a0"
	prefix := strings.Repeat("x", 80)
	snap := &snapshot.Snapshot{V: 1, Team: "crew", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{
		{Name: "alpha1", Status: snapshot.StatusWorking, Stage: prefix + " blocked"},
		{Name: "alpha2", Status: snapshot.StatusWorking, Stage: prefix + " ready"},
	}}}}
	frame := dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at}
	out := dashboard.Render(frame, 80, 24, dashboard.Options{Profile: colorprofile.TrueColor, Background: lipgloss.Color(bg)})
	plain := ansi.Strip(out)
	require.NotContains(t, plain, "blocked", "the trigger word is clipped")
	require.NotContains(t, plain, "ready", "the trigger word is clipped")
	blocked := paintedCell(t, lineWith(t, out, "alpha1"), "xxx").ink
	ready := paintedCell(t, lineWith(t, out, "alpha2"), "xxx").ink
	require.NotNil(t, blocked)
	require.NotNil(t, ready)
	require.NotEqual(t, rgbBytes(blocked), rgbBytes(ready), "a blocked stage must not draw as a ready one")
	require.Equal(t, [3]uint8{0x5f, 0, 0}, rgbBytes(blocked))
	require.Equal(t, [3]uint8{0x14, 0x53, 0x2d}, rgbBytes(ready))
	require.InDelta(t, 5.40, contrastRatio(blocked, lipgloss.Color(bg)), 0.01)
	require.GreaterOrEqual(t, contrastRatio(blocked, lipgloss.Color(bg)), readableContrast)
	require.InDelta(t, 3.48, contrastRatio(ready, lipgloss.Color(bg)), 0.01)
	requireRedInk(t, blocked)
}

// The fifth review's case, a stated limit (NERD-5792): on #b0b0b0 neither the
// queued stage's yellow (3.10:1) nor the ordinary stage's secondary ink
// (3.84:1) reaches 4.5:1, and no stock slot of their hues does, so both draw
// the shared neutral and a clipped queued stage looks like an ordinary one.
// Main drew slots 228 and 60. A change to this behaviour must show here.
func TestANSI256ClippedQueuedStageDrawsLikeAnOrdinaryOneOnGrey(t *testing.T) {
	t.Parallel()

	const bg = "#b0b0b0"
	prefix := strings.Repeat("x", 80)
	snap := &snapshot.Snapshot{V: 1, Team: "crew", Groups: []snapshot.Group{{Name: "coders", Agents: []snapshot.Agent{
		{Name: "alpha1", Status: snapshot.StatusWorking, Stage: prefix + " queued"},
		{Name: "alpha2", Status: snapshot.StatusWorking, Stage: prefix + " reviewing"},
	}}}}
	frame := dashboard.Frame{Snapshot: snap, Selection: dashboard.Selection{Group: -1}, Now: at}
	out := dashboard.Render(frame, 80, 24, dashboard.Options{Profile: colorprofile.ANSI256, Background: lipgloss.Color(bg)})
	plain := ansi.Strip(out)
	require.NotContains(t, plain, "queued", "the trigger word is clipped")
	require.NotContains(t, plain, "reviewing", "the suffix is clipped")
	queued := paintedCell(t, lineWith(t, out, "alpha1"), "xxx").ink
	ordinary := paintedCell(t, lineWith(t, out, "alpha2"), "xxx").ink
	require.Equal(t, ansi.IndexedColor(232), queued)
	require.Equal(t, ansi.IndexedColor(232), ordinary)
	require.GreaterOrEqual(t, contrastRatio(queued, lipgloss.Color(bg)), readableContrast)
}

func TestANSI256SelectionStaysVisible(t *testing.T) {
	t.Parallel()

	snap := fixture(t, "full")
	for _, tc := range []struct {
		bg   string
		fill ansi.IndexedColor
	}{
		{"#1e1e2e", 54},
		{"#4e4e4e", 54},
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
			opts := dashboard.Options{Profile: colorprofile.ANSI256, Background: lipgloss.Color(tc.bg)}
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

func TestTrueColorWithoutBackgroundKeepsDarkPalette(t *testing.T) {
	t.Parallel()

	snap := paletteSpecimen()
	snap.Project = &snapshot.Project{Color: "#808080"}
	m := paletteModel(t, snap)
	m, _ = update(t, m, tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	out := m.View().Content
	name := paintedCell(t, lineWith(t, out, snap.Team), snap.Team)
	selected := paintedCell(t, lineWith(t, out, "doing"), "doing")
	require.Equal(t, [3]uint8{128, 128, 128}, rgbBytes(name.ink))
	require.Equal(t, [3]uint8{54, 71, 107}, rgbBytes(selected.fill))
}

func TestPassingTeamColourIsPreserved(t *testing.T) {
	t.Parallel()

	bg := lipgloss.Color("#eaeaea")
	for _, accent := range []string{"#870000", "#696969"} {
		snap := &snapshot.Snapshot{V: 1, Team: "crew", Project: &snapshot.Project{Color: accent}}
		m := paletteModel(t, snap)
		for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256} {
			out := reportProfile(t, m, p, bg, false).View().Content
			ink := paintedCell(t, lineWith(t, out, snap.Team), snap.Team).ink
			require.NotNil(t, ink)
			want := lipgloss.Color(accent)
			if p == colorprofile.ANSI256 && accent == "#696969" {
				want = ansi.IndexedColor(241) // Closest passing index to the configured grey.
			}
			require.Equal(t, rgbBytes(want), rgbBytes(ink), "%s in %s", accent, p)
		}
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
	alternate := ansi.IndexedColor(232)
	require.Equal(t, alternate, dashboard.ReadableInk(preferred, alternate, nil, backgroundLuminance, true, false))
}

func TestReadableInkFallsBackAlongItsLadder(t *testing.T) {
	t.Parallel()

	// Background luminance 0.35: neither shade reaches 4.5:1, slot 52 does.
	const backgroundLuminance = 0.35
	preferred := ansi.IndexedColor(210)
	alternate := ansi.IndexedColor(88)
	ladder := []ansi.IndexedColor{124, 52}
	require.Equal(t, ansi.IndexedColor(52), dashboard.ReadableInk(preferred, alternate, ladder, backgroundLuminance, true, true))
	require.Equal(t, ansi.IndexedColor(232), dashboard.ReadableInk(preferred, alternate, ladder[:1], backgroundLuminance, true, true), "no ladder slot reaches 4.5:1: the shared neutral")
	require.Equal(t, alternate, dashboard.ReadableInk(preferred, alternate, ladder, 0.8, true, true), "a configured shade that passes wins over the ladder")
}

func TestReadableInkAtTrueColorKeepsMainsInkBelowTheFloor(t *testing.T) {
	t.Parallel()

	const backgroundLuminance = 0.35
	preferred := lipgloss.Color("#ff989f")
	alternate := lipgloss.Color("#870000")
	ladder := []ansi.IndexedColor{124, 52}
	got := dashboard.ReadableInk(preferred, alternate, ladder, backgroundLuminance, false, true)
	require.Equal(t, [3]uint8{0x5f, 0, 0}, rgbBytes(got), "the ladder slot's own RGB, not its index")
	require.IsType(t, color.RGBA{}, got)
	require.Equal(t, preferred, dashboard.ReadableInk(preferred, alternate, ladder[:1], backgroundLuminance, false, true), "no shade reaches 4.5:1: main's ink")
	require.Equal(t, color.Black, dashboard.ReadableInk(preferred, alternate, ladder[:1], backgroundLuminance, false, false), "a role without a hue takes the neutral")
}

// Match attributes, resets and extended colours from the standard xterm palette.
var indexedSGR = regexp.MustCompile(`^(?:(?:[0-9]|2[0-9]|39|49|(?:38|48);5;(?:2[2-9]|[3-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5]))(?:;|$))*$`)

func TestANSI256ReservedSlots(t *testing.T) {
	t.Parallel()

	// #000000 draws the nearest slot, 232. #0000ff would draw slot 21 on a
	// white or cream terminal if the reserved range ended below it.
	for _, project := range []string{"#000000", "#0000ff"} {
		snap := paletteSpecimen()
		snap.Project = &snapshot.Project{Color: project}
		m := paletteModel(t, snap)
		for _, bg := range []string{"#181818", "#fdf6e3", "#ffffff", "#878787", "#777777"} {
			t.Run(project+"/"+bg, func(t *testing.T) {
				out := reportProfile(t, m, colorprofile.ANSI256, lipgloss.Color(bg), true).View().Content
				for _, match := range sgr.FindAllStringSubmatch(out, -1) {
					assert.Truef(t, indexedSGR.MatchString(match[1]), "expected indexed SGR in slots 22–255, got %q", match[0])
				}
			})
		}
	}
}

func TestANSI256UsesBestNeutralOnMidtones(t *testing.T) {
	t.Parallel()

	snap := paletteSpecimen()
	snap.Project = &snapshot.Project{Color: "#000000"}
	m := paletteModel(t, snap)
	for _, tc := range []struct {
		bg   string
		want ansi.IndexedColor
	}{
		{"#777777", 231},
		{"#777778", 232},
	} {
		out := reportProfile(t, m, colorprofile.ANSI256, lipgloss.Color(tc.bg), true).View().Content
		require.Equal(t, tc.want, paintedCell(t, lineWith(t, out, snap.Team), snap.Team).ink, tc.bg)
		require.Equal(t, tc.want, paintedCell(t, lineWith(t, out, "stopped"), "⚠").ink, tc.bg)
	}
}

// sameStageInk is, for each pair of stage cues, the number of the sweep's
// 4,096 backgrounds on which the two draw the same 256-colour ink: a ready
// (green), a blocked (red), a waiting or queued (yellow) and an ordinary
// stage (secondary). A stage can be clipped to its colour (ADR-001 A38). No
// stock slot of a hue reaches 4.5:1 on a band of backgrounds, so the cues
// meet in the shared neutral there. The owner ruled on 2026-11-24 07:08 that
// the floor stays and a non-colour cue is a separate item (NERD-5792).
var sameStageInk = map[string]int{
	"red/green":        913,
	"yellow/green":     1861,
	"red/yellow":       802,
	"secondary/green":  1874,
	"secondary/yellow": 1763,
	"secondary/red":    1373,
}

func TestDashboardContrastSweep(t *testing.T) {
	t.Parallel()

	for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256} {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			// Two selected rows cover every role that appears on the bar.
			snap := &snapshot.Snapshot{
				V:       1,
				Team:    "crew",
				Project: &snapshot.Project{Color: "#a060d0"},
				Groups: []snapshot.Group{{Name: "agents", Agents: []snapshot.Agent{
					{Name: "readying", Status: snapshot.StatusReady, Kind: "cdx", Ctx: new(70), Stage: "blocked stage"},
					{Name: "idling", Status: snapshot.StatusIdle, Stage: "ready stage"},
				}}},
			}
			m := paletteModel(t, snap)
			m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 16})
			m, _ = update(t, m, tea.ColorProfileMsg{Profile: p})
			next, _ := update(t, m, keyDown)
			models := [2]dashboard.Model{m, next}

			rowPaint := func(out, name, text string) cellPaint {
				t.Helper()
				return paintedCell(t, lineWith(t, out, name), text)
			}
			draw := func(m dashboard.Model, bg color.Color) string {
				t.Helper()
				live, _ := update(t, m, tea.BackgroundColorMsg{Color: bg})
				return live.View().Content
			}
			dark := draw(models[0], color.Black)
			light := draw(models[0], color.White)
			darkText, lightText := rowPaint(dark, "idling", "idling").ink, rowPaint(light, "idling", "idling").ink
			darkBar, lightBar := rowPaint(dark, "readying", "readying").fill, rowPaint(light, "readying", "readying").fill

			neutral := make(map[string]int)
			// hueRoles carry a meaning by hue: status, stage and ctx. sameSlot
			// counts the backgrounds on which two of them draw the same ink.
			hueRoles := []string{"green", "yellow", "orange", "red", "cyan"}
			sameSlot := make(map[string]int)
			// nearInk counts the backgrounds on which two hue inks that differ
			// are closer than the OKLab floor of TestANSI256StatusColoursStayDistinct.
			nearInk := make(map[string]int)
			// At TrueColor the sweep also compares every hue ink with the one main
			// drew on that background: the palette shade of the side that reads
			// best. A pair main kept apart must stay apart, and a pair main could
			// tell must stay tellable.
			paletteRole := map[string]int{
				"green": dashboard.PaletteGreen, "yellow": dashboard.PaletteYellow, "orange": dashboard.PaletteOrange,
				"red": dashboard.PaletteRed, "cyan": dashboard.PaletteCyan,
			}
			var mainInk []color.Color
			mainNear := make(map[string]int)
			worseThanMain := make(map[string]int)
			checkedSGR := make(map[string]bool)
			seen := 0
			stages := [...]string{"ready", "queued", "blocked", "reviewing"}
			group := dashboard.Frame{
				Snapshot:  &snapshot.Snapshot{V: 1, Team: "crew", Groups: []snapshot.Group{{Name: "agents"}}},
				Selection: dashboard.Selection{Group: -1}, Now: at, Group: "agents",
			}
			for i, stage := range stages {
				group.Snapshot.Groups[0].Agents = append(group.Snapshot.Groups[0].Agents, snapshot.Agent{
					Name: fmt.Sprintf("worker-%d", i+1), Status: snapshot.StatusWorking,
					Stage: strings.Repeat("x", 180) + " " + stage,
				})
			}
			var groupSameSlot [len(stages)][len(stages)]int
			check := func(role string, ink, bg color.Color) {
				t.Helper()
				require.NotNil(t, ink, role)
				require.NotNil(t, bg, role)
				floor := readableContrast
				if idx, hue := paletteRole[strings.TrimSuffix(role, " selected")]; hue && p == colorprofile.TrueColor && contrastRatio(ink, bg) < floor {
					// No shade of the hue reaches the floor here: the ink is main's.
					require.Equalf(t, rgbBytes(mainInk[idx]), rgbBytes(ink), "%s on %v is below the floor and not main's ink", role, bg)
					return
				}
				if p == colorprofile.ANSI256 {
					floor = min(floor, max(contrastRatio(ansi.IndexedColor(232), bg), contrastRatio(ansi.IndexedColor(231), bg)))
				}
				require.GreaterOrEqualf(t, contrastRatio(ink, bg), floor, "%s on %v", role, bg)
			}
			record := func(role string, ink, bg color.Color) {
				t.Helper()
				check(role, ink, bg)
				switch rgbBytes(ink) {
				case [3]uint8{}, [3]uint8{8, 8, 8}, [3]uint8{255, 255, 255}:
					neutral[role]++
				}
			}
			for r := 0; r <= 255; r += 17 {
				for g := 0; g <= 255; g += 17 {
					for b := 0; b <= 255; b += 17 {
						bg := color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
						if p == colorprofile.TrueColor {
							mainInk = dashboard.PaletteForBackground(contrastRatio(lightText, bg) > contrastRatio(darkText, bg))
						}
						var frames [len(models)]string
						for i, model := range models {
							frames[i] = draw(model, bg)
							if p == colorprofile.ANSI256 {
								_, rest, ok := strings.Cut(frames[i], "\x1b[")
								for ok {
									params, tail, found := strings.Cut(rest, "m")
									if !checkedSGR[params] {
										require.Truef(t, found && indexedSGR.MatchString(params), "expected indexed SGR in slots 22–255, got %q on %v", params, bg)
										checkedSGR[params] = true
									}
									_, rest, ok = strings.Cut(tail, "\x1b[")
								}
							}
						}
						out := frames[0]
						check("team name", rowPaint(out, "crew", "crew").ink, bg)
						check("header border", paintedCell(t, strings.SplitN(out, "\n", 2)[0], "╭").ink, bg)
						button := rowPaint(out, "[ btop ]", "[ btop ]")
						check("button", button.ink, button.fill)
						record("accent", rowPaint(out, "AGENTS", "╭").ink, bg)

						bar := darkBar
						if contrastRatio(lightText, bg) > contrastRatio(darkText, bg) {
							bar = lightBar
						}
						inks := make(map[string]color.Color)
						for _, sample := range []struct {
							frame              int
							role, worker, text string
						}{
							{0, "text", "readying", "readying"},
							{0, "secondary", "readying", "cdx"},
							{0, "orange", "readying", "70%"},
							{0, "cyan", "readying", "⟳"},
							{0, "red", "readying", "blocked stage"},
							{1, "yellow", "idling", "●"},
							{1, "green", "idling", "ready stage"},
						} {
							normal := rowPaint(frames[1-sample.frame], sample.worker, sample.text)
							selected := rowPaint(frames[sample.frame], sample.worker, sample.text)
							require.Nil(t, normal.fill, sample.role)
							require.Equalf(t, bar, selected.fill, "selected %s on %v", sample.role, bg)
							record(sample.role, normal.ink, bg)
							inks[sample.role] = normal.ink
							check(sample.role+" selected", selected.ink, selected.fill)
						}
						if p == colorprofile.TrueColor {
							for i, a := range hueRoles {
								for _, b := range hueRoles[:i] {
									ma, mb := mainInk[paletteRole[a]], mainInk[paletteRole[b]]
									if rgbBytes(ma) == rgbBytes(mb) {
										continue
									}
									if oklabDistance(ma, mb) < minimumInkDistance {
										mainNear[a+"/"+b]++
									}
									if rgbBytes(inks[a]) == rgbBytes(inks[b]) ||
										oklabDistance(ma, mb) >= minimumInkDistance && oklabDistance(inks[a], inks[b]) < minimumInkDistance {
										worseThanMain[a+"/"+b]++
									}
								}
							}
						}
						for _, other := range hueRoles {
							if inks["secondary"] == inks[other] {
								sameSlot["secondary/"+other]++
							}
						}
						for i, a := range hueRoles {
							for _, b := range hueRoles[:i] {
								if inks[a] == inks[b] {
									sameSlot[a+"/"+b]++
								} else if oklabDistance(inks[a], inks[b]) < minimumInkDistance {
									nearInk[a+"/"+b]++
								}
							}
						}
						if p == colorprofile.ANSI256 {
							out := dashboard.Render(group, 80, 40, dashboard.Options{Profile: p, Background: bg})
							inks := make([]color.Color, 0, len(stages))
							for line := range strings.Lines(out) {
								if strings.Contains(line, "xxx") && strings.Contains(line, "…") {
									ink := paintedCell(t, line, "xxx").ink
									require.IsType(t, ansi.IndexedColor(0), ink)
									inks = append(inks, ink)
								}
							}
							require.Len(t, inks, len(stages), "each agent has a clipped stage")
							for i, a := range inks {
								for j, b := range inks[:i] {
									if a == b {
										groupSameSlot[i][j]++
									}
								}
							}
						}
						seen++
					}
				}
			}
			for _, role := range []string{"text", "secondary", "green", "yellow", "orange", "red", "cyan", "accent"} {
				t.Logf("%s neutral fallback: %d/%d (%.2f%%)", role, neutral[role], seen, 100*float64(neutral[role])/float64(seen))
			}
			for i, a := range hueRoles {
				for _, b := range hueRoles[:i] {
					t.Logf("%s and %s draw the same ink: %d/%d", a, b, sameSlot[a+"/"+b], seen)
				}
			}
			if p == colorprofile.TrueColor {
				// Where no shade of a hue reaches 4.5:1 the owner kept main's ink,
				// below the floor, so blocked and ready never share an ink at
				// TrueColor (NERD-5719). The yellow and orange of main's own palette are
				// near each other (OKLab 0.047), so near pairs equal main's count.
				require.Zero(t, sameSlot["red/green"], "blocked and ready stage inks")
				require.Empty(t, worseThanMain, "a pair main kept apart, or could tell apart, on some background")
				for pair, n := range nearInk {
					require.LessOrEqualf(t, n, mainNear[pair], "%s near-identical backgrounds against main", pair)
				}
				t.Logf("near-identical hue inks: %v; main's: %v", nearInk, mainNear)
			}
			for _, other := range hueRoles {
				t.Logf("secondary and %s draw the same ink: %d/%d", other, sameSlot["secondary/"+other], seen)
			}
			if p == colorprofile.ANSI256 {
				for pair, want := range sameStageInk {
					require.Equalf(t, want, sameSlot[pair], "%s stage inks", pair)
				}
				// Group view uses plain text for an ordinary stage. Its three
				// hue cues have the same counts as in the overview.
				wantGroup := [len(stages)][len(stages)]int{{}, {1861}, {913, 802}, {530, 419, 600}}
				for i, stage := range stages {
					for j, other := range stages[:i] {
						t.Logf("group view: %s and %s draw the same ink: %d/%d", stage, other, groupSameSlot[i][j], seen)
						assert.Equalf(t, wantGroup[i][j], groupSameSlot[i][j], "group view %s/%s", stage, other)
					}
				}
				// A ladder slot must stay tellable from every other hue ink on
				// its background; a hue with no such slot keeps the neutral.
				require.Empty(t, nearInk, "distinct hue inks closer than %.3f in OKLab", minimumInkDistance)
				// Hue ladders (NERD-5719) take a status or stage ink along its own
				// hue. No stock slot of green or red reaches 4.5:1 on backgrounds
				// of relative luminance 0.159 to 0.284, so there the two share the
				// neutral. A stated limit: the owner ruled on 2026-11-24 07:08 that the
				// floor stays and a non-colour cue is a separate item (NERD-5792).
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

// minimumInkDistance is the OKLab distance two semantic inks keep apart.
const minimumInkDistance = 0.060

func oklabDistance(a, b color.Color) float64 {
	x, y := okLab(a), okLab(b)
	return math.Hypot(math.Hypot(x[0]-y[0], x[1]-y[1]), x[2]-y[2])
}

func TestANSI256StatusColoursStayDistinct(t *testing.T) {
	t.Parallel()

	for _, light := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("light=%t/selected=%t", light, selected), func(t *testing.T) {
				f := paletteSpecimen()
				bg := "#1e1e2e"
				if light {
					bg = "#eaeaea"
				}
				opts := dashboard.Options{Profile: colorprofile.ANSI256, Background: lipgloss.Color(bg)}
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
						require.GreaterOrEqualf(t, oklabDistance(a, b), minimumInkDistance, "%s and %s OKLab distance", roles[i].role, roles[j].role)
					}
				}
			})
		}
	}
}

package dashboard

import (
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// ModelCells is the widest model ShortModel returns.
const ModelCells = modelCells

// ColumnWidths is how a frame w cells wide lays out the agent rows of groups
// at now: the name's width, then the state, model, item, stage and time
// columns' widths, 0 for a column dropped.
func ColumnWidths(groups []snapshot.Group, now time.Time, w int) (name int, cols []int) {
	c := planColumns(groups, now, rowWidth(w))
	return c.name, c.width[:]
}

// PlaceText draws over at column x on a line of base's segments, all plain
// text, and returns the text drawn: the seam the tests check place's two
// cuts through, wherever the segments break.
func PlaceText(base []string, x int, over string) string {
	l := make(line, 0, len(base))
	for _, s := range base {
		l = append(l, seg{s, plainStyle})
	}
	var b strings.Builder
	for _, s := range place(l, x, line{{over, plainStyle}}) {
		b.WriteString(s.text)
	}
	return b.String()
}

// ClipText clips a line of plain segments, those marked reverse in reverse
// video, to w cells as a frame does, and returns the text it keeps.
func ClipText(base []string, reverse []bool, w int) string {
	l := make(line, 0, len(base))
	for i, s := range base {
		st := plainStyle
		if reverse[i] {
			st = st.Reverse(true)
		}
		l = append(l, seg{s, st})
	}
	var b strings.Builder
	for _, s := range l.cut(0, w).trimRight() {
		b.WriteString(s.text)
	}
	return b.String()
}

// ModelLabel is the text a row's model column draws for a: its model, or
// the agent herdr detects in its place.
func ModelLabel(a snapshot.Agent) string {
	text, _ := modelLabel(a)
	return text
}

const (
	PaletteText      = int(colText)
	PaletteSecondary = int(colDim)
	PaletteGreen     = int(colGreen)
	PaletteYellow    = int(colYellow)
	PaletteOrange    = int(colOrange)
	PaletteRed       = int(colRed)
	PaletteCyan      = int(colCyan)
	PaletteAccent    = int(colAccent)
)

// PaletteForBackground returns the text colours drawn on the terminal and bar.
func PaletteForBackground(light bool) []color.Color {
	side := darkPalette
	if light {
		side = lightPalette
	}
	text := make([]color.Color, colSelectionFill)
	for i := range text {
		text[i] = palette[i].paint(side, false)
	}
	return text
}

// LightTextHasHigherContrast compares the two text luminances on the given background luminance.
func LightTextHasHigherContrast(background, darkText, lightText float64) bool {
	return lightTextHasHigherContrast(background, darkText, lightText)
}

// SelectionColours returns the bar fill and button colours used by the renderer.
func SelectionColours(light bool) (bar, buttonInk, buttonFill color.Color) {
	if light {
		return palette[colSelectionFill].rgb[lightPalette], palette[colButtonText].rgb[lightPalette], palette[colAccent].rgb[lightPalette]
	}
	return palette[colSelectionFill].rgb[darkPalette], palette[colButtonText].rgb[darkPalette], palette[colAccent].rgb[darkPalette]
}

// ProjectColour is the configured project accent drawn over the terminal.
func ProjectColour(light bool, hex string) color.Color {
	theme := makeRenderTheme(false, nil, light)
	return projectColour(&snapshot.Project{Color: hex}, &theme)
}

// PalettePaints exposes the resolved colours the renderer uses for both
// surfaces. The sweep can exercise all roles without rebuilding a frame
// for each selectable row on each reported background.
func PalettePaints(background color.Color, indexed bool) (bar color.Color, terminal, selected []color.Color) {
	theme := makeRenderTheme(indexed, background, false)
	terminal = make([]color.Color, colSelectionFill)
	selected = make([]color.Color, colSelectionFill)
	for role := colText; role < colSelectionFill; role++ {
		style := lipgloss.NewStyle().Foreground(role)
		terminal[role] = theme.style(style).GetForeground()
		selected[role] = theme.style(style.Background(colSelectionFill)).GetForeground()
	}
	return theme.bar, terminal, selected
}

// ReadableInk exercises a pair of palette shades against a surface.
var ReadableInk = readableInk

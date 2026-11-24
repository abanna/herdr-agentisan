package dashboard

import (
	"image/color"
	"math"
	"sort"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

const textContrast = 4.5

func contrast(foreground, background float64) float64 {
	return (max(foreground, background) + 0.05) / (min(foreground, background) + 0.05)
}

func relativeLuminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	linear := func(channel uint32) float64 {
		srgb := float64(channel) / 65535
		if srgb <= 0.04045 {
			return srgb / 12.92
		}
		return math.Pow((srgb+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
}

func lightTextHasHigherContrast(background, darkText, lightText float64) bool {
	return contrast(lightText, background) > contrast(darkText, background)
}

const (
	indexedBlack ansi.IndexedColor = 16
	indexedWhite ansi.IndexedColor = 231
)

func neutralInk(background float64, indexed bool) color.Color {
	white := contrast(1, background) > contrast(0, background)
	switch {
	case indexed && white:
		return indexedWhite
	case indexed:
		return indexedBlack
	case white:
		return color.White
	default:
		return color.Black
	}
}

func readableInk(preferred, alternate color.Color, background float64, indexed bool) color.Color {
	if contrast(relativeLuminance(preferred), background) >= textContrast {
		return preferred
	}
	if contrast(relativeLuminance(alternate), background) >= textContrast {
		return alternate
	}
	return neutralInk(background, indexed)
}

// renderTheme resolves the palette once per frame. Foreground roles are
// resolved against the reported terminal background, selection, or button.
type renderTheme struct {
	background float64
	indexed    bool
	terminal   [colourRoles]color.Color
	selected   [colourRoles]color.Color
	bar        color.Color
	button     color.Color
	buttonInk  color.Color
}

var assumedBackground = [2]color.Color{lipgloss.Color("#1e1e2e"), lipgloss.Color("#eaeaea")}

func makeRenderTheme(indexed bool, background color.Color, light bool) renderTheme {
	side := darkPalette
	var luminance float64
	if background == nil {
		if light {
			side = lightPalette
		}
		luminance = relativeLuminance(assumedBackground[side])
	} else {
		luminance = relativeLuminance(background)
		text := palette[colText]
		if lightTextHasHigherContrast(luminance, relativeLuminance(text.paint(darkPalette, indexed)),
			relativeLuminance(text.paint(lightPalette, indexed))) {
			side = lightPalette
		}
	}
	other := lightPalette - side
	theme := renderTheme{
		background: luminance,
		indexed:    indexed,
		bar:        palette[colSelectionFill].paint(side, indexed),
		button:     palette[colAccent].paint(side, indexed),
	}
	barLuminance := relativeLuminance(theme.bar)
	for role := range colSelectionFill {
		c := palette[role]
		preferred, alternate := c.paint(side, indexed), c.paint(other, indexed)
		theme.terminal[role] = readableInk(preferred, alternate, luminance, indexed)
		if indexed && side == lightPalette && c.lightSelectionIndex != 0 {
			preferred = c.lightSelectionIndex
		}
		theme.selected[role] = readableInk(preferred, alternate, barLuminance, indexed)
	}
	button := palette[colButtonText]
	theme.buttonInk = readableInk(button.paint(side, indexed), button.paint(other, indexed),
		relativeLuminance(theme.button), indexed)
	return theme
}

func (theme *renderTheme) style(st lipgloss.Style) lipgloss.Style {
	bg, _ := st.GetBackground().(colourRole)
	switch bg {
	case colSelectionFill:
		st = st.Background(theme.bar)
	case colAccent:
		st = st.Background(theme.button)
	default:
		// Use the terminal's default background.
	}
	if fg, ok := st.GetForeground().(colourRole); ok {
		var ink color.Color
		switch {
		case fg == colButtonText:
			ink = theme.buttonInk
		case bg == colSelectionFill:
			ink = theme.selected[fg]
		default:
			ink = theme.terminal[fg]
		}
		st = st.Foreground(ink)
	}
	return st
}

// projectColour chooses one readable shade for the team name and border.
// A nil theme uses the configured colour on the legacy profiles.
func projectColour(p *snapshot.Project, theme *renderTheme) color.Color {
	if p == nil || !snapshot.ValidColor(p.Color) {
		return nil
	}
	ink := lipgloss.Color(p.Color)
	if theme == nil {
		return ink
	}
	background := theme.background
	if contrast(relativeLuminance(ink), background) < textContrast {
		ink = blendForContrast(ink, background)
	}
	if theme.indexed {
		return nearestReadableIndex(ink, background)
	}
	return ink
}

func colourChannels(c color.Color) (int, int, int) {
	r, g, b, _ := c.RGBA()
	return int(r >> 8), int(g >> 8), int(b >> 8)
}

func blendForContrast(ink color.Color, background float64) color.Color {
	r, g, b := colourChannels(ink)
	endpoint, _, _ := colourChannels(neutralInk(background, false))
	const steps = 255
	blend := func(step int) color.RGBA {
		channel := func(v int) uint8 {
			return uint8((v*(steps-step) + endpoint*step + steps/2) / steps) // #nosec G115 -- A rounded blend of two 8-bit channels stays in [0,255].
		}
		return color.RGBA{R: channel(r), G: channel(g), B: channel(b), A: 255}
	}
	// The starting contrast is below the floor. Moving every channel
	// toward the same endpoint can cross the floor at most once.
	step := sort.Search(steps, func(step int) bool {
		return contrast(relativeLuminance(blend(step)), background) >= textContrast
	})
	return blend(step)
}

type indexedSample struct {
	r, g, b   int
	luminance float64
}

// The first 16 entries are configurable terminal slots. The standard xterm
// cube and greys give us reproducible RGB and both contrast endpoints.
var standardIndexed = func() [240]indexedSample {
	var samples [240]indexedSample
	for i := range samples {
		c := ansi.IndexedColor(i + int(indexedBlack))
		r, g, b := colourChannels(c)
		samples[i] = indexedSample{r: r, g: g, b: b, luminance: relativeLuminance(c)}
	}
	return samples
}()

func nearestReadableIndex(ink color.Color, background float64) ansi.IndexedColor {
	r, g, b := colourChannels(ink)
	best, bestDistance := indexedBlack, math.MaxInt
	for i, c := range standardIndexed {
		if contrast(c.luminance, background) < textContrast {
			continue
		}
		dr, dg, db := c.r-r, c.g-g, c.b-b
		distance := dr*dr + dg*dg + db*db
		if distance < bestDistance {
			best, bestDistance = ansi.IndexedColor(i+int(indexedBlack)), distance
		}
	}
	return best
}

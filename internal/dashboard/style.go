package dashboard

import (
	"cmp"
	"image/color"
	"slices"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// The palette. Colours are written as 24-bit; bubbletea downsamples them to
// whatever the terminal supports.
var (
	colGreen  = lipgloss.Color("#50fa7b")
	colYellow = lipgloss.Color("#f1fa8c")
	colOrange = lipgloss.Color("#ffb86c")
	colRed    = lipgloss.Color("#ff5555")
	colCyan   = lipgloss.Color("#8be9fd")
	colDim    = lipgloss.Color("#6c7086")
	colAccent = lipgloss.Color("#bd93f9")
)

var (
	plainStyle  = lipgloss.NewStyle()
	dimStyle    = lipgloss.NewStyle().Foreground(colDim)
	boldStyle   = lipgloss.NewStyle().Bold(true)
	accentStyle = lipgloss.NewStyle().Foreground(colAccent)
	errorStyle  = lipgloss.NewStyle().Foreground(colRed)
	noteStyle   = lipgloss.NewStyle().Foreground(colYellow)
	greenStyle  = lipgloss.NewStyle().Foreground(colGreen)
	orangeStyle = lipgloss.NewStyle().Foreground(colOrange)
	// buttonStyle is the btop button: bold on the accent, never reverse
	// video, which marks the selected agent alone.
	buttonStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#282a36")).Background(colAccent)
)

// Thresholds are the context levels of ADR-001 D7, in percent. ctx below
// Soft is green, below Hard yellow, below Ceiling orange, and red from
// Ceiling up.
type Thresholds struct {
	Soft, Hard, Ceiling int
}

// DefaultThresholds are D7's defaults: 55, 65 and 75.
func DefaultThresholds() Thresholds {
	return Thresholds{Soft: 55, Hard: 65, Ceiling: 75}
}

// colour is the colour of a ctx value.
func (t Thresholds) colour(ctx int) color.Color {
	switch {
	case ctx >= t.Ceiling:
		return colRed
	case ctx >= t.Hard:
		return colOrange
	case ctx >= t.Soft:
		return colYellow
	default:
		return colGreen
	}
}

// StatusIcon is the glyph drawn for a status. Working and idle match
// ADR-001's frozen $team token (`N · ◐w ●i[ ⚠b][ ⟳r]`), so the sidebar and
// the dashboard agree.
func StatusIcon(s snapshot.Status) string {
	switch s {
	case snapshot.StatusWorking:
		return "◐"
	case snapshot.StatusIdle:
		return "●"
	case snapshot.StatusBlocked:
		return "⚠"
	case snapshot.StatusDone:
		return "✔"
	case snapshot.StatusReady:
		return "⟳"
	default:
		return "?"
	}
}

// ciGlyph is a PR's CI state: ✓ passed, ✗ failed, ⏳ pending, ? unknown.
func ciGlyph(c snapshot.CIState) seg {
	switch c {
	case snapshot.CISuccess:
		return seg{"✓", greenStyle}
	case snapshot.CIFailure:
		return seg{"✗", errorStyle}
	case snapshot.CIPending:
		return seg{"⏳", noteStyle}
	default:
		return seg{"?", dimStyle}
	}
}

// codexGlyph is where Codex's review stands: 👍 approved, 💬 findings to
// answer, … not yet reviewed, ? unknown.
func codexGlyph(c snapshot.CodexState) seg {
	switch c {
	case snapshot.CodexApproved:
		return seg{"👍", plainStyle}
	case snapshot.CodexFindings:
		return seg{"💬", orangeStyle}
	case snapshot.CodexPending:
		return seg{"…", dimStyle}
	default:
		return seg{"?", dimStyle}
	}
}

// mergeWord is a PR's merge state as GitHub names it, coloured by whether
// it can merge.
func mergeWord(m snapshot.MergeState) seg {
	switch m {
	case snapshot.MergeClean:
		return seg{string(m), greenStyle}
	case snapshot.MergeDirty:
		return seg{string(m), errorStyle}
	case snapshot.MergeBlocked, snapshot.MergeBehind:
		return seg{string(m), noteStyle}
	case snapshot.MergeUnstable:
		return seg{string(m), orangeStyle}
	default:
		return seg{clean(string(m)), dimStyle}
	}
}

// statusLabel names a status in a row and in the help.
func statusLabel(s snapshot.Status) string {
	if s.Valid() {
		return string(s)
	}
	return string(snapshot.StatusUnknown)
}

func statusStyle(s snapshot.Status) lipgloss.Style {
	switch s {
	case snapshot.StatusWorking:
		return lipgloss.NewStyle().Foreground(colGreen)
	case snapshot.StatusIdle:
		return lipgloss.NewStyle().Foreground(colYellow)
	case snapshot.StatusBlocked:
		return lipgloss.NewStyle().Foreground(colRed)
	case snapshot.StatusDone:
		return dimStyle
	case snapshot.StatusReady:
		return lipgloss.NewStyle().Foreground(colCyan)
	default:
		return plainStyle
	}
}

// ShortModel is the model as a row shows it, four cells at most: opus, son
// or hai for Claude's families wherever they appear in the name, otherwise
// the first word, lower-cased, cut to four cells (two wide characters).
func ShortModel(model string) string {
	m := strings.ToLower(clean(model))
	switch {
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "son"
	case strings.Contains(m, "haiku"):
		return "hai"
	}
	words := strings.Fields(m)
	if len(words) == 0 {
		return ""
	}
	return ansi.Truncate(words[0], modelCells, "")
}

// groupRank is the team's working order. Groups not named here follow,
// alphabetically.
var groupRank = map[string]int{
	"coders": 0, "precheck": 1, "qa": 2, "codex": 3, "research": 4, "clerk": 5, "debugger": 6,
}

// Order returns the groups in display order, leaving groups untouched: the
// known groups in the team's working order, then the rest alphabetically
// (case-insensitive). Groups that compare equal keep their snapshot order.
func Order(groups []snapshot.Group) []snapshot.Group {
	out := slices.Clone(groups)
	slices.SortStableFunc(out, func(a, b snapshot.Group) int {
		ra, aKnown := groupRank[strings.ToLower(a.Name)]
		rb, bKnown := groupRank[strings.ToLower(b.Name)]
		switch {
		case aKnown && bKnown:
			return cmp.Compare(ra, rb)
		case aKnown:
			return -1
		case bKnown:
			return 1
		default:
			return cmp.Or(
				cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
				cmp.Compare(a.Name, b.Name),
			)
		}
	})
	return out
}

// clean makes snapshot text safe to draw: control characters, escape
// sequences among them, become spaces, so a field can neither break the
// grid onto a new line nor drive the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

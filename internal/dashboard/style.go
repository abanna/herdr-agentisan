package dashboard

import (
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
	// stageReadyStyle and stageRedStyle are a stage's READY and RED, v12's
	// bold green and bold red.
	stageReadyStyle = lipgloss.NewStyle().Bold(true).Foreground(colGreen)
	stageRedStyle   = lipgloss.NewStyle().Bold(true).Foreground(colRed)
	// staleStyle is a stale stage and its age (ADR-001 A42): dim, faint and
	// italic, which no current stage is. Not every terminal draws faint or
	// italic, so the age's own words, "3h ago", say the stage is old.
	staleStyle = lipgloss.NewStyle().Foreground(colDim).Faint(true).Italic(true)
)

// agentStageStyle is a's stage's style in a row and on the boss line:
// staleStyle when the stage is stale, which takes no rule's colour, so an
// old "blocked" never shows red (A42's F4), else stageStyle.
func agentStageStyle(a snapshot.Agent) lipgloss.Style {
	if a.StageStale {
		return staleStyle
	}
	return stageStyle(clean(a.Stage))
}

// stageStyle is a stage's colour in a row and on the boss line: its rule's
// (stageRule), or dim when it matches none.
func stageStyle(stage string) lipgloss.Style {
	if st, ok := stageRule(stage); ok {
		return st
	}
	return dimStyle
}

// stageRule is the colour of the first of v12's rules a stage matches,
// made case-insensitive and word-bounded (ADR-001 A38), tried in v12's
// order: bold green when a word is "ready"; else bold red when one is "red"
// or "blocked"; else yellow when one is "waiting" or "queued". ok is false
// when it matches none. A word is a run of letters, marks and digits, so
// "ready_to_merge" is ready, "already" is not, and "restored" is not red.
func stageRule(stage string) (st lipgloss.Style, ok bool) {
	words := strings.FieldsFunc(stage, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsDigit(r)
	})
	has := func(targets ...string) bool {
		return slices.ContainsFunc(words, func(w string) bool {
			return slices.ContainsFunc(targets, func(want string) bool { return strings.EqualFold(w, want) })
		})
	}
	switch {
	case has("ready"):
		return stageReadyStyle, true
	case has("red", "blocked"):
		return stageRedStyle, true
	case has("waiting", "queued"):
		return noteStyle, true
	default:
		return plainStyle, false
	}
}

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

// StatusIcon is the glyph drawn for a status. Working and idle are the
// glyphs v12's $team used (`N · ◐w ●i[ ⚠b][ ⟳r]`, ADR-001 A17), before A28
// spelled it out.
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

// prMark is a PR's one mark, herdr-status.sh's (ADR-001 A29). CI decides
// first: ✗ a check failed, ⏳ checks pending, ? a CI state it cannot read.
// Once every check passed: ✓ with a 👍 and a clean merge, 💬 when Codex has
// findings to answer, ? for a Codex state it cannot read, and otherwise …,
// waiting for the 👍 (Codex's, or on a repo without Codex the reviewer's)
// or for a clean merge.
func prMark(pr snapshot.PR) seg {
	switch pr.CI {
	case snapshot.CIFailure:
		return seg{"✗", errorStyle}
	case snapshot.CIPending:
		return seg{"⏳", noteStyle}
	case snapshot.CISuccess:
	default:
		return seg{"?", dimStyle}
	}
	switch {
	case pr.Codex == snapshot.CodexApproved && pr.Merge == snapshot.MergeClean:
		return seg{"✓", greenStyle}
	case pr.Codex == snapshot.CodexFindings:
		return seg{"💬", orangeStyle}
	case pr.Codex == snapshot.CodexUnknown || !pr.Codex.Valid():
		return seg{"?", dimStyle}
	default:
		return seg{"…", dimStyle}
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

// A plain shell's glyph and state word (ADR-001 A38), v12's.
const (
	shellIcon  = "▸"
	shellLabel = "shell"
)

// isShell reports whether a is drawn as a plain shell: it is marked one and
// its status is unknown. A shell with another status, ready from a handoff
// token, is drawn as that status, as $team counts it.
func isShell(a snapshot.Agent) bool {
	return a.Shell && statusLabel(a.Status) == string(snapshot.StatusUnknown)
}

// rowIcon and rowLabel are the glyph and the state word a row draws for a:
// its status's, or a plain shell's. A shell is drawn in unknown's style.
func rowIcon(a snapshot.Agent) string {
	if isShell(a) {
		return shellIcon
	}
	return StatusIcon(a.Status)
}

func rowLabel(a snapshot.Agent) string {
	if isShell(a) {
		return shellLabel
	}
	return statusLabel(a.Status)
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

// ShortModel is the model as a row shows it, modelCells cells at most: opus,
// son, hai or fab for Claude's families wherever they appear in the name, a
// gpt- model as its word and its version (gptLabel: sol6.1 for gpt-6.1-sol),
// cdx for any other model whose name says codex, otherwise the first word,
// lower-cased, cut to wordCells cells (two wide characters).
func ShortModel(model string) string {
	m := strings.ToLower(clean(model))
	switch {
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "son"
	case strings.Contains(m, "haiku"):
		return "hai"
	case strings.Contains(m, "fable"):
		return "fab"
	}
	words := strings.Fields(m)
	if len(words) == 0 {
		return ""
	}
	if rest, ok := strings.CutPrefix(words[0], "gpt-"); ok {
		return gptLabel(rest, strings.Contains(m, "codex"))
	}
	if strings.Contains(m, "codex") {
		return "cdx"
	}
	return ansi.Truncate(words[0], wordCells, "")
}

// kindLabel is the agent herdr detects in a's pane as a row draws it: as
// herdr sent it, made safe to draw, trimmed and cut to modelCells cells
// (ADR-001 A68); "" when none is reported.
func kindLabel(a snapshot.Agent) string {
	return ansi.Truncate(strings.TrimSpace(clean(a.Kind)), modelCells, "")
}

// modelLabel is an overview row's model column and its style: the model as
// ShortModel names it; else, before the agent reports one, the agent herdr
// detects, dim, so the row still says which provider runs in it (A68); else
// "".
func modelLabel(a snapshot.Agent) (string, lipgloss.Style) {
	if m := ShortModel(a.Model); m != "" {
		return m, plainStyle
	}
	return kindLabel(a), dimStyle
}

// gptLabel is a gpt- model, rest being its name after gpt-, as a word and the
// version in front of it: cdx and what comes before codex when the name says
// codex (cdx5.5 for gpt-5.5-codex); else the last part, when it is letters
// only, and what comes before it (sol6.1 for gpt-6.1-sol); else gpt and all
// of rest (gpt5.5 for gpt-5.5). Every such name starts gpt-, and the version
// is what tells gpt-6-sol from gpt-6.1-sol, so a label too wide for its
// column loses its word first, down to one cell, and its version only then.
func gptLabel(rest string, codex bool) string {
	word, version := "gpt", rest
	last := rest[strings.LastIndexByte(rest, '-')+1:]
	switch {
	case codex:
		version, _, _ = strings.Cut(rest, "codex")
		word, version = "cdx", strings.Trim(version, "-")
	case last != "" && !strings.ContainsFunc(last, func(r rune) bool { return !unicode.IsLetter(r) }):
		word, version = last, strings.TrimSuffix(strings.TrimSuffix(rest, last), "-")
	}
	if room := modelCells - ansi.StringWidth(version); ansi.StringWidth(word) > room {
		word = ansi.Truncate(word, max(room, 1), "")
	}
	return ansi.Truncate(word+version, modelCells, "")
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

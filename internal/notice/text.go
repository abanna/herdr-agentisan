package notice

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/abanna/herdr-agentisan/internal/team"
)

// maxField is the most runes a name, item or stage keeps in a line: as much
// as `report stage` accepts.
const maxField = 80

// startedText is the line each team's boss gets once the daemon is up.
const startedText = "agentisan: the daemon started; turns that ended while it was down are not reported"

// who names a worker: its logical id when a row binds it, else the name
// herdr knows its agent by, else its pane.
func who(w team.Worker) string {
	switch {
	case w.Bound:
		return field(w.ID)
	case w.Pane.Name != "":
		return field(w.Pane.Name)
	default:
		return "pane " + field(w.Pane.ID)
	}
}

// details is " · ctx 44% · NERD-5322 · verify": the worker's reported ctx,
// item and stage, each only when it has one (D4: what it reported, never
// what a screen shows).
func details(p team.Pane) string {
	var b strings.Builder
	if n, ok := team.ParseCtx(p.Ctx); ok {
		fmt.Fprintf(&b, " · ctx %d%%", n)
	}
	for _, s := range []string{p.Item, p.Stage} {
		if s = field(s); s != "" {
			b.WriteString(" · " + s)
		}
	}
	return b.String()
}

func turnText(w team.Worker) string {
	return fmt.Sprintf("%s finished a turn (%s)%s", who(w), w.Pane.Status, details(w.Pane))
}

func blockedText(w team.Worker) string {
	return who(w) + " is blocked" + details(w.Pane)
}

func unknownText(w team.Worker) string {
	return who(w) + "'s status is unknown" + details(w.Pane)
}

// goneText names the pane that left too, unless the worker is named by it.
func goneText(w team.Worker) string {
	if !w.Bound && w.Pane.Name == "" {
		return who(w) + " left the team"
	}
	return fmt.Sprintf("%s left the team (pane %s)", who(w), field(w.Pane.ID))
}

// field is s as one line's part: valid UTF-8, every control or format
// character a space, runs of spaces one, trimmed, and at most maxField
// runes, the last "…" when it was longer. A worker writes its own item and
// stage, so neither can break the line or hide in it.
func field(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxField {
		s = string(r[:maxField-1]) + "…"
	}
	return s
}

package team

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// The ctx display (ADR-001 A28): every poll, the daemon writes each agent
// pane's context use as a fixed-width display into one of three tokens, one
// per band, which the user's sidebar rows give a colour each. A colour rule
// cannot band "ctx 56%" itself: gt and lt only match a value that parses
// whole as a number (herdr 0.9.3, src/config/sidebar/rules.rs:159-169). The
// tokens go with source agentisan and TTL, renewed every poll, as `$team`.
const (
	// CtxOKKey holds the display up to CtxWarnAbove, and "ctx  --".
	CtxOKKey = "ctx_ok"
	// CtxWarnKey holds the display above CtxWarnAbove.
	CtxWarnKey = "ctx_warn"
	// CtxHotKey holds the display above CtxHotAbove.
	CtxHotKey = "ctx_hot"
	// CtxWarnAbove and CtxHotAbove are the percentages the bands start
	// above.
	CtxWarnAbove = 55
	CtxHotAbove  = 65
)

// ctxUnknown is the display of a pane whose ctx is absent, expired or not a
// percentage.
const ctxUnknown = "ctx  --"

// CtxDisplay returns the band key and the display for a pane's ctx token as
// its writer left it: "ctx" and the whole percentage right-aligned in four
// cells, so every display is seven characters with its spaces inside, which
// herdr keeps: it trims only a value's ends (src/app/api_helpers.rs:267-275).
// Anything but a whole percentage from 0 to 100 is "ctx  --" in the ok band.
func CtxDisplay(raw string) (key, value string) {
	pct, err := strconv.Atoi(raw)
	if err != nil || pct < 0 || pct > 100 {
		return CtxOKKey, ctxUnknown
	}
	value = fmt.Sprintf("ctx%4s", strconv.Itoa(pct)+"%")
	switch {
	case pct > CtxHotAbove:
		return CtxHotKey, value
	case pct > CtxWarnAbove:
		return CtxWarnKey, value
	default:
		return CtxOKKey, value
	}
}

// PaneTokens are the tokens one pane.report_metadata sets on a pane.
type PaneTokens struct {
	PaneID string
	// Tokens maps key to value; an empty value clears the key.
	Tokens map[string]string
}

// CtxTokens derives the ctx display for every pane in l that herdr's agents
// panel lists, one running an agent herdr detected or one agent.list names
// (Pane.IsAgent), whether or not a resolver places it: the agents of every
// team and of none. A plain shell gets none. Each carries all three band
// keys, the two not in use empty, which clears them, so a pane changing band
// never shows two. A pane herdr detects no agent in shows "ctx  --" (ctxOf).
// They are sorted by pane id.
func CtxTokens(l Layout) []PaneTokens {
	names := agentNames(l.Agents)
	var out []PaneTokens
	for _, p := range l.Panes {
		if !(Pane{Agent: p.Agent, Name: names[p.PaneID]}).IsAgent() {
			continue
		}
		key, value := CtxDisplay(ctxOf(p))
		tokens := map[string]string{CtxOKKey: "", CtxWarnKey: "", CtxHotKey: ""}
		tokens[key] = value
		out = append(out, PaneTokens{PaneID: p.PaneID, Tokens: tokens})
	}
	slices.SortFunc(out, func(a, b PaneTokens) int { return strings.Compare(a.PaneID, b.PaneID) })
	return out
}

// ctxOf is p's ctx token, or "" when herdr detects no agent in p (ADR-001
// A27): nothing clears a Codex's ctx when it exits, and its 24 h TTL would
// keep it in a pane agent.list still names, or a plain shell, long after.
func ctxOf(p herdr.PaneInfo) string {
	if p.Agent == "" {
		return ""
	}
	return p.Tokens[report.CtxKey]
}

// Level is how far a pane's context has gone past its rotation thresholds
// (ADR-001 D7, A33): soft, let the worker finish its unit and rotate it;
// hard, rotate it now; the ceiling, which also alerts the user.
type Level int

// The levels, lowest first.
const (
	LevelNone Level = iota
	LevelSoft
	LevelHard
	LevelCeiling
)

// String is the level's name as notices and events give it.
func (l Level) String() string {
	switch l {
	case LevelSoft:
		return "soft"
	case LevelHard:
		return "hard"
	case LevelCeiling:
		return "ceiling"
	default:
		return "none"
	}
}

// CtxLevel is the highest level ctx has crossed, a level being crossed at
// its threshold or above (A33): thresholds are the soft, hard and ceiling
// percentages, ascending, as settings checks them. Only the first three
// count, and fewer are fewer levels. Unlike the ctx display's bands, which
// start above CtxWarnAbove and CtxHotAbove (A28), 55% is the soft level.
func CtxLevel(ctx int, thresholds []int) Level {
	l := LevelNone
	for i, t := range thresholds[:min(len(thresholds), int(LevelCeiling))] {
		if ctx >= t {
			l = Level(i + 1)
		}
	}
	return l
}

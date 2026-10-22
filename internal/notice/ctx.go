package notice

import (
	"fmt"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// Rearm is how far a known ctx falls below a level's threshold before the
// level is told again (ADR-001 A33): to the threshold less Rearm, or under.
const Rearm = 5

// CtxWatch is the context levels' source (ADR-001 D7, A33): it tells a
// team's boss when a pane's ctx crosses one of its rotation thresholds. Only
// one goroutine uses it, the team poll's.
//
// It covers every agent pane a team places, its boss's included, since the
// boss rotates too. It remembers, for each terminal, the highest level it
// told and that is still armed: a rise past it is told once, at the highest
// level newly crossed, and a level re-arms, with every level above it, once
// a known ctx is at its threshold less Rearm or under. A ctx that is absent,
// expired or not a percentage is unknown, and so is a pane's that herdr
// detects no agent in, which the model blanks (A27): it tells nothing and
// re-arms nothing (D7). The memory is by
// terminal, which a reissued pane id does not carry over (A35), and is held
// in memory only, so a restarted daemon tells each level crossed once more.
type CtxWatch struct {
	thresholds func(project, group string) []int
	// told is each terminal's highest level told and still armed.
	told map[string]team.Level
}

// NewCtxWatch is a CtxWatch that has told nothing yet. thresholds gives a
// pane's soft, hard and ceiling percentages by its team's project and its
// group, "" for the boss.
func NewCtxWatch(thresholds func(project, group string) []int) *CtxWatch {
	return &CtxWatch{thresholds: thresholds, told: map[string]team.Level{}}
}

// Crossing is a level a pane's ctx newly crossed.
type Crossing struct {
	Project string
	// Group is the worker's group; "" for the boss.
	Group  string
	Boss   bool
	Worker team.Worker
	Ctx    int
	Level  team.Level
	// Threshold is the level's percentage.
	Threshold int
}

// Observe is the crossings in m, in model order, each team's boss first. A
// terminal m no longer holds is forgotten.
func (w *CtxWatch) Observe(m team.Model) []Crossing {
	var out []Crossing
	seen := map[string]bool{}
	for _, p := range m.Projects {
		for _, b := range p.Boss {
			if c, ok := w.observe(p.Name, "", true, b, seen); ok {
				out = append(out, c)
			}
		}
		for _, g := range p.Groups {
			for _, wk := range g.Workers {
				if c, ok := w.observe(p.Name, g.Name, false, wk, seen); ok {
					out = append(out, c)
				}
			}
		}
	}
	for term := range w.told {
		if !seen[term] {
			delete(w.told, term)
		}
	}
	return out
}

// observe updates wk's terminal's memory, once per terminal, and is its
// crossing, if it made one.
func (w *CtxWatch) observe(project, group string, boss bool, wk team.Worker, seen map[string]bool) (Crossing, bool) {
	term := wk.Pane.TerminalID
	if term == "" || seen[term] {
		return Crossing{}, false
	}
	seen[term] = true
	n, ok := team.ParseCtx(wk.Pane.Ctx)
	if !ok || !wk.Pane.IsAgent() {
		return Crossing{}, false
	}
	ts := w.thresholds(project, group)
	told := w.told[term]
	for told > team.LevelNone && int(told) <= len(ts) && n <= ts[told-1]-Rearm {
		told--
	}
	cur := team.CtxLevel(n, ts)
	if cur <= told {
		w.told[term] = told
		return Crossing{}, false
	}
	w.told[term] = cur
	return Crossing{Project: project, Group: group, Boss: boss, Worker: wk, Ctx: n, Level: cur, Threshold: ts[cur-1]}, true
}

// Notice is c's line for its boss. alerted says whether the user was shown
// the ceiling's alert, which only a ceiling's line reports. The boss's own
// carries no Worker, so no mute silences it.
func (c Crossing) Notice(alerted bool) Notice {
	kind := map[team.Level]Kind{team.LevelSoft: CtxSoft, team.LevelHard: CtxHard, team.LevelCeiling: CtxCeiling}[c.Level]
	n := Notice{Project: c.Project, Subject: c.Worker.Pane.ID, Kind: kind, Text: ctxText(c, alerted)}
	if !c.Boss {
		n.Worker = WorkerName(c.Worker)
	}
	return n
}

// Alert is the toast that alerts the user to c, a ceiling (D7), with the
// sound herdr gives a request.
func (c Crossing) Alert() herdr.Notification {
	body := fmt.Sprintf("past the %d%% ceiling; its boss is told to rotate it", c.Threshold)
	if c.Boss {
		body = fmt.Sprintf("the boss is past the %d%% ceiling and is told to hand off", c.Threshold)
	}
	return herdr.Notification{Title: fmt.Sprintf("agentisan: %s at %d%% context", who(c.Worker), c.Ctx), Body: body, Sound: "request"}
}

// ctxText is "ha-coder-1 is at 66% context (hard: 65%) · NERD-5261 ·
// verify: rotate it now", or the boss's own "you are at …: hand off now".
func ctxText(c Crossing, alerted bool) string {
	subject := who(c.Worker) + " is"
	if c.Boss {
		subject = "you are"
	}
	var advice string
	switch {
	case c.Level == team.LevelSoft && c.Boss:
		advice = "finish your unit, then hand off"
	case c.Level == team.LevelSoft:
		advice = "let it finish its unit, then rotate it"
	case c.Level == team.LevelHard && c.Boss:
		advice = "hand off now"
	case c.Level == team.LevelHard:
		advice = "rotate it now"
	case c.Boss:
		advice = "hand off now"
	default:
		advice = "act now"
	}
	if c.Level == team.LevelCeiling {
		if alerted {
			advice += "; the user has been alerted"
		} else {
			advice += "; the user's alert was not shown"
		}
	}
	return fmt.Sprintf("%s at %d%% context (%s: %d%%)%s: %s", subject, c.Ctx, c.Level, c.Threshold, workDetails(c.Worker.Pane), advice)
}

// workDetails is " · NERD-5261 · verify": the item and stage the pane
// reported, each only when it has one.
func workDetails(p team.Pane) string {
	var b strings.Builder
	for _, f := range []string{p.Item, p.Stage} {
		if f = field(f); f != "" {
			b.WriteString(" · " + f)
		}
	}
	return b.String()
}

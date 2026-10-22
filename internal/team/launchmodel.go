package team

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// The agents' program names, as argv[0] has them.
const (
	claudeProgram = "claude"
	codexProgram  = "codex"
)

// MaxModelChars is the longest model a row carries, in code points: a herdr
// token value's limit, which item and stage keep too.
const MaxModelChars = report.MaxValueChars

// LaunchModel is the model the agent among procs, a pane's foreground job,
// was started with (ADR-001 A39). The agent is the first process, in herdr's
// order, whose arguments are Claude's (argv[0] named claude) or Codex's
// (argv[0] named codex, or node running codex or codex.js, whose own flags
// follow its script); found reports whether there was one, and pid is it.
//
// Its model is the last --model X or --model=X before any "--", and for
// Codex also -m X, -mX or -m=X; Codex's -c model=, profiles and config file
// are not read. model is "" when it was started with none, or with one that
// is empty, starts with "-", or is not printable UTF-8 of at most
// MaxModelChars code points; U+FFFD, which invalid bytes decode to, is not
// printable here.
func LaunchModel(procs []herdr.Process) (model string, pid uint32, found bool) {
	for _, p := range procs {
		if flags, codex, ok := agentFlags(p.Argv); ok {
			return modelFlag(flags, codex), p.PID, true
		}
	}
	return "", 0, false
}

// agentFlags are argv's flags when it is Claude's or Codex's, and whether it
// is Codex's.
func agentFlags(argv []string) (flags []string, codex, ok bool) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, false, false
	}
	switch filepath.Base(argv[0]) {
	case claudeProgram:
		return argv[1:], false, true
	case codexProgram:
		return argv[1:], true, true
	case "node":
		if len(argv) > 1 {
			if b := filepath.Base(argv[1]); b == codexProgram || b == codexProgram+".js" {
				return argv[2:], true, true
			}
		}
	}
	return nil, false, false
}

// modelFlag is the model flags name, the last flag deciding, or "".
func modelFlag(flags []string, codex bool) string {
	model := ""
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		switch {
		case f == "--":
			return report.ValidModel(model)
		case f == "--model" || codex && f == "-m":
			model = ""
			if i+1 < len(flags) {
				i++
				model = flags[i]
			}
		case strings.HasPrefix(f, "--model="):
			model = strings.TrimPrefix(f, "--model=")
		case codex && strings.HasPrefix(f, "-m"):
			model = strings.TrimPrefix(strings.TrimPrefix(f, "-m"), "=")
		}
	}
	return report.ValidModel(model)
}

// CurrentModels is the model each agent pane of m's teams, bosses and
// workers, runs now, by pane id (ADR-001 A56): the model its agent reported,
// when the report is not older than the agent's process, whose start started
// holds by pane, or when started holds no start for the pane; else the launch
// model launch holds for it (A39). A pane with neither is left out. A model
// reported before its agent started is one an agent before it in the pane
// left, as a Codex's can be for 24 h.
func CurrentModels(m Model, launch map[string]string, started map[string]time.Time) map[string]string {
	out := map[string]string{}
	add := func(ws []Worker) {
		for _, w := range ws {
			if model := currentModel(w.Pane, launch[w.Pane.ID], started[w.Pane.ID]); model != "" {
				out[w.Pane.ID] = model
			}
		}
	}
	for _, pr := range m.Projects {
		add(pr.Boss)
		for _, g := range pr.Groups {
			add(g.Workers)
		}
	}
	return out
}

// currentModel is p's reported model when it is not older than start, the
// agent's, or start is unknown (zero); else launch.
func currentModel(p Pane, launch string, start time.Time) string {
	if p.Model != "" && (start.IsZero() || !p.ModelAt.Before(start)) {
		return p.Model
	}
	return launch
}

// FillModels sets every row of s, the boss's too, to the model byPane holds
// for its pane, CurrentModels' or LaunchModel's: none when it holds none.
func FillModels(s *snapshot.Snapshot, byPane map[string]string) {
	if s.Boss != nil {
		s.Boss.Model = byPane[s.Boss.PaneID]
	}
	for i := range s.Groups {
		for j := range s.Groups[i].Agents {
			s.Groups[i].Agents[j].Model = byPane[s.Groups[i].Agents[j].PaneID]
		}
	}
}

// FillBossStart sets s's boss's started_at to when its pane's agent process
// started, as byPane holds it, in UTC (ADR-001 A47), and leaves it out when
// byPane holds none for that pane: the runtime is never a guess. Workers
// carry no runtime.
func FillBossStart(s *snapshot.Snapshot, byPane map[string]time.Time) {
	if s.Boss == nil {
		return
	}
	s.Boss.StartedAt = nil
	if at, ok := byPane[s.Boss.PaneID]; ok {
		at = at.UTC()
		s.Boss.StartedAt = &at
	}
}

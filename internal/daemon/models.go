package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

const (
	// ModelRetry is how long a pane where no agent process was found, a
	// miss, waits before it is asked again (A39).
	ModelRetry = 60 * time.Second
	// ModelBudget bounds one poll's process_info asks together (A39), so a
	// slow herdr never makes `$team` late.
	ModelBudget = time.Second
)

// launchModels are the model each team's agent pane was started with (A39),
// read from its processes' arguments. An entry is kept while its pane shows
// the same terminal and agent and, where a process's start time can be read,
// the same process, so a poll asks herdr nothing for a pane that has not
// changed. Only the poll's goroutine uses it.
type launchModels struct {
	byPane map[string]launchModel
	// failed holds each pane whose last ask failed, so that is logged once.
	failed map[string]bool
	// overran is set while the budget runs out before every pane is asked,
	// so that is logged once.
	overran bool
}

// launchModel is what one ask found in one pane.
type launchModel struct {
	terminal, agent string
	// pid and start are the agent's process; pid is 0 for a miss.
	pid   uint32
	start uint64
	model string
	// asked is when the pane was asked.
	asked time.Time
}

func newLaunchModels() *launchModels {
	return &launchModels{byPane: map[string]launchModel{}, failed: map[string]bool{}}
}

// agentPanes are the panes of m's teams, workers and bosses, where herdr
// detects an agent, as panes lists them.
func agentPanes(m team.Model, panes []herdr.PaneInfo) []herdr.PaneInfo {
	inTeam := map[string]bool{}
	for _, pr := range m.Projects {
		for _, w := range pr.Boss {
			inTeam[w.Pane.ID] = true
		}
		for _, g := range pr.Groups {
			for _, w := range g.Workers {
				inTeam[w.Pane.ID] = true
			}
		}
	}
	var out []herdr.PaneInfo
	for _, p := range panes {
		if p.Agent != "" && inTeam[p.PaneID] {
			out = append(out, p)
		}
	}
	return out
}

// refresh is one poll's look at panes, each a team's agent pane, and returns
// each pane's model, for the panes that have one. A pane whose entry still
// holds is not asked. Any other pane is asked with pane.process_info, all of
// them within ModelBudget, and a pane the budget leaves out has no model
// until a later poll. A miss is asked again ModelRetry after it was asked.
//
// A failed ask leaves its pane no model, keeps nothing and is asked again at
// the next poll; it is logged once until an ask succeeds, unless the pane
// had closed since the list. Entries of panes not in panes are dropped. It
// returns ErrHerdrGone, and asks nothing more, once a call finds the socket
// no longer names the daemon's server.
//
// startTime reads a process's start time; nil, off Linux and darwin, keeps
// an entry until its pane's terminal or agent changes.
func (lm *launchModels) refresh(ctx context.Context, c herdr.Client, panes []herdr.PaneInfo, startTime func(pid int) (uint64, error), now time.Time, log zerolog.Logger) (map[string]string, error) {
	deadline := time.Now().Add(ModelBudget)
	ask, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// spent is checked by the clock too: a read cut off by the deadline can
	// return before the context records that it has passed.
	spent := func() bool { return ask.Err() != nil || !time.Now().Before(deadline) }
	out := map[string]string{}
	listed := make(map[string]bool, len(panes))
	left := false
	for _, p := range panes {
		listed[p.PaneID] = true
		if e, ok := lm.byPane[p.PaneID]; ok && e.holds(p, startTime, now) {
			if e.model != "" {
				out[p.PaneID] = e.model
			}
			continue
		}
		delete(lm.byPane, p.PaneID)
		if spent() {
			left = true
			continue
		}
		info, err := c.PaneProcessInfo(ask, p.PaneID)
		if errors.Is(err, ErrHerdrGone) {
			return nil, fmt.Errorf("read pane %s's processes: %w", p.PaneID, err)
		}
		if err != nil {
			if spent() {
				left = true
				continue
			}
			if !lm.failed[p.PaneID] && !errors.Is(err, herdr.ErrPaneNotFound) {
				log.Warn().Err(err).Str("pane", p.PaneID).Msg("team poll: read a pane's processes for its model; asked again each poll")
			}
			lm.failed[p.PaneID] = true
			continue
		}
		delete(lm.failed, p.PaneID)
		e := read(info, p, startTime, now)
		lm.byPane[p.PaneID] = e
		if e.model != "" {
			out[p.PaneID] = e.model
		}
	}
	if left && !lm.overran && ctx.Err() == nil {
		log.Warn().Dur("budget", ModelBudget).Msg("team poll: herdr is slow; the panes not asked in time have no model until a later poll")
	}
	lm.overran = left
	for id := range lm.byPane {
		if !listed[id] {
			delete(lm.byPane, id)
		}
	}
	for id := range lm.failed {
		if !listed[id] {
			delete(lm.failed, id)
		}
	}
	return out, nil
}

// read is the entry for pane p from its processes. A pane with no agent
// process, or whose agent's start time cannot be read, as when it has just
// exited, is a miss: no model, asked again ModelRetry later.
func read(info herdr.ProcessInfo, p herdr.PaneInfo, startTime func(pid int) (uint64, error), now time.Time) launchModel {
	e := launchModel{terminal: p.TerminalID, agent: p.Agent, asked: now}
	model, pid, found := team.LaunchModel(info.ForegroundProcesses)
	if !found || pid == 0 {
		return e
	}
	if startTime != nil {
		start, err := startTime(int(pid))
		if err != nil {
			return e
		}
		e.start = start
	}
	e.pid, e.model = pid, model
	return e
}

// holds reports whether e is still pane p's: the same terminal and agent,
// and the same process alive, or for a miss, asked less than ModelRetry ago.
func (e launchModel) holds(p herdr.PaneInfo, startTime func(pid int) (uint64, error), now time.Time) bool {
	if e.terminal != p.TerminalID || e.agent != p.Agent {
		return false
	}
	if e.pid == 0 {
		return now.Sub(e.asked) < ModelRetry
	}
	if startTime == nil {
		return true
	}
	start, err := startTime(int(e.pid))
	return err == nil && start == e.start
}

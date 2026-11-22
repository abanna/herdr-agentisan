package daemon

import (
	"github.com/abanna/herdr-agentisan/internal/attention"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// attentionConfigs keeps the resolved alert settings for the team poll.
type attentionConfigs struct {
	Default  attention.Config
	Projects map[string]attention.Config
}

func newAttentionConfigs(ns settings.NoticeSettings) attentionConfigs {
	cfgs := attentionConfigs{
		Default:  attention.Config(ns.Default.Attention),
		Projects: make(map[string]attention.Config, len(ns.Projects)),
	}
	for project, cfg := range ns.Projects {
		cfgs.Projects[project] = attention.Config(cfg.Attention)
	}
	return cfgs
}

func (c attentionConfigs) For(project string) attention.Config {
	if cfg, ok := c.Projects[project]; ok {
		return cfg
	}
	return c.Default
}

// attentionStateOf combines the pending read with the boss place. The
// tracker can pause when the place is empty or when the poll has no model.
func attentionStateOf(p store.BoardPending, built *team.Model, project, boss string) attention.State {
	state := attention.State{
		Boss: boss, Pending: p.Pending, Oldest: p.Oldest, OldestCeiling: p.OldestCeiling, BossMark: p.BossMark,
		Held: p.Held, OldestHeld: p.OldestHeld, OldestHeldCeiling: p.OldestHeldCeiling,
		NewestCeiling: p.NewestCeiling, NewestCeilingPost: p.NewestCeilingPost, Status: team.StatusUnknown,
	}
	if built == nil {
		return state
	}
	for _, pr := range built.Projects {
		if pr.Name != project {
			continue
		}
		state.Place = len(pr.Boss) > 0
		for _, worker := range pr.Boss {
			if worker.Pane.Name == boss {
				state.Status, state.Agent = worker.Pane.Status, worker.Pane.Agent
				break
			}
		}
		break
	}
	return state
}

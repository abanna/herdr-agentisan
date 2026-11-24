package team

import (
	"time"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// BossSnapshot projects one team's named poll after GateCtx for NERD-5748.
func BossSnapshot(p Project, at time.Time, idle *IdleClock, started map[string]time.Time, rows map[string]store.Worker) snapshot.BossSnapshot {
	out := snapshot.BossSnapshot{V: snapshot.BossVersion, Team: p.Name, Groups: []snapshot.BossGroup{}}
	var boss Pane
	matches := 0
	for _, w := range p.Boss {
		if w.Pane.Name == p.Short+"-boss" {
			boss = w.Pane
			matches++
		}
	}
	if matches == 1 && bossField(boss.ID) != nil {
		start := started[boss.ID]
		out.Boss = &snapshot.BossMember{BossRow: bossRow(boss, at, start, idle)}
		if !start.IsZero() {
			out.Boss.StartedAt = new(start.UTC())
		}
	}

	count := 0
	for _, g := range orderedGroups(p) {
		group := snapshot.BossGroup{Name: g.Name}
		for _, w := range g.Workers {
			if count == snapshot.MaxBossWorkers || bossField(g.Name) == nil || bossField(w.Pane.ID) == nil {
				out.WorkersOmitted++
				continue
			}
			member := snapshot.BossWorker{BossRow: bossRow(w.Pane, at, started[w.Pane.ID], idle)}
			r, ok := rows[w.ID]
			if w.Bound && ok && r.Project == p.Name && r.TerminalID == w.Pane.TerminalID {
				member.ID = bossField(w.ID)
				if member.ID != nil {
					member.Generation = new(r.Generation)
				}
			}
			group.Workers = append(group.Workers, member)
			count++
		}
		if len(group.Workers) > 0 {
			out.Groups = append(out.Groups, group)
		}
	}
	return out
}

func bossRow(p Pane, at, start time.Time, idle *IdleClock) snapshot.BossRow {
	a := row(p, nil, idle, at)
	if bossField(a.Name) == nil {
		a.Name = p.ID
	}
	if start.IsZero() || p.CtxAt.IsZero() || p.CtxAt.After(at) {
		a.Ctx = nil
	}
	stage := bossField(a.Stage)
	if stage == nil {
		a.StageAt, a.StageStale = nil, false
	}
	return snapshot.BossRow{
		PaneID: p.ID, Name: a.Name, Agent: bossField(a.Kind), Status: a.Status,
		Item: bossField(a.Item), Stage: stage, StageAt: a.StageAt,
		StageStale: a.StageStale, IdleSince: a.IdleSince, Ctx: a.Ctx,
	}
}

func bossField(s string) *string {
	if s == "" || len(s) > snapshot.MaxBossFieldBytes || !utf8.ValidString(s) {
		return nil
	}
	return new(s)
}

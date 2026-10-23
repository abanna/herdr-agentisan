package workforce

import (
	"context"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// UpRequest is team up.
type UpRequest struct {
	Selector
}

// GroupFill is what team up did for one group.
type GroupFill struct {
	Name string `json:"name"`
	// Workers is the group's configured default.
	Workers int `json:"workers"`
	// Occupied is how many of its slots were taken before the fill.
	Occupied int `json:"occupied"`
	// Spawned is how many workers it opened for the group.
	Spawned int `json:"spawned"`
}

// UpResult is team up's answer.
type UpResult struct {
	Project string      `json:"project"`
	Groups  []GroupFill `json:"groups"`
	Workers []Spawned   `json:"workers"`
	// Warnings are as worker spawn's.
	Warnings []string `json:"warnings,omitempty"`
	DashboardOutcome
}

// Up fills every configured group of a tabs team up to its workers count,
// filling empty slots only, so a run on a full team spawns nothing (I9), and
// opens the team's dashboard above the boss when none is open. It is refused
// whole, before anything opens, on a failed preflight (A34) or a debugger it
// is to open but cannot launch (A32), as Spawn is.
// Groups fill in the team model's order; every worker's tab opens first, then
// the dashboard, then all the agents start at once. A worker that does not
// come up fails the op with ErrSpawnFailed, as in Spawn; the others stay.
// Routing decides each group's launch as it does a spawn's (A50), once per
// group it fills, under the lock and before any tab opens, so a group it
// refuses refuses the whole up.
func (s *Service) Up(ctx context.Context, req UpRequest) (UpResult, error) {
	_, p, _, err := s.resolve(ctx, req.Selector)
	if err != nil {
		return UpResult{}, err
	}
	// A debugger's launch is built as Spawn builds it, before the preflight
	// and the lock (A32), when its group fills to any worker; a failure
	// refuses the team only if a debugger is to open, which the fill knows.
	var dbg *debugger.Command
	var dbgErr error
	if g, ok := p.Groups[settings.DebuggerType]; ok && !g.DescribesOnly && g.Workers > 0 {
		if c, err := s.debuggerLaunch(p.Repo); err != nil {
			dbgErr = configFailed(err)
		} else {
			dbg = &c
		}
	}
	if err := s.preflight(ctx, p.Repo); err != nil {
		return UpResult{}, err
	}
	var fills []GroupFill
	var routes []Routed
	pending, st, err := s.planAndOpen(ctx, req.Selector, func(st state) ([]planned, error) {
		if st.profile.Repo != p.Repo {
			return nil, fmt.Errorf("%w: the repo changed from %s to %s during the preflight; run team up again", ErrPreflight, p.Repo, st.profile.Repo)
		}
		taken := st.taken()
		var all []planned
		fills, routes = []GroupFill{}, nil
		for _, name := range groupOrder(configured(st.profile), team.TypeOrder()) {
			g := st.profile.Groups[name]
			fill := GroupFill{Name: name, Workers: g.Workers, Occupied: st.occupied(name)}
			if want := g.Workers - fill.Occupied; want > 0 {
				if name == settings.DebuggerType && dbgErr != nil {
					return nil, dbgErr
				}
				ids, err := nextIDs(st.team.Short, name, want, taken)
				if err != nil {
					return nil, err
				}
				d, refused, err := s.route(st.profile, SpawnRequest{Group: name})
				if err != nil {
					return nil, err
				}
				agent, err := agentOf(st.profile, name, "")
				if err != nil {
					return nil, err
				}
				ps, err := planRouted(st.profile, name, choice{model: agent.Model}, d, ids, dbg)
				if err != nil {
					return nil, err
				}
				routes = append(routes, Routed{Op: "team up", Project: st.team.Project, Group: name, Decision: d, Refused: refused})
				all = append(all, ps...)
				fill.Spawned = want
			}
			fills = append(fills, fill)
		}
		return all, nil
	})
	if err != nil {
		return UpResult{}, err
	}
	decided := map[string]*routing.Decision{}
	for _, r := range routes {
		s.tellRouted(ctx, r, pending)
		decided[r.Group] = r.Decision
	}
	res := UpResult{Project: st.team.Project, Groups: fills, Warnings: s.warnings(pending)}
	res.DashboardOutcome = s.ensureDashboard(ctx, st.team.Project)
	res.Workers, err = s.bringUp(ctx, st.profile.Spawn, pending)
	for i := range res.Workers {
		routedResult(&res.Workers[i], decided[res.Workers[i].Group])
	}
	return res, err
}

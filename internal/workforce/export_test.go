package workforce

import (
	"cmp"
	"context"
	"time"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// WaitForPrompt is spawn's wait for paneID's shell prompt, as cfg sets it.
func (s *Service) WaitForPrompt(ctx context.Context, cfg settings.Spawn, paneID string) error {
	w := opened{paneID: paneID}
	return s.waitForPrompt(ctx, cfg, w, &keySender{s: s, w: w, cfg: cfg, started: time.Now()})
}

// Plan is spawn's launch for one worker of typ under p: part 4's choice
// from the request's model, with effort added.
func Plan(p *settings.Profile, typ, model, effort string) (kind string, args []string, err error) {
	ps, err := plan(p, typ, choice{model: cmp.Or(model, p.AgentFor(typ).Model), effort: effort}, []string{"ha-" + typ + "-1"})
	if err != nil {
		return "", nil, err
	}
	return ps[0].launch.kind, ps[0].launch.args, nil
}

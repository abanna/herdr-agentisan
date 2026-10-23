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
	agent, err := agentOf(p, typ)
	if err != nil {
		return "", nil, err
	}
	return PlanRouted(p, typ, cmp.Or(model, agent.Model), effort, "")
}

// PlanRouted is spawn's launch for one worker of typ under p, with the model
// and effort routing chose for provider.
func PlanRouted(p *settings.Profile, typ, model, effort, provider string) (kind string, args []string, err error) {
	ps, err := plan(p, typ, choice{model: model, effort: effort, provider: provider}, []string{"ha-" + typ + "-1"}, nil)
	if err != nil {
		return "", nil, err
	}
	return ps[0].launch.kind, ps[0].launch.args, nil
}

// TurnHooks is the --settings a Claude worker starts with (A42).
const TurnHooks = turnHooks

// WithTurnHooks is the debugger's launch args with the turn hooks in its
// one --settings object.
func WithTurnHooks(args []string) ([]string, error) { return withTurnHooks(args) }

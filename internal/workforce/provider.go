package workforce

import (
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// agentOf is the agent a worker of typ launches under the type's default
// provider (ADR-001 A62). A provider the type cannot run yet, which Load
// already refuses, is the configuration's error.
func agentOf(p *settings.Profile, typ string) (settings.Agent, error) {
	a, err := p.AgentFor(typ, "")
	if err != nil {
		return settings.Agent{}, fmt.Errorf("%w: %s: %w", ErrConfig, typ, err)
	}
	return a, nil
}

// checkFamily refuses a launch whose model's name says another provider runs
// it than the agent kind launched (A62), whoever chose the model: the
// group, the boss's --model while routing is off, or routing. It names the
// families, never the model. A name settings.ModelFamily does not know
// passes.
func checkFamily(typ, model, kind string) error {
	if f := settings.ModelFamily(model); f != "" && f != kind {
		return fmt.Errorf("%w: %s's agent model is a %s model, and its agent runs %s", ErrConfig, typ, f, kind)
	}
	return nil
}

package workforce

import (
	"cmp"
	"errors"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// agentOf is the agent a worker of typ launches under provider (ADR-001
// A62), "" being the type's default. A provider the spawn asked for that the
// type cannot run yet is settings.ErrProviderRefused, the boss's to fix
// (A66); the type's default being refused, which Load already refuses, is
// the configuration's error.
func agentOf(p *settings.Profile, typ, provider string) (settings.Agent, error) {
	a, err := p.AgentFor(typ, provider)
	switch {
	case err == nil:
		return a, nil
	case provider != "" && errors.Is(err, settings.ErrProviderRefused):
		return settings.Agent{}, fmt.Errorf("%s: %w", typ, err)
	}
	return settings.Agent{}, fmt.Errorf("%w: %s: %w", ErrConfig, typ, err)
}

// providerOf is the provider a spawn runs (A66): --provider, else the task
// packet's provider_preference, else "" for the type's default. Each is
// claude or codex; a --provider the packet's preference disagrees with is a
// mismatch, whether or not routing is on, since the provider is the spawn's
// choice, not routing's.
func providerOf(req SpawnRequest) (string, error) {
	if req.Provider != "" && req.Provider != routing.Claude && req.Provider != routing.Codex {
		return "", fmt.Errorf("%w: --provider is neither %s nor %s", ErrBadRequest, routing.Claude, routing.Codex)
	}
	var preferred string
	if req.Task != nil {
		preferred = req.Task.ProviderPreference
	}
	if req.Provider != "" && preferred != "" && preferred != req.Provider {
		return "", fmt.Errorf("%w: --provider is %s, and the task packet's provider_preference is %s", routing.ErrProviderMismatch, req.Provider, preferred)
	}
	return cmp.Or(req.Provider, preferred), nil
}

// modelHint follows a refusal of the boss's --model for its provider:
// --model checks the provider, and never chooses it (A66).
const modelHint = "; --model never chooses the provider: pass --provider"

// checkAsked refuses the boss's --model, while routing does not decide the
// spawn, when its name says the other provider runs it than the spawn's
// (A66): --model checks the provider, never chooses it. It names the
// families, never the model. A model the configuration gave goes through
// checkFamily instead, the configuration's error.
func checkAsked(typ, model, kind string) error {
	if f := settings.ModelFamily(model); f != "" && f != kind {
		return fmt.Errorf("%w: --model is a %s model, and %s runs %s here%s %s", routing.ErrProviderMismatch, f, typ, kind, modelHint, f)
	}
	return nil
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

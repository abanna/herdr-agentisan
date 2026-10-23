package workforce

import (
	"context"
	"errors"
	"time"

	"github.com/abanna/herdr-agentisan/internal/jev"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// JevAsker asks Jev with the key from an env file: jev.Asker, which the
// daemon gives the service (A57).
type JevAsker interface {
	Ask(ctx context.Context, envFile string, body []byte, timeout time.Duration) (jev.Answer, error)
}

// keyError is a failure to get Jev's key as routing reads it: ErrJevKey, in
// the failure's own words, which never hold the key.
type keyError struct{ error }

func (keyError) Is(target error) bool { return target == routing.ErrJevKey }

// askJev asks Jev about d, a spawn's decision on p, and returns d with
// routing's mapping of the answer or the failure (A57). Only a decision that
// AsksJev, while p's policy has Jev enabled, is asked: once, within the
// rules' timeout. Any other is returned as it is, as is every decision of a
// service with no Jev. It reads the decision, its provider included, never
// the group. No key, the env file's ~/ with no HOME included, keeps the
// default as jev_key_unavailable.
func (s *Service) askJev(ctx context.Context, p *settings.Profile, d routing.Decision, task *routing.Task) routing.Decision {
	if s.Jev == nil || !d.AsksJev() {
		return d
	}
	// route decided d by this policy, so it resolves; one that does not
	// asks nothing.
	policy, err := p.Policy()
	if err != nil || !policy.JevEnabled() {
		return d
	}
	body, err := policy.JevRequest(task)
	if err != nil {
		return policy.ApplyJev(d, "", nil, err)
	}
	envFile, err := p.JevEnvFile(s.LookupEnv)
	if err != nil {
		return policy.ApplyJev(d, "", nil, keyError{err})
	}
	ans, err := s.Jev.Ask(ctx, envFile, body, policy.JevTimeout())
	if errors.Is(err, jev.ErrNoKey) {
		err = keyError{err}
	}
	return policy.ApplyJev(d, ans.Model, ans.Answers, err)
}

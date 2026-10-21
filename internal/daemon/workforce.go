package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// The workforce ops (ADR-001 D11, A23). Each takes its request as args and
// has a budget of its own, past the 5 s exchange every other op fits in.
const (
	OpTeamStatus = "team.status"
)

// StatusBudget is team status's budget.
const StatusBudget = 10 * time.Second

// notReadyRetry is how long Workforce waits before asking again a daemon
// that answered not_ready.
const notReadyRetry = 100 * time.Millisecond

// The refusal codes of the workforce ops, one per workforce sentinel.
var workforceCodes = []struct {
	err  error
	code string
}{
	{ErrNotReady, codeNotReady},
	{workforce.ErrBadRequest, codeBadRequest},
	{workforce.ErrUnknownProject, "unknown_project"},
	{settings.ErrBypassesAgentisan, "bypasses_agentisan"},
	{workforce.ErrConfig, "config_invalid"},
	{ErrHerdrGone, "herdr_gone"},
	{workforce.ErrHerdr, codeHerdrFailed},
	{store.ErrStore, "store_failed"},
}

// workforceCode is the refusal code for a workforce op's error.
func workforceCode(err error) string {
	for _, c := range workforceCodes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return "failed"
}

// workforceOps are the workforce ops on svc. Each refuses not_ready until
// ready is set, once the team poll has reconciled the workers rows since the
// daemon started (A35): before that, a worker a live handoff re-keyed reads
// as a pane no row binds.
func workforceOps(svc *workforce.Service, ready *atomic.Bool) map[string]op {
	fixed := func(d time.Duration) func(json.RawMessage) time.Duration {
		return func(json.RawMessage) time.Duration { return d }
	}
	ops := map[string]op{
		OpTeamStatus: {budget: fixed(StatusBudget), code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req workforce.StatusRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return svc.Status(ctx, req)
		}},
	}
	for name, o := range ops {
		run := o.run
		o.run = func(ctx context.Context, args json.RawMessage) (any, error) {
			if !ready.Load() {
				return nil, fmt.Errorf("%w: the team poll has not reconciled the workers rows since the daemon started", ErrNotReady)
			}
			return run(ctx, args)
		}
		ops[name] = o
	}
	return ops
}

// OpBudget is how long op may run; zero for an op that is not a workforce
// op. A client waits that long, and an exchange more.
func OpBudget(op string, _ json.RawMessage) time.Duration {
	if op == OpTeamStatus {
		return StatusBudget
	}
	return 0
}

// decodeArgs decodes a request's args strictly: a key the op does not take is
// a typo until proven otherwise.
func decodeArgs(args json.RawMessage, out any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: args: %w", workforce.ErrBadRequest, err)
	}
	return nil
}

// newWorkforce is the daemon's workforce service, or nil when the team poll
// is off: every herdr call it makes proves the server's identity first (A3).
func (o Options) newWorkforce(st *store.Store, ident Identity, idle *team.IdleClock) *workforce.Service {
	if o.Team == nil {
		return nil
	}
	return &workforce.Service{
		Herdr:     herdr.Client{SocketPath: o.HerdrSocket, Dialed: o.onServer(ident)},
		Store:     st,
		ConfigDir: o.Team.ConfigDir,
		Idle:      idle,
		Now:       o.Now,
	}
}

// Workforce runs a workforce op on the daemon at socket with args and returns
// the data of its answer, waiting the op's budget and an exchange more. A
// refusal is a RequestError with the op's code. A daemon that has not
// reconciled its workers rows since it started answers not_ready at once
// (A35); Workforce asks again, within that same wait, so a command run just
// after a daemon start waits for its first poll and still never waits longer
// than its budget and an exchange.
func Workforce(ctx context.Context, socket, op string, args any) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: encode args: %w", ErrProtocol, op, err)
	}
	wait := OpBudget(op, raw) + ioTimeout
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		data, err := callWith(ctx, socket, op, raw, wait)
		if !errors.Is(err, ErrNotReady) {
			return data, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(notReadyRetry):
		}
	}
}

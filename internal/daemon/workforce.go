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
	OpTeamStatus   = "team.status"
	OpWorkerRetire = "worker.retire"
)

// The workforce ops' budgets. A handoff wait gets 1,500 s of the skill's
// wrapper's 1,800.
const (
	StatusBudget  = 10 * time.Second
	RetireBudget  = 10 * time.Second
	HandoffBudget = 1500 * time.Second
)

// notReadyRetry is how long Workforce waits before asking again a daemon
// that answered not_ready.
const notReadyRetry = 100 * time.Millisecond

// opBudgets is each workforce op's budget given its args, the one table the
// server and its client both read.
var opBudgets = map[string]func(json.RawMessage) time.Duration{
	OpTeamStatus:   fixed(StatusBudget),
	OpWorkerRetire: retireBudget,
}

func fixed(d time.Duration) func(json.RawMessage) time.Duration {
	return func(json.RawMessage) time.Duration { return d }
}

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
	{workforce.ErrUnknownWorker, "unknown_worker"},
	{workforce.ErrNotAWorker, "not_a_worker"},
	{workforce.ErrPaneChanged, "pane_changed"},
	{workforce.ErrHandoffPending, "handoff_pending"},
	{workforce.ErrHandoffTimeout, "handoff_timeout"},
	{ErrHerdrGone, "herdr_gone"},
	{workforce.ErrHerdr, codeHerdrFailed},
	{store.ErrStore, codeStoreFailed},
}

// codeStoreFailed answers an op whose state.db call failed.
const codeStoreFailed = "store_failed"

// workforceCode is the refusal code for a workforce op's error.
func workforceCode(err error) string {
	for _, c := range workforceCodes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return codeFailed
}

// workforceOps are the workforce ops on svc. Each refuses not_ready until
// ready is set, once the team poll has reconciled the workers rows since the
// daemon started (A35): before that, a worker a live handoff re-keyed reads
// as a pane no row binds.
func workforceOps(svc *workforce.Service, ready *atomic.Bool) map[string]op {
	ops := map[string]op{
		OpTeamStatus: {code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req workforce.StatusRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return svc.Status(ctx, req)
		}},
		OpWorkerRetire: {code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req workforce.RetireRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return svc.Retire(ctx, req)
		}},
	}
	for name, o := range ops {
		o.budget = opBudgets[name]
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

// retireBudget is a retire's budget: a handoff wait gets HandoffBudget.
func retireBudget(args json.RawMessage) time.Duration {
	var req struct {
		AfterHandoff bool `json:"after_handoff"`
	}
	if json.Unmarshal(args, &req) == nil && req.AfterHandoff {
		return HandoffBudget
	}
	return RetireBudget
}

// OpBudget is how long op may run; zero for an op that is not a workforce
// op. A client waits that long, and an exchange more.
func OpBudget(op string, args json.RawMessage) time.Duration {
	if b, ok := opBudgets[op]; ok {
		return b(args)
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
// than its budget and an exchange. A deadline or a cancel that cuts short an
// ask after a not_ready refusal returns that refusal: the daemon answered,
// and was not ready, which no restart would change.
func Workforce(ctx context.Context, socket, op string, args any) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: encode args: %w", ErrProtocol, op, err)
	}
	wait := OpBudget(op, raw) + ioTimeout
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var refused error
	for {
		data, err := callWith(ctx, socket, op, raw, wait)
		if refused != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return nil, refused
		}
		if !errors.Is(err, ErrNotReady) {
			return data, err
		}
		refused = err
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(notReadyRetry):
		}
	}
}

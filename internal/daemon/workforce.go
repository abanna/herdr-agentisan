package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// The workforce ops (ADR-001 D11, A23). Each takes its request as args and
// has a budget of its own, past the 5 s exchange every other op fits in.
const (
	OpTeamStatus   = "team.status"
	OpTeamUp       = "team.up"
	OpWorkerSpawn  = "worker.spawn"
	OpWorkerRetire = "worker.retire"
	// OpSend queues a message (A30).
	OpSend = "send"
)

// The workforce ops' budgets. A command waits at most NotReadyWindow, its
// op's budget and an exchange (A52), and the skill's wrapper gives it 120 s,
// or 1,800 s for a handoff wait; each sum leaves the wrapper 5 s. So a
// spawn, which waits for agents to start, gets 100 s: 10 + 100 + 5 = 115,
// and team up, which starts a team's, gets a spawn's budget. A handoff
// wait gets 1,500 s.
const (
	StatusBudget  = 10 * time.Second
	SpawnBudget   = 100 * time.Second
	RetireBudget  = 10 * time.Second
	HandoffBudget = 1500 * time.Second
	SendBudget    = 10 * time.Second
)

// notReadyRetry is how long Workforce waits before asking again a daemon
// that answered not_ready.
const notReadyRetry = 100 * time.Millisecond

// NotReadyWindow is how long Workforce goes on asking a daemon that answers
// not_ready (A52). It bounds only the waits between asks: every ask gets
// the op's whole budget and an exchange.
const NotReadyWindow = 10 * time.Second

// opBudgets is each workforce op's budget given its args, the one table the
// server and its client both read.
var opBudgets = map[string]func(json.RawMessage) time.Duration{
	OpTeamStatus:   fixed(StatusBudget),
	OpTeamUp:       fixed(SpawnBudget),
	OpWorkerSpawn:  fixed(SpawnBudget),
	OpWorkerRetire: retireBudget,
	OpSend:         fixed(SendBudget),
	// The notices ops (A44), which the daemon serves beside the workforce's.
	OpNoticesMute:   fixed(NoticesBudget),
	OpNoticesUnmute: fixed(NoticesBudget),
	OpNoticesList:   fixed(NoticesBudget),
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
	{workforce.ErrNoTarget, "no_target"},
	{workforce.ErrUnknownGroup, "unknown_group"},
	{routing.ErrUnknownRole, "unknown_role"},
	{routing.ErrProviderMismatch, "provider_mismatch"},
	{routing.ErrOverrideRefused, "override_refused"},
	{routing.ErrBlocked, "routing_blocked"},
	{workforce.ErrSlotCap, "slot_cap"},
	{workforce.ErrInvalidID, "invalid_id"},
	{workforce.ErrPreflight, "preflight_failed"},
	{workforce.ErrSpawnFailed, "spawn_failed"},
	{workforce.ErrUnknownRecipient, "unknown_recipient"},
	{workforce.ErrUnknownSender, "unknown_sender"},
	{store.ErrTooLarge, "too_large"},
	{store.ErrInvalid, codeBadRequest},
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

// workforceOps are the workforce ops on svc, and send, which calls queued
// with each message it queues. Each refuses not_ready until ready is set,
// once the team poll has reconciled the workers rows since the daemon
// started (A35): before that, a worker a live handoff re-keyed reads as a
// pane no row binds, and would send as one.
func workforceOps(svc *workforce.Service, ready *atomic.Bool, queued func(context.Context, workforce.SendResult)) map[string]op {
	ops := map[string]op{
		OpTeamStatus: {code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req workforce.StatusRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return svc.Status(ctx, req)
		}},
		OpTeamUp: {code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req workforce.UpRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return svc.Up(ctx, req)
		}},
		OpWorkerSpawn: {code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			// JSON would fold bytes that are not UTF-8 into U+FFFD, and a
			// task packet's assignment is hashed as its bytes (A50).
			if !utf8.Valid(args) {
				return nil, fmt.Errorf("%w: args are not UTF-8", workforce.ErrBadRequest)
			}
			var req workforce.SpawnRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return svc.Spawn(ctx, req)
		}},
		OpWorkerRetire: {code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req workforce.RetireRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return svc.Retire(ctx, req)
		}},
		OpSend: {code: workforceCode, run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req workforce.SendRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			res, err := svc.Send(ctx, req)
			if err != nil {
				return nil, err //nolint:wrapcheck // workforce wraps it, and workforceCode maps its sentinel
			}
			queued(ctx, res)
			return res, nil
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

// OpBudget is how long op may run; zero for an op with no budget of its
// own. A client waits that long, and an exchange more.
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
// A debugger it spawns may read serverDir, the daemon's own directory
// (A32).
func (o Options) newWorkforce(st *store.Store, ident Identity, idle *team.IdleClock, serverDir string) *workforce.Service {
	if o.Team == nil {
		return nil
	}
	return &workforce.Service{
		Herdr:      herdr.Client{SocketPath: o.HerdrSocket, Dialed: o.onServer(ident)},
		Store:      st,
		ConfigDir:  o.Team.ConfigDir,
		Idle:       idle,
		Now:        o.Now,
		Preflight:  o.Hooks.Preflight,
		CodexHooks: o.Team.CodexHooks,
		Dashboard:  o.Team.Dashboard,
		Debugger:   debugger.Launch{Root: o.Team.PluginRoot, Bin: o.Team.Binary, ServerDir: serverDir},
		Routed:     o.routed,
	}
}

// routed records a routing decision of a spawn or of one group team up
// fills (A50): one workflow event per worker and a daemon.log line, named for
// the op, with the whole decision, which holds no assignment text. A refusal
// while routing is off is logged only.
func (o Options) routed(ctx context.Context, r workforce.Routed) {
	log := o.Logger.Info().Str("project", r.Project).Str("group", r.Group).Strs("workers", r.IDs)
	if r.Decision == nil {
		log.AnErr("refused", r.Refused).Msg(r.Op + ": routing is off; when on, it would refuse this")
		return
	}
	log.Interface("routing", r.Decision).Msg(r.Op + " routed")
	for _, id := range r.IDs {
		o.Team.Events.WorkerRouted(ctx, id, r.Project, r.Group, *r.Decision)
	}
}

// Workforce runs a workforce op on the daemon at socket with args and returns
// the data of its answer, giving each ask the op's budget and an exchange
// more. A refusal is a RequestError with the op's code. A daemon that has
// not reconciled its workers rows since it started answers not_ready at once
// (A35); Workforce asks again for NotReadyWindow, so a command run just
// after a daemon start waits for its first poll. The window ends only the
// waits between asks, never an ask: one the daemon took may be running its
// op, which a refusal would hide (A52). So a command never waits longer than
// the window, the budget and an exchange, and no ask starts after the
// window. A deadline or a cancel of the caller's own that cuts short an ask
// after a not_ready refusal returns that refusal (A35): the caller chose to
// stop, and the daemon was not ready when last it answered.
func Workforce(ctx context.Context, socket, op string, args any) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: encode args: %w", ErrProtocol, op, err)
	}
	wait := OpBudget(op, raw) + ioTimeout
	asking, stop := context.WithTimeout(ctx, NotReadyWindow)
	defer stop()
	var refused error
	for {
		data, err := callWith(ctx, socket, op, raw, wait)
		if refused != nil && errors.Is(err, ErrUnavailable) && (ctx.Err() != nil || deadlinePassed(ctx)) {
			return nil, refused
		}
		if !errors.Is(err, ErrNotReady) {
			return data, err
		}
		refused = err
		select {
		case <-asking.Done():
		case <-time.After(notReadyRetry):
		}
		// After the select, not in it: a wait that ends as the window does
		// may pick either case.
		if asking.Err() != nil {
			return nil, err
		}
	}
}

// deadlinePassed reports whether ctx's deadline has passed. An exchange's
// socket timeout, set to that deadline, can end a read a moment before the
// context's own timer makes ctx.Err() say so (#90's F1, as herdr's
// pastDeadline for #78's).
func deadlinePassed(ctx context.Context) bool {
	d, ok := ctx.Deadline()
	return ok && !time.Now().Before(d)
}

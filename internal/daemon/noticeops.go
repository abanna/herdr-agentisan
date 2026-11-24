package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// The notices ops (A44): the boss mutes a worker's notices for a while,
// ends a mute, and lists the mutes in force.
const (
	OpNoticesMute   = "notices.mute"
	OpNoticesUnmute = "notices.unmute"
	OpNoticesList   = "notices.list"
)

// NoticesBudget bounds a notices op, including the mute's worker row check.
const NoticesBudget = 5 * time.Second

// MuteRequest is notices.mute's args: the worker by the name its notices
// carry (notice.WorkerName), and how long, notice.DefaultMute when 0.
type MuteRequest struct {
	Worker string        `json:"worker"`
	For    time.Duration `json:"for,omitempty"`
}

// UnmuteRequest is notices.unmute's args: the worker as mute selects it.
type UnmuteRequest struct {
	Worker string `json:"worker"`
}

// UnmuteResult is notices.unmute's answer: Ended says whether a mute was in
// force. Unmuting a worker with none is no refusal.
type UnmuteResult struct {
	Worker string `json:"worker"`
	Ended  bool   `json:"ended"`
}

// MutesResult is notices.list's answer.
type MutesResult struct {
	Mutes []notice.Mute `json:"mutes"`
}

// noticeOps are the notices ops on mutes. A mute names a worker of the
// model the team poll last gave in, so it waits for the first poll's
// (not_ready).
func (o Options) noticeOps(in *inbox, mutes *notice.Mutes) map[string]op {
	ops := map[string]op{
		OpNoticesMute: {run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req MuteRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			m, _ := in.view()
			if m == nil {
				return nil, fmt.Errorf("%w: the team poll has no model to find %q in", ErrNotReady, req.Worker)
			}
			return mutes.Mute(ctx, *m, req.Worker, req.For, o.Now())
		}},
		OpNoticesUnmute: {run: func(_ context.Context, args json.RawMessage) (any, error) {
			var req UnmuteRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			m, _ := in.view() // no model, no refusal: the stored mutes are searched by name
			return UnmuteResult{Worker: req.Worker, Ended: mutes.Unmute(m, req.Worker, o.Now())}, nil
		}},
		OpNoticesList: {run: func(_ context.Context, args json.RawMessage) (any, error) {
			if err := decodeArgs(args, &struct{}{}); err != nil {
				return nil, err
			}
			return MutesResult{Mutes: mutes.List(o.Now())}, nil
		}},
	}
	for name, op := range ops {
		op.budget = opBudgets[name]
		op.code = noticeCode
		ops[name] = op
	}
	return ops
}

// newNoticeMutes checks the selection from the poll against state.db
// (NERD-5730). The terminal proves an unbound pane is the pending replacement.
func newNoticeMutes(st *store.Store) *notice.Mutes {
	return notice.NewMutes(func(ctx context.Context, worker string, w team.Worker) (int64, bool, error) {
		generation, replacement, err := st.LiveWorkerGeneration(ctx, worker, w.Pane.TerminalID)
		switch {
		case errors.Is(err, store.ErrNoWorker) && !w.Bound:
			return 0, false, nil
		case errors.Is(err, store.ErrNoWorker):
			return 0, false, notice.ErrUnknownWorker
		case err != nil:
			return 0, false, fmt.Errorf("read mute binding: %w", err)
		case w.Bound && generation == w.Generation, !w.Bound && replacement:
			return generation, true, nil
		default:
			// The cached selection cannot mute a different live binding.
			return 0, false, notice.ErrUnknownWorker
		}
	})
}

// noticeCode is the refusal code for a notices op's error.
func noticeCode(err error) string {
	switch {
	case errors.Is(err, notice.ErrUnknownWorker):
		return "unknown_worker"
	case errors.Is(err, notice.ErrMuteLength):
		return codeBadRequest
	}
	return workforceCode(err)
}

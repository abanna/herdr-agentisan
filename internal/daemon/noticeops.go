package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
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

// UnmuteRequest is notices.unmute's args.
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
			return UnmuteResult{Worker: req.Worker, Ended: mutes.Unmute(req.Worker, o.Now())}, nil
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

// newNoticeMutes checks the row selected by the model against state.db.
// A handoff can rebind the terminal while keeping the same generation.
func newNoticeMutes(st *store.Store) *notice.Mutes {
	return notice.NewMutes(func(ctx context.Context, worker string, generation int64) (bool, error) {
		row, err := st.WorkerRow(ctx, worker)
		if errors.Is(err, store.ErrNoWorker) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read worker row: %w", err)
		}
		return row.Generation == generation && row.RetiredAt.IsZero() && row.Status != store.StatusLost, nil
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

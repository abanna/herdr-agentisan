package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// InboxNotice delivers m as the poll's sink does (A44), with bosses, by
// project, as the poll gives them in.
func InboxNotice(o Options, st *store.Store, bosses map[string]string, m notice.Message) error {
	in := newInbox()
	in.bosses = bosses
	return inboxSink{st: st, in: in, log: o.Logger, events: o.Team.Events, now: o.Now}.Send(context.Background(), m)
}

// NoticeOpNames is every op the daemon's notices op map serves.
func NoticeOpNames() []string {
	return slices.Sorted(maps.Keys(Options{}.noticeOps(newInbox(), nil)))
}

// ServedNoticeBudget is the budget the daemon's notices op map gives op.
func ServedNoticeBudget(op string) time.Duration {
	return Options{}.noticeOps(newInbox(), nil)[op].budget(nil)
}

// NoticeOp runs the notices op name with args as the daemon serves it, on
// mutes, with m as the team poll's last model (nil before the first), and
// returns its answer and, when it refuses, its code.
func NoticeOp(o Options, m *team.Model, mutes *notice.Mutes, name string, args json.RawMessage) (any, string, error) {
	in := newInbox()
	in.model = m
	op := o.noticeOps(in, mutes)[name]
	v, err := op.run(context.Background(), args)
	if err != nil {
		return nil, op.code(err), err
	}
	return v, "", nil
}

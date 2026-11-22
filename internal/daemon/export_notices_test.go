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
	_, err := inboxSink{st: st, in: in, log: o.Logger, events: o.Team.Events, now: o.Now}.Send(context.Background(), m)
	return err
}

// FlushToInbox flushes b's waiting notices as the poll does, through its
// inbox sink on st, with bosses, by project, as the poll gives them in
// (A44), and settles what the sink kept.
func FlushToInbox(o Options, st *store.Store, bosses map[string]string, b *notice.Batcher) {
	in := newInbox()
	in.bosses = bosses
	o.flushNotices(context.Background(), &notices{batch: b, sink: inboxSink{st: st, in: in, log: o.Logger, events: o.Team.Events, now: o.Now}})
}

// NoticeCycle is one daemon's notices on a store, with no loop of its own:
// the team poll's notices, which go through the inbox (A44), the GitHub
// collector's source that feeds them (A45), and the dispatcher that types
// the boss's inbox, so a test runs each step in turn.
type NoticeCycle struct {
	o   Options
	n   *notices
	src GitHubSource
	d   *dispatcher
}

// NewNoticeCycle is o's notices, GitHub source and dispatcher on st, with
// bosses, by project, as the poll gives them in.
func NewNoticeCycle(o Options, st *store.Store, bosses map[string]string) *NoticeCycle {
	in := newInbox()
	in.bosses = bosses
	n := o.newNotices(st, in, nil)
	return &NoticeCycle{o: o, n: n, src: GitHubSource{o.newGitHubNotices(st, n)}, d: o.newDispatcher(st, in)}
}

// Source is the cycle's GitHub source; what it collects, Take hands back.
func (c *NoticeCycle) Source() GitHubSource { return c.src }

// Add gives the cycle's Batcher ns at now, as a source does.
func (c *NoticeCycle) Add(now time.Time, ns []notice.Notice) { c.n.batch.Add(now, ns) }

// Flush ends a poll's notices as endPoll does: what has waited its gap goes
// into the inbox, and what the inbox kept is settled.
func (c *NoticeCycle) Flush(ctx context.Context) { c.o.flushNotices(ctx, c.n) }

// Serve types boss's inbox into the agent named boss, as a dispatcher pass
// does, until nothing is queued for it.
func (c *NoticeCycle) Serve(ctx context.Context, boss string) {
	c.d.serve(ctx, store.Address{Kind: store.KindBoss, ID: boss}, boss)
}

// EndPoll ends a poll as pollTeam does, with b's notices for s, and
// reports, for each message s got, whether the dispatcher had been woken by
// then, and whether it was woken once the poll ended.
func EndPoll(o Options, b *notice.Batcher, s notice.Sink) (wokenAtSend []bool, wokenAfter bool) {
	in := newInbox()
	n := &notices{batch: b, sink: wakeSpy{s: s, in: in, woken: &wokenAtSend}}
	o.endPoll(context.Background(), n, in, &team.Model{})
	return wokenAtSend, len(in.wake) == 1
}

type wakeSpy struct {
	s     notice.Sink
	in    *inbox
	woken *[]bool
}

func (w wakeSpy) Send(ctx context.Context, m notice.Message) (int, error) {
	*w.woken = append(*w.woken, len(w.in.wake) == 1)
	return w.s.Send(ctx, m) //nolint:wrapcheck // a test's pass-through
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

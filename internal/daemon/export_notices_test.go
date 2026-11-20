package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
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

// EndPoll ends a poll as pollTeam does, with b's notices for s, and
// reports, for each message s got, whether the dispatcher had been woken by
// then, and whether it was woken once the poll ended.
func EndPoll(o Options, b *notice.Batcher, s notice.Sink) (wokenAtSend []bool, wokenAfter bool) {
	in := newInbox()
	n := &notices{batch: b, sink: wakeSpy{s: s, in: in, woken: &wokenAtSend}}
	o.endPoll(context.Background(), n, in, &team.Model{})
	return wokenAtSend, len(in.wake) == 1
}

// Poller ends team polls and runs dispatcher passes, one at a time, on one
// inbox, one notices and one dispatcher, configured as the daemon
// configures them: in.configure(teamConfigOf(teams, "")), the inbox's
// delivery settings from cfgs, o.newDispatcher(st, in), and
// notices{batch: b, configs: cfgs, sink: s}. s nil: the inbox sink the
// daemon builds, on st and p's inbox. b nil: a Batcher on cfgs. Each call
// keeps the same inbox, memory and notices, so a test can run poll, pass,
// poll and pass.
type Poller struct {
	o  Options
	in *inbox
	n  *notices
	d  *dispatcher
}

// NewPoller is a Poller on st for teams, as a daemon run with o polls them.
func NewPoller(o Options, st *store.Store, teams []settings.Team, cfgs notice.Configs, b *notice.Batcher, s notice.Sink) *Poller {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Team == nil {
		o.Team = &TeamOptions{}
	}
	in := newInbox()
	in.configure(teamConfigOf(teams, ""))
	in.setConfigs(cfgs)
	if b == nil {
		b = notice.NewBatcher(cfgs)
	}
	if s == nil {
		s = inboxSink{st: st, in: in, log: o.Logger, events: o.Team.Events, now: o.Now}
	}
	return &Poller{o: o, in: in, n: &notices{batch: b, configs: cfgs, sink: s}, d: o.newDispatcher(st, in)}
}

// EndPoll ends a poll on m as pollTeam does: o.endPoll, which remembers
// m's boss agents, flushes, and gives the dispatcher m.
func (p *Poller) EndPoll(m *team.Model) {
	p.o.endPoll(context.Background(), p.n, p.in, m)
}

// Pass runs one dispatcher pass, as a wake does.
func (p *Poller) Pass() {
	p.d.pass(context.Background())
}

// Post posts to's queue on project's board as a pass does, on ctx.
func (p *Poller) Post(ctx context.Context, to store.Address, project string) {
	p.d.post(ctx, to, project)
}

// TakeWake empties the inbox's wake, as the dispatcher's loop takes it
// before a pass, and reports whether it held one.
func (p *Poller) TakeWake() bool {
	select {
	case <-p.in.wake:
		return true
	default:
		return false
	}
}

// HoldRoute takes the inbox's route, the lock a board-mode boss's typed
// claim and a poll's remember take, until release is called.
func (p *Poller) HoldRoute() (release func()) {
	p.in.route.Lock()
	var once sync.Once
	return func() { once.Do(p.in.route.Unlock) }
}

// Delivery is how the inbox delivers to project's boss now (A74).
func (p *Poller) Delivery(project string) notice.Delivery {
	return p.in.delivery(project)
}

type wakeSpy struct {
	s     notice.Sink
	in    *inbox
	woken *[]bool
}

func (w wakeSpy) Send(ctx context.Context, m notice.Message) error {
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

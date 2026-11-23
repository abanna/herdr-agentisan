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
	_, err := inboxSink{st: st, in: in, log: o.Logger, events: o.Team.Events, now: o.Now}.Send(context.Background(), m)
	return err
}

// FlushToInbox flushes b's waiting notices as the poll does, through its
// inbox sink on st, with bosses, by project, as the poll gives them in
// (A44), and settles what the sink kept.
func FlushToInbox(o Options, st *store.Store, bosses map[string]string, b *notice.Batcher) {
	in := newInbox()
	in.bosses = bosses
	o.flushNotices(context.Background(), &notices{batch: b, sink: inboxSink{st: st, in: in, log: o.Logger, events: o.Team.Events, now: o.Now}}, in)
}

// NoticeCycle is one daemon's notices on a store, with no loop of its own:
// the team poll's notices, which go through the inbox (A44), the GitHub
// collector's source that feeds them (A45), and the dispatcher that types
// the boss's inbox, so a test runs each step in turn.
type NoticeCycle struct {
	o      Options
	n      *notices
	src    GitHubSource
	d      *dispatcher
	bosses map[string]string
}

// NewNoticeCycle is o's notices, GitHub source and dispatcher on st, with
// bosses, by project, as the poll gives them in.
func NewNoticeCycle(o Options, st *store.Store, bosses map[string]string) *NoticeCycle {
	in := newInbox()
	in.bosses = bosses
	n := o.newNotices(st, in, nil)
	return &NoticeCycle{o: o, n: n, src: GitHubSource{o.newGitHubNotices(st, n)}, d: o.newDispatcher(st, in), bosses: bosses}
}

// Source is the cycle's GitHub source; what it collects, Take hands back.
func (c *NoticeCycle) Source() GitHubSource { return c.src }

// Add gives the cycle's Batcher ns at now, as a source does.
func (c *NoticeCycle) Add(now time.Time, ns []notice.Notice) { c.n.batch.Add(now, ns) }

// Flush ends a poll's notices as endPoll does: what has waited its gap goes
// into the inbox, and what the inbox kept is settled.
func (c *NoticeCycle) Flush(ctx context.Context) { c.o.flushNotices(ctx, c.n, c.d.in) }

// Serve types boss's inbox into the agent named boss, as a dispatcher pass
// does with the poll's bosses, until nothing is queued for it.
func (c *NoticeCycle) Serve(ctx context.Context, boss string) {
	c.d.serve(ctx, c.bosses, store.Address{Kind: store.KindBoss, ID: boss}, boss)
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
	return &Poller{o: o, in: in, n: &notices{batch: b, configs: cfgs, sink: s, board: o.boardWriter(st)}, d: o.newDispatcher(st, in)}
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
	out, err := p.d.postQueued(ctx, to, project)
	p.d.posted(ctx, to, project, out, err)
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

// HoldMu takes the inbox's mu until release is called.
func (p *Poller) HoldMu() (release func()) {
	p.in.mu.Lock()
	var once sync.Once
	return func() { once.Do(p.in.mu.Unlock) }
}

// RouteHeld reports whether someone holds the inbox's route now: it fails
// to take it, else it takes it and gives it back at once.
func (p *Poller) RouteHeld() bool { return p.in.route.Held() }

// MuHeld reports whether someone holds the inbox's mu now, as RouteHeld
// does for route.
func (p *Poller) MuHeld() bool {
	if p.in.mu.TryLock() {
		p.in.mu.Unlock()
		return false
	}
	return true
}

// RouteHolds is how many times route has been taken.
func (p *Poller) RouteHolds() uint64 { return p.in.route.Holds() }

// SetDeliveryProbe makes f the inbox's delivery probe, called at each
// delivery read; nil removes it.
func (p *Poller) SetDeliveryProbe(f func()) { p.in.deliveryProbe = f }

// Delivery is how the inbox delivers to project's boss now (A74).
func (p *Poller) Delivery(project string) notice.Delivery {
	return p.in.delivery(project)
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

// TypedTurn stops after a pass has selected a typed recipient. Its caller
// finishes a poll, then resumes serve with the same target and boss map.
func (p *Poller) TypedTurn(to store.Address) func() {
	ctx := context.Background()
	m, bosses := p.in.view()
	if p.d.untyped(ctx, bosses, to) {
		return nil
	}
	target, held := targetOf(*m, bosses, to)
	if held != "" {
		return nil
	}
	return func() { p.d.serve(ctx, bosses, to, target) }
}

// ObserveBossClaim observes the dispatcher's atomic store on its own
// goroutine, immediately after it publishes or clears the address.
func (p *Poller) ObserveBossClaim(f func(*store.Address)) {
	p.in.typedBoss = observedClaim{p.in.typedBoss, f}
}

type observedClaim struct {
	bossClaim
	observe func(*store.Address)
}

func (c observedClaim) Store(to *store.Address) {
	c.bossClaim.Store(to)
	c.observe(to)
}

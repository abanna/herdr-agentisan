package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// ToastBudget bounds the toast that alerts the user to a ceiling (A33): the
// one herdr call the context levels add to a poll.
const ToastBudget = time.Second

// noticeMsg is the log message a notice is written under, which a boss
// following daemon.log matches (A44).
const noticeMsg = "notice"

// notices tells each team's boss what the team poll sees (A44): the poll's
// Watch makes the notices, a Batcher paces them per boss, dropping those
// about a worker the boss muted, and a sink delivers them through the
// inbox. Only the poll's goroutine uses it.
type notices struct {
	watch *notice.Watch
	// ctx tells the context levels each pane crosses (A33).
	ctx     *notice.CtxWatch
	batch   *notice.Batcher
	configs notice.Configs
	sink    notice.Sink
	// board posts a board-mode team's notices on its board (A74).
	board boardSink
	// github carries the GitHub collector's notices to the poll (A45).
	github noticeQueue
}

// newNotices reads every team's notices settings from the configuration,
// once, as the poll reads its teams (A19). A configuration that does not
// resolve is logged, and every team gets the defaults: notices must not
// stop because of a typo, any more than $team does. Its messages go to each
// team's boss through st's inbox, by the bosses the poll gives in, and the
// workers mutes silences are dropped.
func (o Options) newNotices(st *store.Store, in *inbox, mutes *notice.Mutes) *notices {
	ns, err := settings.Notices(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Error().Err(err).Msg("herdr-agentisan.toml does not resolve; every boss gets the default notices until it is fixed and the daemon restarts")
		ns, _ = settings.Notices("")
	}
	cfgs := notice.Configs{Default: noticeConfig(ns.Default), Projects: map[string]notice.Config{}}
	for name, c := range ns.Projects {
		cfgs.Projects[name] = noticeConfig(c)
	}
	in.setConfigs(cfgs)
	batch := notice.NewBatcher(cfgs)
	batch.Mutes = mutes
	sink := inboxSink{st: st, in: in, log: o.Logger, events: o.Team.Events, now: o.Now}
	board := boardSink{st: st, log: o.Logger, events: o.Team.Events, now: o.Now}
	return &notices{watch: notice.NewWatch(), ctx: o.newCtxWatch(), batch: batch, configs: cfgs, sink: sink, board: board}
}

// newCtxWatch reads every team's rotation thresholds from the configuration
// and each project's repo file, once (A33). A configuration that does not
// resolve gives every team the defaults, and a repo file that cannot be read
// its project the thresholds of its other layers; each is logged.
func (o Options) newCtxWatch() *notice.CtxWatch {
	rs, err := settings.Rotations(settings.LoadOptions{ConfigDir: o.Team.ConfigDir})
	if err != nil {
		o.Logger.Error().Err(err).Msg("herdr-agentisan.toml does not resolve; every team gets the default rotation thresholds until it is fixed and the daemon restarts")
		rs, _ = settings.Rotations(settings.LoadOptions{})
	}
	for _, name := range slices.Sorted(maps.Keys(rs.Unread)) {
		o.Logger.Warn().Err(rs.Unread[name]).Str("project", name).Msg("rotation thresholds: the project's repo file is not read, so its other layers' apply until it is fixed and the daemon restarts")
	}
	return notice.NewCtxWatch(func(project, group string) []int { return rs.For(project).For(group) })
}

func noticeConfig(c settings.NoticeConfig) notice.Config {
	skip := map[notice.Kind]bool{}
	for _, k := range c.Skip {
		skip[notice.Kind(k)] = true
	}
	return notice.Config{Skip: skip, Gap: c.Gap, Repeat: c.Repeat, Board: c.Delivery == settings.DeliveryBoard}
}

// observe takes one poll's model and, when agent.list answered it (named),
// its agents.
func (n *notices) observe(m team.Model, agents []herdr.AgentInfo, named bool, now time.Time) {
	n.batch.Add(now, n.watch.Observe(m, agents, named))
}

// observeCtx tells each boss the context levels its panes crossed in m
// (A33), and records each as an event. A ceiling first alerts the user with
// a toast, within ToastBudget: a toast herdr does not show, or a call that
// fails, is logged and never tried again, and the boss's line says so.
func (o Options) observeCtx(ctx context.Context, c herdr.Client, n *notices, m team.Model, now time.Time) {
	crossed := n.ctx.Observe(m)
	ns := make([]notice.Notice, 0, len(crossed))
	for _, cr := range crossed {
		alerted := cr.Level == team.LevelCeiling
		shown := alerted && o.alert(ctx, c, cr)
		ns = append(ns, cr.Notice(shown))
		role := telemetry.RoleWorker
		if cr.Boss {
			role = telemetry.RoleBoss
		}
		o.Team.Events.CtxLevelCrossed(ctx, telemetry.CtxLevel{
			WorkerID: cr.Worker.ID, Bound: cr.Worker.Bound, Project: cr.Project, Group: cr.Group, Role: role,
			Agent: cr.Worker.Pane.Agent, PaneID: cr.Worker.Pane.ID, Level: cr.Level.String(),
			Ctx: cr.Ctx, Threshold: cr.Threshold, Alerted: alerted, Shown: shown,
		})
	}
	n.batch.Add(now, ns)
}

// alert shows the user cr's toast and reports whether herdr showed it.
func (o Options) alert(ctx context.Context, c herdr.Client, cr notice.Crossing) bool {
	tctx, cancel := context.WithTimeout(ctx, ToastBudget)
	defer cancel()
	res, err := c.ShowNotification(tctx, cr.Alert())
	switch {
	case err != nil:
		o.warnUnlessStopping(ctx, err, "the context ceiling's toast")
		return false
	case !res.Shown:
		o.Logger.Warn().Str("reason", res.Reason).Str("pane", cr.Worker.Pane.ID).Msg("the context ceiling's toast was not shown")
	}
	return res.Shown
}

// endPoll ends a poll: it remembers the agent on each tabs team's boss pane
// (A74), queues the poll's notices, then gives the dispatcher the poll's
// model, which wakes it. In that order, so the flush and the dispatcher read
// one memory, already updated with this poll, and a boss in its place is
// typed a notice in the pass the poll that queued it wakes (A44); the other
// way round, that pass could read the inbox first and leave the notice for
// the next poll's.
func (o Options) endPoll(ctx context.Context, n *notices, in *inbox, built *team.Model) {
	in.remember(built)
	o.flushNotices(ctx, n, in)
	in.polled(built)
}

// flush sends every boss whose gap has passed its waiting notices: a
// board-mode boss's on its board (A74), every other's through the sink, to
// the inbox. A message the sink refuses is logged and dropped,
// unacknowledged.
func (o Options) flushNotices(ctx context.Context, n *notices, in *inbox) {
	n.batch.Add(o.Now(), n.github.take())
	_, bosses := in.view()
	for _, m := range n.batch.Flush(o.Now()) {
		posted, tail := o.postNotices(ctx, n, in, bosses, m)
		if posted {
			continue
		}
		if tail {
			m.Tail = true
		}
		if err := n.sink.Send(ctx, m); err != nil {
			o.Logger.Warn().Err(err).Str("project", m.Project).Msg("notice not delivered")
			continue
		}
		m.Settle()
	}
}

// inboxSink delivers each message to its team's boss through the inbox
// (A44): appended to the boss's notice still queued, else a new one, which
// the dispatcher types into the boss once it is in its place. It writes each
// message to daemon.log as well, one line, as part 1 did, with the inbox
// message it went into: a boss can follow the log, and it is the record of
// a message to a project no team's boss is configured for, which goes
// nowhere else.
type inboxSink struct {
	st     *store.Store
	in     *inbox
	log    zerolog.Logger
	events *telemetry.Events
	now    func() time.Time
}

// Send queues m for its boss and logs it. A project with no boss is only
// logged, and settled: told again, it would still have nowhere to go,
// since the daemon reads its teams once. A failed write is logged too, and
// returned, so m is not settled.
func (s inboxSink) Send(ctx context.Context, m notice.Message) error {
	_, bosses := s.in.view()
	e := s.log.Info().Str("project", m.Project).Strs("notices", m.Lines)
	if m.Dropped > 0 {
		e = e.Int("dropped", m.Dropped)
	}
	boss, ok := bosses[m.Project]
	if !ok {
		e.Str("boss", "").Msg(noticeMsg)
		return nil
	}
	to := store.Address{Kind: store.KindBoss, ID: boss}
	queued, appended, err := s.st.Notify(ctx, to, s.now(), m.AppendTo)
	e = e.Str("boss", boss)
	if err != nil {
		e.Msg(noticeMsg)
		return fmt.Errorf("queue the notice for %s: %w", boss, err)
	}
	e.Str("inbox", queued.ID).Bool("appended", appended).Msg(noticeMsg)
	if !appended {
		s.events.MessageQueued(ctx, telemetry.Message{ID: queued.ID, FromKind: store.Daemon.Kind, From: store.Daemon.ID, ToKind: to.Kind, To: to.ID, At: queued.CreatedAt})
	}
	return nil
}

// postNotices posts m on its team's board when its boss is a board-mode
// boss (A74): the boss's queue first, so a held notice or message is never
// posted after a newer one, then, when that call left nothing queued, m,
// one post per notice, every notice acknowledged. It reports whether it
// did, and whether m goes to the inbox as a tail: a board-mode boss whose
// queue could not be emptied gets m as a new inbox row behind every held
// one; the others' m (a typed or held boss, a boss name two teams share, a
// project with no boss, a flush the board refuses) goes to the inbox by
// A44's append.
func (o Options) postNotices(ctx context.Context, n *notices, in *inbox, bosses map[string]string, m notice.Message) (posted, tail bool) {
	boss, ok := bosses[m.Project]
	if !ok {
		return false, false
	}
	if p, one := bossProject(bosses, boss); !one || p != m.Project || in.delivery(m.Project) != notice.DeliverBoard {
		return false, false
	}
	out, err := o.postQueue(ctx, n.board.st, store.Address{Kind: store.KindBoss, ID: boss}, m.Project)
	if err != nil {
		o.Logger.Warn().Err(err).Str("project", m.Project).Msg("notice not posted; it goes to the inbox")
		return false, true
	}
	if out.More {
		return false, true
	}
	if err := n.board.Send(ctx, boss, m); err != nil {
		o.Logger.Warn().Err(err).Str("project", m.Project).Msg("notice not posted; it goes to the inbox")
		return false, false
	}
	m.SettleAll()
	return true, false
}

// boardSink posts a board-mode team's notices on its board (A74): one
// daemon post per notice, the ones the inbox would cut included, in one
// transaction and one wake. It logs each flush to daemon.log as the inbox
// sink does, with delivery board and the posts' seqs in place of the
// inbox's keys, and records it as one agentisan.board.posted event.
type boardSink struct {
	st     *store.Store
	log    zerolog.Logger
	events *telemetry.Events
	now    func() time.Time
}

// Send posts m's notices for boss, each with its kind, subject and worker;
// a subject or worker the board would refuse is posted empty. A flush the
// board refuses posts none of them, and is logged without posts and
// returned.
func (s boardSink) Send(ctx context.Context, boss string, m notice.Message) error {
	entries := m.Entries()
	at := s.now()
	ps := make([]store.BoardPost, 0, len(entries))
	bodies := make([]string, 0, len(entries))
	for _, e := range entries {
		ps = append(ps, store.BoardPost{
			Project: m.Project, Channel: store.ChannelDaemon, Author: store.Daemon, Kind: string(e.Kind),
			Subject: store.FitID(e.Subject), Worker: store.FitID(e.Worker), Body: e.Body, At: at,
		})
		bodies = append(bodies, e.Body)
	}
	ev := s.log.Info().Str("project", m.Project).Strs("notices", bodies).Str("boss", boss).Str("delivery", "board")
	posted, err := s.st.PostAll(ctx, ps)
	if err != nil {
		ev.Msg(noticeMsg)
		return fmt.Errorf("post the notices for %s: %w", boss, err)
	}
	seqs := make([]int64, 0, len(posted))
	for _, p := range posted {
		seqs = append(seqs, p.Seq)
	}
	ev.Ints64("posts", seqs).Msg(noticeMsg)
	if len(seqs) > 0 {
		s.events.BoardPosted(ctx, telemetry.BoardPost{Project: m.Project, Channel: store.ChannelDaemon, Posts: len(seqs), FirstSeq: seqs[0], LastSeq: seqs[len(seqs)-1]})
	}
	return nil
}

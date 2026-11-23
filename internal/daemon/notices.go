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
	board   boardWriter
	// github carries the GitHub collector's notices to the poll (A45).
	github noticeQueue
}

// newNotices reads every team's notices settings from the configuration,
// once, as the poll reads its teams (A19). A configuration that does not
// resolve is logged. Use defaults if this read fails while the separate
// team poll read succeeds. The sink queues each message for the boss the
// poll names, and the batcher drops notices for muted workers.
func (o Options) newNotices(st *store.Store, in *inbox, mutes *notice.Mutes) *notices {
	ns, err := settings.Notices(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Error().Err(err).Msg("notices: cannot read settings. Using defaults until the daemon restarts")
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
	return &notices{watch: notice.NewWatch(), ctx: o.newCtxWatch(), batch: batch, configs: cfgs, sink: sink, board: o.boardWriter(st)}
}

// newCtxWatch reads every team's rotation thresholds from the configuration
// and each project's repo file, once (A33). A configuration that does not
// resolve gives defaults if the separate team poll read succeeds. A repo
// file that cannot be read leaves the project's other layers in effect.
func (o Options) newCtxWatch() *notice.CtxWatch {
	rs, err := settings.Rotations(settings.LoadOptions{ConfigDir: o.Team.ConfigDir})
	if err != nil {
		o.Logger.Error().Err(err).Msg("rotation: cannot read settings. Using defaults until the daemon restarts")
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
// (A74), flushes the poll's notices, then gives the dispatcher the poll's
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

// flush sends every boss whose gap has passed its waiting notices. A
// message the sink refuses is logged and dropped, unacknowledged; of one it
// accepts, only the lines the message it committed kept are settled.
func (o Options) flushNotices(ctx context.Context, n *notices, in *inbox) {
	n.batch.Add(o.Now(), n.github.take())
	_, bosses := in.view()
	for _, m := range n.batch.Flush(o.Now()) {
		handled, tail := n.board.postNotices(ctx, in, bosses, m)
		if handled {
			continue
		}
		m.Tail = tail
		kept, err := n.sink.Send(ctx, m)
		if err != nil {
			o.Logger.Warn().Err(err).Str("project", m.Project).Msg("notice not delivered")
			continue
		}
		m.Settle(kept)
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

// Send queues m for its boss and logs it, and keeps the lines the committed
// body holds: AppendTo cuts from the front, its header included. A project
// with no boss goes only to daemon.log (at info; above that level, nowhere),
// and is kept whole, so settled: told again, it would still have nowhere to
// go, since the daemon reads its teams once. A failed write is logged too,
// and returned, so m is not settled.
func (s inboxSink) Send(ctx context.Context, m notice.Message) (int, error) {
	_, bosses := s.in.view()
	e := s.log.Info().Str("project", m.Project).Strs("notices", m.Lines)
	if m.Dropped > 0 {
		e = e.Int("dropped", m.Dropped)
	}
	boss, ok := bosses[m.Project]
	if !ok {
		e.Str("boss", "").Msg(noticeMsg)
		return len(m.Lines), nil
	}
	to := store.Address{Kind: store.KindBoss, ID: boss}
	queued, appended, err := s.st.Notify(ctx, to, s.now(), m.AppendTo)
	e = e.Str("boss", boss)
	if err != nil {
		e.Msg(noticeMsg)
		return 0, fmt.Errorf("queue the notice for %s: %w", boss, err)
	}
	e.Str("inbox", queued.ID).Bool("appended", appended).Msg(noticeMsg)
	if !appended {
		s.events.MessageQueued(ctx, telemetry.Message{ID: queued.ID, FromKind: store.Daemon.Kind, From: store.Daemon.ID, ToKind: to.Kind, To: to.ID, At: queued.CreatedAt})
	}
	return m.Kept(queued.Body), nil
}

// postNotices handles a board decision and its writes in one route hold.
func (s boardWriter) postNotices(ctx context.Context, in *inbox, bosses map[string]string, m notice.Message) (handled, tail bool) {
	boss, ok := bosses[m.Project]
	if !ok || !in.boardKey(m.Project) {
		return false, false
	}
	if _, one := bossProject(bosses, boss); !one {
		return false, false
	}
	in.route.Lock()
	if in.delivery(m.Project) != notice.DeliverBoard {
		in.route.Unlock()
		return false, false
	}
	to := store.Address{Kind: store.KindBoss, ID: boss}
	if held := in.typedBoss.Load(); held != nil && *held == to {
		in.route.Unlock()
		return false, true
	}
	queue, err := s.postQueued(ctx, to, m.Project)
	if err != nil {
		in.route.Unlock()
		s.log.Warn().Err(err).Str("project", m.Project).Msg("notice not posted; it goes to the inbox")
		return false, true
	}
	var out store.PostedNotices
	var bodies []string
	committed := 0
	if !queue.More {
		out, bodies, committed, err = s.postBatch(ctx, m)
	}
	in.route.Unlock()
	s.reportQueue(ctx, to, m.Project, queue)
	if queue.More {
		return false, true
	}
	s.reportNotices(ctx, boss, m.Project, out, bodies, committed, err)
	m.SettleEntries(committed)
	return true, false
}

func (s boardWriter) postBatch(ctx context.Context, m notice.Message) (store.PostedNotices, []string, int, error) {
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
	var out store.PostedNotices
	committed := 0
	for chunk := range slices.Chunk(ps, store.MaxQueuePost) {
		batch, err := s.st.PostAll(ctx, chunk)
		if err != nil {
			return out, bodies, committed, fmt.Errorf("post notice batch: %w", err)
		}
		out.Posts = append(out.Posts, batch.Posts...)
		out.Refused += batch.Refused
		committed += len(chunk)
	}
	return out, bodies, committed, nil
}

func (s boardWriter) reportNotices(ctx context.Context, boss, project string, out store.PostedNotices, bodies []string, committed int, err error) {
	ev := s.log.Info().Str("project", project).Strs("notices", bodies).Str("boss", boss).Str("delivery", "board")
	if out.Refused > 0 {
		ev.Int("refused", out.Refused)
	}
	if err != nil {
		dropped := len(bodies) - committed
		ev.Int("dropped", dropped)
		s.log.Warn().Err(err).Str("project", project).Int("dropped", dropped).Msg("notice not delivered")
	}
	seqs := make([]int64, 0, len(out.Posts))
	for _, p := range out.Posts {
		seqs = append(seqs, p.Seq)
	}
	if len(seqs) > 0 {
		ev.Ints64("posts", seqs)
		s.events.BoardPosted(ctx, telemetry.BoardPost{Project: project, Channel: store.ChannelDaemon, Posts: len(seqs), FirstSeq: seqs[0], LastSeq: seqs[len(seqs)-1]})
	}
	ev.Msg(noticeMsg)
}

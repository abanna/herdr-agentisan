package daemon

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
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
// Watch makes the notices, a Batcher paces them per boss, and a sink
// delivers them. Only the poll's goroutine uses it.
type notices struct {
	watch *notice.Watch
	// ctx tells the context levels each pane crosses (A33).
	ctx     *notice.CtxWatch
	batch   *notice.Batcher
	configs notice.Configs
	sink    notice.Sink
	// github carries the GitHub collector's notices to the poll (A45).
	github noticeQueue
}

// newNotices reads every team's notices settings from the configuration,
// once, as the poll reads its teams (A19). A configuration that does not
// resolve is logged, and every team gets the defaults: notices must not
// stop because of a typo, any more than $team does.
func (o Options) newNotices() *notices {
	ns, err := settings.Notices(o.Team.ConfigDir)
	if err != nil {
		o.Logger.Error().Err(err).Msg("herdr-agentisan.toml does not resolve; every boss gets the default notices until it is fixed and the daemon restarts")
		ns, _ = settings.Notices("")
	}
	cfgs := notice.Configs{Default: noticeConfig(ns.Default), Projects: map[string]notice.Config{}}
	for name, c := range ns.Projects {
		cfgs.Projects[name] = noticeConfig(c)
	}
	return &notices{watch: notice.NewWatch(), ctx: o.newCtxWatch(), batch: notice.NewBatcher(cfgs), configs: cfgs, sink: logSink{o.Logger}}
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
	return notice.Config{Skip: skip, Gap: c.Gap, Repeat: c.Repeat}
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

// flush sends every boss whose gap has passed its waiting notices. A
// message the sink refuses is logged and dropped, unacknowledged.
func (o Options) flushNotices(ctx context.Context, n *notices) {
	n.batch.Add(o.Now(), n.github.take())
	for _, m := range n.batch.Flush(o.Now()) {
		if err := n.sink.Send(ctx, m); err != nil {
			o.Logger.Warn().Err(err).Str("project", m.Project).Msg("notice not delivered")
			continue
		}
		m.Settle()
	}
}

// logSink writes each message to daemon.log, one line, which a boss
// follows until notices go through the inbox (A44).
type logSink struct {
	logger zerolog.Logger
}

func (s logSink) Send(_ context.Context, m notice.Message) error {
	e := s.logger.Info().Str("project", m.Project).Strs("notices", m.Lines)
	if m.Dropped > 0 {
		e = e.Int("dropped", m.Dropped)
	}
	e.Msg(noticeMsg)
	return nil
}

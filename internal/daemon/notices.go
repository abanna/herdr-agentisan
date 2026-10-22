package daemon

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// noticeMsg is the log message a notice is written under, which a boss
// following daemon.log matches (A44).
const noticeMsg = "notice"

// notices tells each team's boss what the team poll sees (A44): the poll's
// Watch makes the notices, a Batcher paces them per boss, and a sink
// delivers them. Only the poll's goroutine uses it.
type notices struct {
	watch *notice.Watch
	batch *notice.Batcher
	sink  notice.Sink
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
	return &notices{watch: notice.NewWatch(), batch: notice.NewBatcher(cfgs), sink: logSink{o.Logger}}
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

// flush sends every boss whose gap has passed its waiting notices. A
// message the sink refuses is logged and dropped, unacknowledged.
func (o Options) flushNotices(ctx context.Context, n *notices) {
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

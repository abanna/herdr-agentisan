package daemon

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// EventsFlushTimeout bounds the flush of pending workflow events when the
// daemon exits. `daemon run` flushes after Run has returned and released the
// lock, so a dead collector delays only this process's exit, never the next
// daemon's start (A3), and this bound keeps even that short.
const EventsFlushTimeout = 2 * time.Second

// SetupEvents builds the daemon's workflow events (D10, A21) and returns them
// with a flush to run on exit. The endpoint is tc.OTLPEndpoint when set
// (HERDR_AGENTISAN_OTLP_ENDPOINT), else telemetry.endpoint in the shared file
// in configDir; with neither, the Events are nil and emit nothing.
//
// Telemetry never stops the daemon: a configuration that does not resolve,
// or an exporter that cannot be built, is logged and the daemon runs without
// events, as a typo must not stop $team, focus and Back. Export errors reach
// the log through otel's process-wide error handler, which this sets.
func SetupEvents(ctx context.Context, configDir string, tc telemetry.Config, logger zerolog.Logger) (*telemetry.Events, func()) {
	noop := func() {}
	endpoint, err := settings.TelemetryEndpoint(configDir, tc.OTLPEndpoint)
	if err != nil {
		logger.Error().Err(err).Msg("herdr-agentisan.toml does not resolve; the daemon runs without workflow events until it is fixed and the daemon restarts")
		return nil, noop
	}
	tc.OTLPEndpoint = endpoint
	events, shutdown, err := telemetry.SetupEvents(ctx, tc)
	if err != nil {
		logger.Error().Err(err).Msg("the daemon runs without workflow events")
		return nil, noop
	}
	if events == nil {
		return nil, noop
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn().Err(err).Msg("telemetry export")
	}))
	logger.Info().Str("endpoint", endpoint).Msg("workflow events export over OTLP")
	return events, func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), EventsFlushTimeout)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			logger.Warn().Err(err).Msg("telemetry export: flush workflow events")
		}
	}
}

// emitTransitions sends each status change between two polls' models as an
// event. The pane, not the logical id, is what changed status, so a worker
// whose row the poll could not read is still reported, under its
// provisional id.
func (t *TeamOptions) emitTransitions(ctx context.Context, prev, next team.Model) {
	for _, tr := range team.Transitions(prev, next) {
		role := telemetry.RoleWorker
		if tr.Boss {
			role = telemetry.RoleBoss
		}
		t.Events.StatusChanged(ctx, telemetry.StatusChange{
			WorkerID: tr.Worker.ID,
			Bound:    tr.Worker.Bound,
			Project:  tr.Project,
			Group:    tr.Group,
			Role:     role,
			Agent:    tr.Worker.Pane.Agent,
			PaneID:   tr.Worker.Pane.ID,
			From:     tr.From,
			To:       tr.To,
		})
	}
}

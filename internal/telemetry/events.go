package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// Workflow events (ADR-001 D10, A21) are OTLP log records carrying an event
// name, which Elastic stores in its logs data streams for the debugger agent
// to query. Log records are not sampled, so unlike spans none is dropped by
// SampleRatio. docs/telemetry.md is the schema's contract: rename a key here
// and every saved query on the other side stops matching.

// EventStatusChanged is a pane's agent status changing between two polls of
// the daemon, or the pane appearing in or leaving the team model.
const EventStatusChanged = "agentisan.worker.status_changed"

// The attribute keys of EventStatusChanged.
const (
	// AttrWorkerID is the logical worker id, or the pane id while no workers
	// row binds the pane (AttrWorkerBound false).
	AttrWorkerID    = "agentisan.worker.id"
	AttrWorkerBound = "agentisan.worker.bound"
	// AttrProject is the configured project; "" is the unnamed project.
	AttrProject = "agentisan.project"
	// AttrGroup is the group's name; "" for the boss.
	AttrGroup = "agentisan.group"
	// AttrRole is RoleBoss or RoleWorker.
	AttrRole = "agentisan.role"
	// AttrAgent is the agent herdr detected in the pane; "" for a shell.
	AttrAgent  = "agentisan.agent"
	AttrPaneID = "herdr.pane.id"
	// AttrFrom and AttrTo are herdr agent statuses, or "absent" on the side
	// of the poll the pane was not in the model.
	AttrFrom = "agentisan.status.from"
	AttrTo   = "agentisan.status.to"
)

// The values of AttrRole.
const (
	RoleBoss   = "boss"
	RoleWorker = "worker"
)

// scope names the instrumentation the events come from.
const scope = "github.com/abanna/herdr-agentisan/internal/telemetry"

// StatusChange is one EventStatusChanged.
type StatusChange struct {
	WorkerID string
	Bound    bool
	Project  string
	Group    string
	Role     string
	Agent    string
	PaneID   string
	From, To string
}

// Events emits workflow events. A nil *Events emits nothing, which is what
// the daemon holds when no endpoint is configured.
type Events struct {
	logger log.Logger
}

// NewEvents returns Events emitting through provider.
func NewEvents(provider log.LoggerProvider) *Events {
	return &Events{logger: provider.Logger(scope)}
}

// StatusChanged emits c as an EventStatusChanged record.
func (e *Events) StatusChanged(ctx context.Context, c StatusChange) {
	if e == nil {
		return
	}
	var r log.Record
	r.SetEventName(EventStatusChanged)
	r.SetTimestamp(time.Now())
	r.SetSeverity(log.SeverityInfo)
	r.SetSeverityText("INFO")
	r.SetBody(attribute.StringValue(c.WorkerID + " " + c.From + " → " + c.To))
	r.AddAttributes(
		attribute.String(AttrWorkerID, c.WorkerID),
		attribute.Bool(AttrWorkerBound, c.Bound),
		attribute.String(AttrProject, c.Project),
		attribute.String(AttrGroup, c.Group),
		attribute.String(AttrRole, c.Role),
		attribute.String(AttrAgent, c.Agent),
		attribute.String(AttrPaneID, c.PaneID),
		attribute.String(AttrFrom, c.From),
		attribute.String(AttrTo, c.To),
	)
	e.logger.Emit(ctx, r)
}

// SetupEvents builds the workflow events' exporter: OTLP/HTTP log records to
// cfg.OTLPEndpoint, batched, under the same resource traces carry. It
// returns a shutdown function that flushes what is pending within its
// context's deadline.
//
// With an empty OTLPEndpoint it builds nothing and returns nil Events and a
// no-op shutdown: no goroutine, no connection, nothing exported. Unlike
// SetupTracing it installs no global provider: the daemon is handed its
// Events, so tests can hand it theirs.
//
// Export errors go to otel's error handler; the caller routes that to a log.
func SetupEvents(ctx context.Context, cfg Config) (*Events, func(context.Context) error, error) {
	if cfg.OTLPEndpoint == "" {
		return nil, func(context.Context) error { return nil }, nil
	}
	exp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpoint(cfg.OTLPEndpoint),
		// The exporter also reads OTEL_EXPORTER_OTLP_*, and the daemon
		// inherits herdr's environment: pin the path and the headers too, so
		// a variable meant for another program can neither redirect the
		// events nor attach its credentials to them.
		otlploghttp.WithURLPath("/v1/logs"),
		otlploghttp.WithHeaders(map[string]string{}),
		// Plaintext, as for traces: the collector is expected on localhost
		// and holds Elastic's credentials (A21).
		otlploghttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create otlp log exporter: %w", err)
	}
	res, err := newResource(cfg)
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	return NewEvents(lp), func(ctx context.Context) error {
		if err := lp.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("shutdown logger provider: %w", err)
		}
		return nil
	}, nil
}

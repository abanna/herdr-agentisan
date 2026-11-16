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

	"github.com/abanna/herdr-agentisan/internal/routing"
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

// EventWorkerRouted is the routing decision a worker spawn launched a worker
// on (A50): one record per worker, the decision applied or, while routing is
// off, the one it would have applied.
const EventWorkerRouted = "agentisan.worker.routed"

// The attribute keys of EventWorkerRouted beside AttrWorkerID, AttrProject
// and AttrGroup: the routing rules' audit fields, then the decision's own.
const (
	AttrRoutingTaskID           = "agentisan.routing.task_id"
	AttrRoutingLookupStatus     = "agentisan.routing.task_lookup_status"
	AttrRoutingAssignmentDigest = "agentisan.routing.assignment_digest"
	AttrRoutingRole             = "agentisan.routing.role"
	AttrRoutingProvider         = "agentisan.routing.provider"
	AttrRoutingBinding          = "agentisan.routing.selected_binding"
	AttrRoutingModelID          = "agentisan.routing.model_id"
	AttrRoutingEffort           = "agentisan.routing.effort"
	AttrRoutingPolicyVersion    = "agentisan.routing.policy_version"
	AttrRoutingJevModel         = "agentisan.routing.jev_model_returned"
	AttrRoutingAnswers          = "agentisan.routing.answers"
	AttrRoutingFallbackReason   = "agentisan.routing.fallback_reason"
	AttrRoutingJevError         = "agentisan.routing.jev_error"
	AttrRoutingOverrideBinding  = "agentisan.routing.override.binding"
	AttrRoutingOverrideBy       = "agentisan.routing.override.by"
	AttrRoutingOverrideEffort   = "agentisan.routing.override.effort"
	AttrRoutingTimestamp        = "agentisan.routing.timestamp"
	AttrRoutingSelection        = "agentisan.routing.selection"
	AttrRoutingApplied          = "agentisan.routing.applied"
	AttrRoutingUnchecked        = "agentisan.routing.unchecked"
)

// The values of AttrRole.
const (
	RoleBoss   = "boss"
	RoleWorker = "worker"
)

// The message events (A30): a message send queued; a dispatch the daemon
// recorded as delivered, or as failed, since it may not have reached its
// pane (A2); and a message returned to the boss once the binding it was sent
// to ended. None carries a message's body.
const (
	EventMessageQueued    = "agentisan.message.queued"
	EventMessageDelivered = "agentisan.message.delivered"
	EventMessageFailed    = "agentisan.message.failed"
	EventMessageReturned  = "agentisan.message.returned"
)

// The attribute keys of the message events.
const (
	// AttrMessageID is the message's ULID.
	AttrMessageID = "agentisan.message.id"
	// AttrMessageFromKind and AttrMessageToKind are worker, boss, pane or,
	// for a sender, daemon.
	AttrMessageFromKind = "agentisan.message.from.kind"
	AttrMessageFrom     = "agentisan.message.from.id"
	AttrMessageToKind   = "agentisan.message.to.kind"
	AttrMessageTo       = "agentisan.message.to.id"
	// AttrMessageReturnedFrom is the worker a message returned to the boss
	// was sent to; "" for any other.
	AttrMessageReturnedFrom = "agentisan.message.returned_from"
	// AttrMessageAttempts counts the dispatches the message was claimed for.
	AttrMessageAttempts = "agentisan.message.attempts"
	// AttrMessageReason is why a message failed; "" for the other events.
	AttrMessageReason = "agentisan.message.reason"
)

// EventCtxLevel is a pane's context crossing one of its rotation thresholds
// (A33), as the daemon tells its boss.
const EventCtxLevel = "agentisan.worker.ctx_level"

// The attribute keys of EventCtxLevel, beside the worker's.
const (
	// AttrCtxLevel is soft, hard or ceiling.
	AttrCtxLevel = "agentisan.ctx.level"
	// AttrCtx is the context used, as a whole percentage.
	AttrCtx = "agentisan.ctx.percent"
	// AttrCtxThreshold is the level's percentage.
	AttrCtxThreshold = "agentisan.ctx.threshold"
	// AttrCtxAlerted is whether the user was asked to be alerted: only at
	// the ceiling.
	AttrCtxAlerted = "agentisan.ctx.alerted"
	// AttrCtxShown is whether herdr showed that alert.
	AttrCtxShown = "agentisan.ctx.shown"
)

// EventHandoff is a step of a worker's handoff (A33): its handoff token set
// (HandoffReady) or cleared (HandoffCleared), and a rotation completed by
// its ack (HandoffCompleted) or failed (HandoffFailed).
const EventHandoff = "agentisan.worker.handoff"

// The keys of EventHandoff's own attributes, beside the worker's.
const (
	AttrHandoffStep = "agentisan.handoff.step"
	// AttrHandoffOldPane is the pane the worker was on: on HandoffCompleted
	// and HandoffFailed.
	AttrHandoffOldPane = "agentisan.handoff.old_pane"
	// AttrHandoffOld is how the old pane's item and stage read at the ack,
	// "matched" or "unread": on HandoffCompleted.
	AttrHandoffOld = "agentisan.handoff.old"
)

// The values of AttrHandoffStep.
const (
	HandoffReady     = "ready"
	HandoffCleared   = "cleared"
	HandoffCompleted = "completed"
	HandoffFailed    = "failed"
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

// CtxLevel is one EventCtxLevel.
type CtxLevel struct {
	WorkerID string
	Bound    bool
	Project  string
	Group    string
	Role     string
	Agent    string
	PaneID   string
	// Level is soft, hard or ceiling.
	Level          string
	Ctx, Threshold int
	Alerted, Shown bool
}

// CtxLevelCrossed emits c as an EventCtxLevel record.
func (e *Events) CtxLevelCrossed(ctx context.Context, c CtxLevel) {
	if e == nil {
		return
	}
	r := record(EventCtxLevel, fmt.Sprintf("%s at %d%% crossed %s (%d%%)", c.WorkerID, c.Ctx, c.Level, c.Threshold))
	r.AddAttributes(
		attribute.String(AttrWorkerID, c.WorkerID),
		attribute.Bool(AttrWorkerBound, c.Bound),
		attribute.String(AttrProject, c.Project),
		attribute.String(AttrGroup, c.Group),
		attribute.String(AttrRole, c.Role),
		attribute.String(AttrAgent, c.Agent),
		attribute.String(AttrPaneID, c.PaneID),
		attribute.String(AttrCtxLevel, c.Level),
		attribute.Int(AttrCtx, c.Ctx),
		attribute.Int(AttrCtxThreshold, c.Threshold),
		attribute.Bool(AttrCtxAlerted, c.Alerted),
		attribute.Bool(AttrCtxShown, c.Shown),
	)
	e.logger.Emit(ctx, r)
}

// Handoff is one EventHandoff.
type Handoff struct {
	WorkerID string
	Bound    bool
	Project  string
	Group    string
	PaneID   string
	// Step is one of the values of AttrHandoffStep.
	Step string
	// OldPane and Old are AttrHandoffOldPane's and AttrHandoffOld's; ""
	// leaves the attribute out.
	OldPane, Old string
}

// HandoffStep emits h as an EventHandoff record.
func (e *Events) HandoffStep(ctx context.Context, h Handoff) {
	if e == nil {
		return
	}
	r := record(EventHandoff, h.WorkerID+" handoff "+h.Step)
	r.AddAttributes(
		attribute.String(AttrWorkerID, h.WorkerID),
		attribute.Bool(AttrWorkerBound, h.Bound),
		attribute.String(AttrProject, h.Project),
		attribute.String(AttrGroup, h.Group),
		attribute.String(AttrPaneID, h.PaneID),
		attribute.String(AttrHandoffStep, h.Step),
	)
	if h.OldPane != "" {
		r.AddAttributes(attribute.String(AttrHandoffOldPane, h.OldPane))
	}
	if h.Old != "" {
		r.AddAttributes(attribute.String(AttrHandoffOld, h.Old))
	}
	e.logger.Emit(ctx, r)
}

// record is an INFO record named name, now, with body.
func record(name, body string) log.Record {
	var r log.Record
	r.SetEventName(name)
	r.SetTimestamp(time.Now())
	r.SetSeverity(log.SeverityInfo)
	r.SetSeverityText("INFO")
	r.SetBody(attribute.StringValue(body))
	return r
}

// Message is one message event: the message, never its body.
type Message struct {
	ID             string
	FromKind, From string
	ToKind, To     string
	ReturnedFrom   string
	Attempts       int
	Reason         string
	// At is when it happened, the record's time; zero is when it is
	// emitted.
	At time.Time
}

// MessageQueued emits m as an EventMessageQueued record.
func (e *Events) MessageQueued(ctx context.Context, m Message) {
	e.message(ctx, EventMessageQueued, "queued for", m)
}

// MessageDelivered emits m as an EventMessageDelivered record.
func (e *Events) MessageDelivered(ctx context.Context, m Message) {
	e.message(ctx, EventMessageDelivered, "delivered to", m)
}

// MessageFailed emits m as an EventMessageFailed record.
func (e *Events) MessageFailed(ctx context.Context, m Message) {
	e.message(ctx, EventMessageFailed, "failed to", m)
}

// MessageReturned emits m as an EventMessageReturned record.
func (e *Events) MessageReturned(ctx context.Context, m Message) {
	e.message(ctx, EventMessageReturned, "returned to", m)
}

func (e *Events) message(ctx context.Context, name, what string, m Message) {
	if e == nil {
		return
	}
	at := m.At
	if at.IsZero() {
		at = time.Now()
	}
	var r log.Record
	r.SetEventName(name)
	r.SetTimestamp(at)
	r.SetSeverity(log.SeverityInfo)
	r.SetSeverityText("INFO")
	r.SetBody(attribute.StringValue(m.ID + " " + what + " " + m.To))
	r.AddAttributes(
		attribute.String(AttrMessageID, m.ID),
		attribute.String(AttrMessageFromKind, m.FromKind),
		attribute.String(AttrMessageFrom, m.From),
		attribute.String(AttrMessageToKind, m.ToKind),
		attribute.String(AttrMessageTo, m.To),
		attribute.String(AttrMessageReturnedFrom, m.ReturnedFrom),
		attribute.Int(AttrMessageAttempts, m.Attempts),
		attribute.String(AttrMessageReason, m.Reason),
	)
	e.logger.Emit(ctx, r)
}

// WorkerRouted emits d as the EventWorkerRouted of one worker of a spawn.
// Every audit field is present: "" for a task id, an override, Jev's
// answers or Jev's error d has none of.
func (e *Events) WorkerRouted(ctx context.Context, workerID, project, group string, d routing.Decision) {
	if e == nil {
		return
	}
	var (
		taskID, answers string
		o               routing.Override
	)
	if d.TaskID != nil {
		taskID = *d.TaskID
	}
	if len(d.Answers) > 0 && string(d.Answers) != "null" {
		answers = string(d.Answers)
	}
	if d.Override != nil {
		o = *d.Override
	}
	var r log.Record
	r.SetEventName(EventWorkerRouted)
	r.SetTimestamp(d.Timestamp)
	r.SetSeverity(log.SeverityInfo)
	r.SetSeverityText("INFO")
	r.SetBody(attribute.StringValue(workerID + " " + d.SelectedBinding + " " + d.Effort))
	r.AddAttributes(
		attribute.String(AttrWorkerID, workerID),
		attribute.String(AttrProject, project),
		attribute.String(AttrGroup, group),
		attribute.String(AttrRoutingTaskID, taskID),
		attribute.String(AttrRoutingLookupStatus, d.TaskLookupStatus),
		attribute.String(AttrRoutingAssignmentDigest, d.AssignmentDigest),
		attribute.String(AttrRoutingRole, d.Role),
		attribute.String(AttrRoutingProvider, d.Provider),
		attribute.String(AttrRoutingBinding, d.SelectedBinding),
		attribute.String(AttrRoutingModelID, d.ModelID),
		attribute.String(AttrRoutingEffort, d.Effort),
		attribute.String(AttrRoutingPolicyVersion, d.PolicyVersion),
		attribute.String(AttrRoutingJevModel, d.JevModelReturned),
		attribute.String(AttrRoutingAnswers, answers),
		attribute.String(AttrRoutingFallbackReason, string(d.FallbackReason)),
		attribute.String(AttrRoutingJevError, d.JevError),
		attribute.String(AttrRoutingOverrideBinding, o.Binding),
		attribute.String(AttrRoutingOverrideBy, o.By),
		attribute.String(AttrRoutingOverrideEffort, o.Effort),
		attribute.String(AttrRoutingTimestamp, d.Timestamp.UTC().Format(time.RFC3339Nano)),
		attribute.String(AttrRoutingSelection, string(d.Selection)),
		attribute.Bool(AttrRoutingApplied, d.Applied),
		attribute.StringSlice(AttrRoutingUnchecked, d.Unchecked),
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

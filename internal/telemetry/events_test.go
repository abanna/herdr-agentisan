package telemetry_test

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
)

// TestStatusChangedEmitsTheSchema is the D10 contract a debugger queries
// Elastic by: the event name and every attribute, spelled out here by hand
// so a renamed key fails the test rather than silently moving the field.
func TestStatusChangedEmitsTheSchema(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   telemetry.StatusChange
		want map[string]any
	}{
		"a bound worker changes status": {
			in: telemetry.StatusChange{
				WorkerID: "coder-1", Bound: true, Project: "agentisan", Group: "coders",
				Role: telemetry.RoleWorker, Agent: "claude", PaneID: "wN:p1", From: "working", To: "idle",
			},
			want: map[string]any{
				"agentisan.worker.id": "coder-1", "agentisan.worker.bound": true,
				"agentisan.project": "agentisan", "agentisan.group": "coders", "agentisan.role": "worker",
				"agentisan.agent": "claude", "herdr.pane.id": "wN:p1",
				"agentisan.status.from": "working", "agentisan.status.to": "idle",
			},
		},
		"the boss's pane appears, unbound, in the unnamed project": {
			in: telemetry.StatusChange{
				WorkerID: "wJ:p1", Role: telemetry.RoleBoss, Agent: "claude", PaneID: "wJ:p1", From: "absent", To: "working",
			},
			want: map[string]any{
				"agentisan.worker.id": "wJ:p1", "agentisan.worker.bound": false,
				"agentisan.project": "", "agentisan.group": "", "agentisan.role": "boss",
				"agentisan.agent": "claude", "herdr.pane.id": "wJ:p1",
				"agentisan.status.from": "absent", "agentisan.status.to": "working",
			},
		},
		"a group labelled in Unicode, with a NUL, a CR and spaces, passes verbatim": {
			in: telemetry.StatusChange{
				WorkerID: "wN:p2", Project: "agentisan", Group: "cödërs \x00\r\n ◆", Role: telemetry.RoleWorker,
				Agent: "claude", PaneID: "wN:p2", From: "idle", To: "done",
			},
			want: map[string]any{
				"agentisan.worker.id": "wN:p2", "agentisan.worker.bound": false,
				"agentisan.project": "agentisan", "agentisan.group": "cödërs \x00\r\n ◆", "agentisan.role": "worker",
				"agentisan.agent": "claude", "herdr.pane.id": "wN:p2",
				"agentisan.status.from": "idle", "agentisan.status.to": "done",
			},
		},
		"a shell pane leaves": {
			in: telemetry.StatusChange{
				WorkerID: "wN:p4", Project: "agentisan", Group: "coders", Role: telemetry.RoleWorker,
				PaneID: "wN:p4", From: "unknown", To: "absent",
			},
			want: map[string]any{
				"agentisan.worker.id": "wN:p4", "agentisan.worker.bound": false,
				"agentisan.project": "agentisan", "agentisan.group": "coders", "agentisan.role": "worker",
				"agentisan.agent": "", "herdr.pane.id": "wN:p4",
				"agentisan.status.from": "unknown", "agentisan.status.to": "absent",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, rec := telemetrytest.NewEvents()
			events.StatusChanged(t.Context(), tc.in)

			got := rec.Events()
			require.Len(t, got, 1)
			assert.Equal(t, "agentisan.worker.status_changed", got[0].Name)
			assert.Equal(t, tc.want, got[0].Attrs, "exactly the schema's attributes, no more")

			records := rec.Records()
			assert.Equal(t, log.SeverityInfo, records[0].Severity())
			assert.Equal(t, "INFO", records[0].SeverityText())
			assert.Equal(t, tc.in.WorkerID+" "+tc.in.From+" → "+tc.in.To, records[0].Body().AsString(),
				"the body reads as a log line in Discover")
			assert.False(t, records[0].Timestamp().IsZero(), "the event carries when it was seen")
		})
	}
}

// TestMessageEventsEmitTheSchema is A30's contract for the four message
// events, as docs/telemetry.md lists it: each name, exactly its eight
// attributes, the body line and the severity, spelled out by hand so a
// renamed event or key fails the test rather than silently moving the field.
func TestMessageEventsEmitTheSchema(t *testing.T) {
	t.Parallel()

	m := telemetry.Message{
		ID: "01JMSG", FromKind: "worker", From: "ha-coder-1", ToKind: "boss", To: "ha-boss",
		ReturnedFrom: "ha-coder-2", Attempts: 2, Reason: "timed out",
	}
	want := map[string]any{
		"agentisan.message.id": "01JMSG", "agentisan.message.from.kind": "worker", "agentisan.message.from.id": "ha-coder-1",
		"agentisan.message.to.kind": "boss", "agentisan.message.to.id": "ha-boss",
		"agentisan.message.returned_from": "ha-coder-2", "agentisan.message.attempts": int64(2),
		"agentisan.message.reason": "timed out",
	}
	tests := map[string]struct {
		emit func(*telemetry.Events, context.Context, telemetry.Message)
		body string
	}{
		"agentisan.message.queued":    {(*telemetry.Events).MessageQueued, "01JMSG queued for ha-boss"},
		"agentisan.message.delivered": {(*telemetry.Events).MessageDelivered, "01JMSG delivered to ha-boss"},
		"agentisan.message.failed":    {(*telemetry.Events).MessageFailed, "01JMSG failed to ha-boss"},
		"agentisan.message.returned":  {(*telemetry.Events).MessageReturned, "01JMSG returned to ha-boss"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, rec := telemetrytest.NewEvents()
			tc.emit(events, t.Context(), m)

			got := rec.Events()
			require.Len(t, got, 1)
			assert.Equal(t, name, got[0].Name)
			assert.Equal(t, want, got[0].Attrs, "exactly the schema's attributes, no more")

			records := rec.Records()
			assert.Equal(t, log.SeverityInfo, records[0].Severity())
			assert.Equal(t, "INFO", records[0].SeverityText())
			assert.Equal(t, tc.body, records[0].Body().AsString(), "the message and its recipient, never its body")
			assert.False(t, records[0].Timestamp().IsZero(), "the event carries when it was seen")
		})
	}
}

// TestAMessageEventTakesItsTime: a message event's record carries the time
// it is given, At, and when it is given none, the time it is emitted.
func TestAMessageEventTakesItsTime(t *testing.T) {
	t.Parallel()
	events, rec := telemetrytest.NewEvents()
	at := time.Date(2026, 10, 22, 5, 0, 0, 123456789, time.UTC)
	events.MessageQueued(t.Context(), telemetry.Message{ID: "01JA", To: "ha-boss", At: at})
	before := time.Now()
	events.MessageDelivered(t.Context(), telemetry.Message{ID: "01JA", To: "ha-boss"})
	after := time.Now()

	records := rec.Records()
	require.Len(t, records, 2)
	assert.True(t, records[0].Timestamp().Equal(at), "the time it was given: %v", records[0].Timestamp())
	assert.False(t, records[1].Timestamp().Before(before), "no earlier than its emit")
	assert.False(t, records[1].Timestamp().After(after), "no later than its emit")
}

// TestNilEventsEmitNothing: the daemon holds a nil *Events when no endpoint
// is configured, and every emit is then a silent no-op, never a panic.
func TestNilEventsEmitNothing(t *testing.T) {
	t.Parallel()
	var events *telemetry.Events
	assert.NotPanics(t, func() {
		events.StatusChanged(t.Context(), telemetry.StatusChange{WorkerID: "w", From: "idle", To: "working"})
		events.WorkerRouted(t.Context(), "w", "p", "coder", routing.Decision{})
		events.CtxLevelCrossed(t.Context(), telemetry.CtxLevel{WorkerID: "w", Level: "soft"})
	})
}

// TestWorkerRoutedEmitsTheSchema is A50's audit as the debugger queries it:
// every audit field under agentisan.routing, present even when the decision
// has none of it, spelled out by hand so a renamed key fails here.
func TestWorkerRoutedEmitsTheSchema(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 21, 12, 0, 0, 5, time.FixedZone("CDT", -5*3600))
	id := "NERD-5289"
	base := map[string]any{
		"agentisan.worker.id": "ha-coder-1", "agentisan.project": "herdr-agentisan", "agentisan.group": "coder",
		"agentisan.routing.role": "coder", "agentisan.routing.provider": "claude",
		"agentisan.routing.selected_binding": "claude_mid", "agentisan.routing.model_id": "claude-sonnet-5-5",
		"agentisan.routing.effort": "high", "agentisan.routing.policy_version": "v1-0123456789ab",
		"agentisan.routing.jev_model_returned": "", "agentisan.routing.selection": "jev_complexity",
		"agentisan.routing.timestamp": "2026-10-21T17:00:00.000000005Z",
		"agentisan.routing.unchecked": []string{"tool_compatibility", "context_headroom", "availability", "spending_limits"},
	}
	with := func(kv map[string]any) map[string]any {
		out := maps.Clone(base)
		maps.Copy(out, kv)
		return out
	}
	decision := routing.Decision{
		TaskLookupStatus: routing.LookupNotFound, Role: "coder", Provider: "claude", SelectedBinding: "claude_mid",
		ModelID: "claude-sonnet-5-5", Effort: "high", PolicyVersion: "v1-0123456789ab", FallbackReason: routing.ReasonTaskNotFound,
		Timestamp: at, Selection: "jev_complexity", Unchecked: routing.Unchecked,
		// JSON's null, as a decision read back from JSON carries no answers.
		Answers: json.RawMessage("null"),
	}
	overridden := decision
	overridden.TaskID, overridden.TaskLookupStatus, overridden.AssignmentDigest = &id, routing.LookupFound, "sha256:ab"
	overridden.Override, overridden.FallbackReason, overridden.Applied = &routing.Override{Binding: "claude_mid", By: "boss", Effort: "high"}, routing.ReasonNone, true
	overridden.Answers = json.RawMessage(`{"complexity":"bounded"}`)
	tests := map[string]struct {
		in   routing.Decision
		want map[string]any
	}{
		"would have, with nothing to name": {in: decision, want: with(map[string]any{
			"agentisan.routing.task_id": "", "agentisan.routing.task_lookup_status": "not_found", "agentisan.routing.assignment_digest": "",
			"agentisan.routing.answers": "", "agentisan.routing.fallback_reason": "task_not_found",
			"agentisan.routing.override.binding": "", "agentisan.routing.override.by": "", "agentisan.routing.override.effort": "",
			"agentisan.routing.applied": false,
		})},
		"applied, with a task and an override": {in: overridden, want: with(map[string]any{
			"agentisan.routing.task_id": id, "agentisan.routing.task_lookup_status": "found", "agentisan.routing.assignment_digest": "sha256:ab",
			"agentisan.routing.answers": `{"complexity":"bounded"}`, "agentisan.routing.fallback_reason": "",
			"agentisan.routing.override.binding": "claude_mid", "agentisan.routing.override.by": "boss", "agentisan.routing.override.effort": "high",
			"agentisan.routing.applied": true,
		})},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, rec := telemetrytest.NewEvents()

			events.WorkerRouted(t.Context(), "ha-coder-1", "herdr-agentisan", "coder", tc.in)

			got := rec.Events()
			require.Len(t, got, 1)
			assert.Equal(t, "agentisan.worker.routed", got[0].Name)
			assert.Equal(t, tc.want, got[0].Attrs, "exactly the schema's attributes, no more")
			records := rec.Records()
			assert.Equal(t, log.SeverityInfo, records[0].Severity())
			assert.Equal(t, "ha-coder-1 claude_mid high", records[0].Body().AsString())
			assert.True(t, records[0].Timestamp().Equal(at), "the decision's own time")
		})
	}
}

// TestCtxLevelEmitsTheSchema is A33's contract for the ctx level event, as
// docs/telemetry.md lists it: its name, exactly its twelve attributes, the
// body line and the severity, spelled out by hand.
func TestCtxLevelEmitsTheSchema(t *testing.T) {
	t.Parallel()
	events, rec := telemetrytest.NewEvents()
	events.CtxLevelCrossed(t.Context(), telemetry.CtxLevel{
		WorkerID: "ha-coder-1", Bound: true, Project: "ha", Group: "cödërs \x00\r\n ◆", Role: telemetry.RoleWorker,
		Agent: "claude", PaneID: "w1:p1", Level: "ceiling", Ctx: 78, Threshold: 75, Alerted: true, Shown: false,
	})

	got := rec.Events()
	require.Len(t, got, 1)
	assert.Equal(t, "agentisan.worker.ctx_level", got[0].Name)
	assert.Equal(t, map[string]any{
		"agentisan.worker.id": "ha-coder-1", "agentisan.worker.bound": true,
		"agentisan.project": "ha", "agentisan.group": "cödërs \x00\r\n ◆", "agentisan.role": "worker",
		"agentisan.agent": "claude", "herdr.pane.id": "w1:p1",
		"agentisan.ctx.level": "ceiling", "agentisan.ctx.percent": int64(78), "agentisan.ctx.threshold": int64(75),
		"agentisan.ctx.alerted": true, "agentisan.ctx.shown": false,
	}, got[0].Attrs, "exactly the schema's attributes, no more")
	records := rec.Records()
	assert.Equal(t, log.SeverityInfo, records[0].Severity())
	assert.Equal(t, "INFO", records[0].SeverityText())
	assert.Equal(t, "ha-coder-1 at 78% crossed ceiling (75%)", records[0].Body().AsString())
	assert.False(t, records[0].Timestamp().IsZero())
}

// TestSetupEventsWithoutAnEndpointIsANoop: with no endpoint (the default,
// and .env.example's) nothing is built and nothing is exported.
func TestSetupEventsWithoutAnEndpointIsANoop(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.OTLPEndpoint = ""

	events, shutdown, err := telemetry.SetupEvents(t.Context(), cfg)
	require.NoError(t, err)
	assert.Nil(t, events, "no endpoint, no emitter")
	require.NotNil(t, shutdown)
	require.NoError(t, shutdown(t.Context()))
}

// TestSetupEventsExportsOverOTLP is the end-to-end proof for events: one
// emitted through SetupEvents reaches an OTLP/HTTP receiver on loopback at
// /v1/logs, with its name, its attributes and the same resource traces
// carry. It also proves the log resource merges with the SDK's schema, the
// failure telemetry.go warns only shows once an endpoint is configured.
func TestSetupEventsExportsOverOTLP(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var bodies []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		mu.Lock()
		bodies = append(bodies, r.URL.Path+"|"+string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	cfg := testConfig()
	cfg.OTLPEndpoint = strings.TrimPrefix(collector.URL, "http://")
	events, shutdown, err := telemetry.SetupEvents(t.Context(), cfg)
	require.NoError(t, err)
	require.NotNil(t, events)

	events.StatusChanged(t.Context(), telemetry.StatusChange{
		WorkerID: "coder-1", Bound: true, Project: "agentisan", Group: "coders",
		Role: telemetry.RoleWorker, Agent: "claude", PaneID: "wN:p1", From: "working", To: "blocked",
	})
	// Shutdown flushes the batch, so the export has landed once it returns.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1, "one export carrying the one event")
	got := bodies[0]
	assert.True(t, strings.HasPrefix(got, "/v1/logs|"), "events go to the OTLP logs path, got %.40q", got)
	for _, want := range []string{
		"agentisan.worker.status_changed", "agentisan.worker.id", "coder-1", "herdr.pane.id", "wN:p1",
		"agentisan.status.from", "working", "agentisan.status.to", "blocked",
		"herdr-agentisan", "deadbeef", // the resource: service.name and the commit
	} {
		assert.Contains(t, got, want)
	}
}

// TestSetupEventsShutdownIsBounded: a collector that never answers costs the
// daemon's exit no more than the deadline it flushes under.
func TestSetupEventsShutdownIsBounded(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer collector.Close()
	defer close(release) // runs first: the hung handler returns before Close waits for it

	cfg := testConfig()
	cfg.OTLPEndpoint = strings.TrimPrefix(collector.URL, "http://")
	events, shutdown, err := telemetry.SetupEvents(t.Context(), cfg)
	require.NoError(t, err)
	events.StatusChanged(t.Context(), telemetry.StatusChange{WorkerID: "w", From: "idle", To: "working"})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = shutdown(ctx)
	assert.Less(t, time.Since(start), 5*time.Second, "shutdown honours its deadline")
	require.Error(t, err, "a flush that did not land is reported")
}

// TestSetupEventsIgnoresInheritedOTLPVariables: the daemon inherits herdr's
// environment, which can carry OTEL_EXPORTER_OTLP_* meant for some other
// program. The events still go to the endpoint SetupEvents was given, at
// /v1/logs, with no header the environment added: nothing is sent to, or
// carries credentials for, a collector the configuration did not name.
//
// Not parallel: it sets the process environment.
func TestSetupEventsIgnoresInheritedOTLPVariables(t *testing.T) {
	type hit struct{ path, auth, body string }
	serve := func(hits *[]hit, mu *sync.Mutex) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			*hits = append(*hits, hit{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)})
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	}
	var mu sync.Mutex
	var ours, theirs []hit
	collector, other := serve(&ours, &mu), serve(&theirs, &mu)
	defer collector.Close()
	defer other.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", other.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", other.URL+"/elsewhere")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer leaked")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "authorization=Bearer leaked")
	t.Setenv("OTEL_SERVICE_NAME", "impostor-service")

	cfg := testConfig()
	cfg.OTLPEndpoint = strings.TrimPrefix(collector.URL, "http://")
	events, shutdown, err := telemetry.SetupEvents(t.Context(), cfg)
	require.NoError(t, err)
	events.StatusChanged(t.Context(), telemetry.StatusChange{WorkerID: "w", From: "idle", To: "working"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx))

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, theirs, "nothing goes to the collector the environment names")
	require.Len(t, ours, 1)
	assert.Equal(t, "/v1/logs", ours[0].path)
	assert.Empty(t, ours[0].auth, "no inherited header")
	assert.Contains(t, ours[0].body, "herdr-agentisan", "service.name is the configured one")
	assert.NotContains(t, ours[0].body, "impostor-service", "OTEL_SERVICE_NAME does not rename the events' source")
}

// TestSetupEventsWithAnUnusableEndpoint: SetupEvents takes the endpoint it
// is given. One the exporter cannot use, which the daemon's own check
// (settings) refuses before it gets here, still never panics or hangs: it is
// refused at setup, or every export fails and the flush ends inside its
// deadline.
//
// Each case is a loopback address and a port this test holds open, made
// malformed, so whatever the exporter makes of it reaches nothing but that
// port and never asks a resolver: IDNA drops a byte-order mark, which leaves
// the owned address, never a collector the machine runs. A host that needs
// resolving (non-ASCII, invalid UTF-8, like a flag) and a missing port
// (which dials :80) cannot be built that way, so only settings tries them
// (TestTelemetryEndpointErrors, TestTelemetryEndpointChecksTheOverride).
func TestSetupEventsWithAnUnusableEndpoint(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	owned := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(owned.Close)
	t.Cleanup(func() { close(release) }) // runs first: a hung handler returns before Close waits for it
	addr := strings.TrimPrefix(owned.URL, "http://")
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.NotNil(t, net.ParseIP(host), "an address, so no case is ever looked up")

	for name, endpoint := range map[string]string{
		"a NUL byte":             host + "\x00:" + port,
		"a byte-order mark":      "\uFEFF" + addr,
		"a trailing CR":          addr + "\r",
		"white space":            host + " :" + port,
		"a URL, not host:port":   "http://" + addr,
		"64 KiB of port":         host + ":" + strings.Repeat("9", 64<<10),
		"a port that is no port": host + ":99999",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			cfg.OTLPEndpoint = endpoint
			events, shutdown, err := telemetry.SetupEvents(t.Context(), cfg)
			if err != nil {
				assert.Nil(t, events)
				return
			}
			require.NotNil(t, events)
			assert.NotPanics(t, func() {
				events.StatusChanged(t.Context(), telemetry.StatusChange{WorkerID: "w", From: "idle", To: "working"})
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			_ = shutdown(ctx) // an export that cannot be sent is reported, or dropped
			assert.Less(t, time.Since(start), 5*time.Second, "the flush ends inside its deadline")
		})
	}
}

package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"

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

// TestNilEventsEmitNothing: the daemon holds a nil *Events when no endpoint
// is configured, and every emit is then a silent no-op, never a panic.
func TestNilEventsEmitNothing(t *testing.T) {
	t.Parallel()
	var events *telemetry.Events
	assert.NotPanics(t, func() {
		events.StatusChanged(t.Context(), telemetry.StatusChange{WorkerID: "w", From: "idle", To: "working"})
	})
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
func TestSetupEventsWithAnUnusableEndpoint(t *testing.T) {
	t.Parallel()
	for name, endpoint := range map[string]string{
		"a non-ASCII host":       "kollektör:4318",
		"invalid UTF-8":          "local\xffhost:4318",
		"a NUL byte":             "localhost\x00:4318",
		"a byte-order mark":      "\uFEFFlocalhost:4318",
		"a trailing CR":          "localhost:4318\r",
		"white space":            "local host:4318",
		"like a flag":            "--help",
		"a URL, not host:port":   "http://localhost:4318",
		"no port":                "localhost",
		"64 KiB of host":         strings.Repeat("a", 64<<10) + ":4318",
		"a port that is no port": "localhost:99999",
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

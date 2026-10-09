package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/nerds-run/go-agents/internal/telemetry"
)

// traceIDOf reports the trace ID on ctx, or "" when no span is present.
func traceIDOf(ctx context.Context) string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func testConfig() telemetry.Config {
	return telemetry.Config{
		ServiceName: "go-agents",
		Version:     "v1.2.3",
		Commit:      "deadbeef",
		Env:         "development",
		SampleRatio: 1,
	}
}

// gather pulls the current metric families keyed by name.
func gather(t *testing.T, r *telemetry.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := r.Gatherer().Gather()
	require.NoError(t, err)

	out := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

// labelsOf renders one metric's label set as a map so assertions read plainly.
func labelsOf(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

// TestNewRegistryStampsBuildInfo is the deployment-observability claim: every
// scrape carries the version and commit of the binary that served it.
func TestNewRegistryStampsBuildInfo(t *testing.T) {
	t.Parallel()

	families := gather(t, telemetry.NewRegistry(testConfig()))

	build := families["build_info"]
	require.NotNil(t, build, "build_info must be exported")
	require.Len(t, build.GetMetric(), 1)

	assert.Equal(t, map[string]string{
		"version": "v1.2.3", "commit": "deadbeef", "env": "development",
	}, labelsOf(build.GetMetric()[0]))
	assert.InDelta(t, 1.0, build.GetMetric()[0].GetGauge().GetValue(), 0)
}

// TestRegistryIsIsolatedFromTheDefault proves two registries can coexist. The
// default registry would panic on the second duplicate registration, which is
// exactly what breaks a test binary that starts more than one server.
func TestRegistryIsIsolatedFromTheDefault(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		telemetry.NewRegistry(testConfig())
		telemetry.NewRegistry(testConfig())
	})
}

// routerWithMetrics returns an engine carrying the middleware and one
// parameterised route, which is what makes the cardinality assertion possible.
func routerWithMetrics(reg *telemetry.Registry) *gin.Engine {
	r := gin.New()
	r.Use(reg.Middleware())
	r.GET("/v1/notes/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.DELETE("/v1/notes/:id", func(c *gin.Context) { c.Status(http.StatusNotFound) })
	return r
}

func get(r http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// TestMiddlewareLabelsWithTheRouteTemplate is the cardinality guard. Labelling
// with the raw path would mint one time series per note ID and eventually take
// Prometheus down; the middleware must use gin's matched template instead.
func TestMiddlewareLabelsWithTheRouteTemplate(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	r := routerWithMetrics(reg)

	for _, id := range []string{"aaa", "bbb", "ccc"} {
		require.Equal(t, http.StatusOK, get(r, http.MethodGet, "/v1/notes/"+id).Code)
	}

	requests := gather(t, reg)["http_requests_total"]
	require.NotNil(t, requests)
	require.Len(t, requests.GetMetric(), 1, "three distinct IDs must collapse to one series")

	m := requests.GetMetric()[0]
	assert.Equal(t, map[string]string{
		"method": http.MethodGet, "route": "/v1/notes/:id", "status": "200",
	}, labelsOf(m))
	assert.InDelta(t, 3.0, m.GetCounter().GetValue(), 0)
}

// TestMiddlewareSeparatesStatusAndMethod proves the RED breakdown is real:
// errors are distinguishable from successes on the same route.
func TestMiddlewareSeparatesStatusAndMethod(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	r := routerWithMetrics(reg)

	get(r, http.MethodGet, "/v1/notes/x")
	get(r, http.MethodDelete, "/v1/notes/x")

	requests := gather(t, reg)["http_requests_total"]
	require.NotNil(t, requests)

	seen := map[string]float64{}
	for _, m := range requests.GetMetric() {
		l := labelsOf(m)
		seen[l["method"]+" "+l["status"]] = m.GetCounter().GetValue()
	}
	assert.Equal(t, map[string]float64{"GET 200": 1, "DELETE 404": 1}, seen)
}

// TestMiddlewareBucketsUnmatchedRequests: a 404 for an unrouted URL must not
// create a series per URL either, or a scanner would do the damage instead.
func TestMiddlewareBucketsUnmatchedRequests(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	r := routerWithMetrics(reg)

	get(r, http.MethodGet, "/wp-admin.php")
	get(r, http.MethodGet, "/.env")

	requests := gather(t, reg)["http_requests_total"]
	require.NotNil(t, requests)
	require.Len(t, requests.GetMetric(), 1)
	assert.Equal(t, "unmatched", labelsOf(requests.GetMetric()[0])["route"])
}

func TestMiddlewareObservesDuration(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	r := routerWithMetrics(reg)
	get(r, http.MethodGet, "/v1/notes/x")

	duration := gather(t, reg)["http_request_duration_seconds"]
	require.NotNil(t, duration)
	require.Len(t, duration.GetMetric(), 1)

	h := duration.GetMetric()[0].GetHistogram()
	assert.Equal(t, uint64(1), h.GetSampleCount())
	// The buckets are hand-picked for a service answering from memory; the
	// first one existing at 1ms is what keeps that useful.
	require.NotEmpty(t, h.GetBucket())
	assert.InDelta(t, 0.001, h.GetBucket()[0].GetUpperBound(), 0)
}

// TestMiddlewareReturnsInflightToZero: a gauge that leaks on every request
// reads as a permanent pile-up and would fire the saturation alert forever.
func TestMiddlewareReturnsInflightToZero(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	r := routerWithMetrics(reg)
	for range 5 {
		get(r, http.MethodGet, "/v1/notes/x")
	}

	inflight := gather(t, reg)["http_requests_in_flight"]
	require.NotNil(t, inflight)
	require.Len(t, inflight.GetMetric(), 1)
	assert.InDelta(t, 0.0, inflight.GetMetric()[0].GetGauge().GetValue(), 0)
}

func TestMiddlewareCountsInflightDuringTheRequest(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	var during float64

	r := gin.New()
	r.Use(reg.Middleware())
	r.GET("/slow", func(c *gin.Context) {
		families, err := reg.Gatherer().Gather()
		require.NoError(t, err)
		for _, f := range families {
			if f.GetName() == "http_requests_in_flight" {
				during = f.GetMetric()[0].GetGauge().GetValue()
			}
		}
		c.Status(http.StatusOK)
	})
	get(r, http.MethodGet, "/slow")

	assert.InDelta(t, 1.0, during, 0, "the gauge must be raised while the handler runs")
}

func TestAdminHandlerServesMetrics(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	get(routerWithMetrics(reg), http.MethodGet, "/v1/notes/x")

	w := get(reg.AdminHandler(), http.MethodGet, "/metrics")
	require.Equal(t, http.StatusOK, w.Code)

	body := w.Body.String()
	for _, want := range []string{
		"http_requests_total", "http_request_duration_seconds",
		"http_requests_in_flight", "build_info",
		// Registered by the Go and process collectors; their absence would
		// mean the runtime is invisible even though the app metrics are not.
		"go_goroutines", "process_open_fds",
	} {
		assert.Containsf(t, body, want, "scrape must expose %s", want)
	}
}

func TestAdminHandlerServesPprof(t *testing.T) {
	t.Parallel()

	h := telemetry.NewRegistry(testConfig()).AdminHandler()

	for _, path := range []string{
		"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/goroutine", "/debug/pprof/cmdline",
	} {
		w := get(h, http.MethodGet, path)
		assert.Equalf(t, http.StatusOK, w.Code, "path %s", path)
	}
}

// TestAdminHandlerDoesNotServeTheAPI proves the split is a real boundary and
// not just two mounts on one mux: the admin port answers nothing else.
func TestAdminHandlerDoesNotServeTheAPI(t *testing.T) {
	t.Parallel()

	h := telemetry.NewRegistry(testConfig()).AdminHandler()

	for _, path := range []string{"/v1/notes", "/healthz", "/"} {
		w := get(h, http.MethodGet, path)
		assert.Equalf(t, http.StatusNotFound, w.Code, "path %s must not be served here", path)
	}
}

// TestAdminServerTimeouts pins the pprof invariant: a write deadline shorter
// than the profile duration truncates every CPU profile, so WriteTimeout must
// stay zero while ReadHeaderTimeout still bounds a slowloris client.
func TestAdminServerTimeouts(t *testing.T) {
	t.Parallel()

	srv := telemetry.NewRegistry(testConfig()).AdminServer(":9090")

	assert.Equal(t, ":9090", srv.Addr)
	assert.Positive(t, srv.ReadHeaderTimeout, "an unbounded header read is a slowloris hole")
	assert.Zero(t, srv.WriteTimeout, "a write deadline would truncate /debug/pprof/profile")
	assert.NotNil(t, srv.Handler)
}

// TestSetupTracingWithoutAnEndpointIsANoop: `go run` on a laptop has no
// collector, and the binary must still start and still propagate context.
func TestSetupTracingWithoutAnEndpointIsANoop(t *testing.T) {
	cfg := testConfig()
	cfg.OTLPEndpoint = ""

	shutdown, err := telemetry.SetupTracing(t.Context(), cfg)
	require.NoError(t, err)
	require.NotNil(t, shutdown)
	require.NoError(t, shutdown(t.Context()))

	// Propagation is installed either way: a service that cannot export its
	// own spans should still pass a trace context to the next hop.
	assert.Contains(t, otel.GetTextMapPropagator().Fields(), "traceparent")
}

// TestSetupTracingExportsSpansOverOTLP is the end-to-end proof that tracing is
// wired rather than merely imported: a span recorded through the global
// provider reaches an OTLP/HTTP receiver.
func TestSetupTracingExportsSpansOverOTLP(t *testing.T) {
	var posted atomic.Int64
	received := make(chan string, 4)

	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		posted.Add(1)
		select {
		case received <- r.URL.Path + "|" + string(body):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	cfg := testConfig()
	cfg.OTLPEndpoint = strings.TrimPrefix(collector.URL, "http://")

	shutdown, err := telemetry.SetupTracing(t.Context(), cfg)
	require.NoError(t, err)

	_, span := otel.Tracer("test").Start(t.Context(), "unit-test-span")
	span.End()

	// Shutdown flushes the batcher, so the export must have landed by the
	// time it returns.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx))

	require.Positive(t, posted.Load(), "the collector received no export")

	select {
	case got := <-received:
		assert.Contains(t, got, "/v1/traces")
		assert.Contains(t, got, "unit-test-span", "the span name must be in the payload")
		assert.Contains(t, got, "go-agents", "the resource must carry service.name")
		assert.Contains(t, got, "deadbeef", "the resource must carry the commit")
	default:
		t.Fatal("no export body captured")
	}
}

// TestTracingMiddlewareOpensASpan proves the gin middleware is real
// instrumentation: the handler sees a recording span on its context.
func TestTracingMiddlewareOpensASpan(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	cfg := testConfig()
	cfg.OTLPEndpoint = strings.TrimPrefix(collector.URL, "http://")
	shutdown, err := telemetry.SetupTracing(t.Context(), cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, shutdown(t.Context())) }()

	var traceID string
	r := gin.New()
	r.Use(telemetry.TracingMiddleware("go-agents"))
	r.GET("/v1/notes/:id", func(c *gin.Context) {
		traceID = traceIDOf(c.Request.Context())
		c.Status(http.StatusOK)
	})

	require.Equal(t, http.StatusOK, get(r, http.MethodGet, "/v1/notes/x").Code)
	assert.NotEmpty(t, traceID, "no span was placed on the request context")
	assert.NotEqual(t, "00000000000000000000000000000000", traceID)
}

// TestTracingMiddlewareSkipsProbes: the kubelet probes every few seconds per
// pod, so tracing them buries every real request in the trace store and spends
// the sampling budget on liveness.
func TestTracingMiddlewareSkipsProbes(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	cfg := testConfig()
	cfg.OTLPEndpoint = strings.TrimPrefix(collector.URL, "http://")
	shutdown, err := telemetry.SetupTracing(t.Context(), cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, shutdown(t.Context())) }()

	traced := map[string]bool{}
	r := gin.New()
	r.Use(telemetry.TracingMiddleware("go-agents"))
	record := func(c *gin.Context) {
		traced[c.FullPath()] = traceIDOf(c.Request.Context()) != ""
		c.Status(http.StatusOK)
	}
	r.GET("/healthz", record)
	r.GET("/readyz", record)
	r.GET("/v1/notes", record)

	for _, p := range []string{"/healthz", "/readyz", "/v1/notes"} {
		require.Equal(t, http.StatusOK, get(r, http.MethodGet, p).Code)
	}

	assert.False(t, traced["/healthz"], "liveness probes must not be traced")
	assert.False(t, traced["/readyz"], "readiness probes must not be traced")
	assert.True(t, traced["/v1/notes"], "real traffic must still be traced")
}

// TestMetricsStillCoverProbes is the other half of the trade: probes are
// dropped from TRACES, never from metrics. Their success rate is exactly what
// GoAgentsReadinessFailing alerts on.
func TestMetricsStillCoverProbes(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())
	r := gin.New()
	r.Use(reg.Middleware())
	r.GET("/readyz", func(c *gin.Context) { c.Status(http.StatusServiceUnavailable) })
	get(r, http.MethodGet, "/readyz")

	requests := gather(t, reg)["http_requests_total"]
	require.NotNil(t, requests)
	require.Len(t, requests.GetMetric(), 1)
	assert.Equal(t, map[string]string{
		"method": http.MethodGet, "route": "/readyz", "status": "503",
	}, labelsOf(requests.GetMetric()[0]))
}

// TestMiddlewareSurvivesAPanickingHandler is the case the happy-path inflight
// test claimed to cover and did not. gin.Recovery sits OUTSIDE the observers,
// so a panic unwinds through the metrics middleware; before the defer, the
// gauge was left permanently +1 and the request vanished from the RED metrics.
func TestMiddlewareSurvivesAPanickingHandler(t *testing.T) {
	t.Parallel()

	reg := telemetry.NewRegistry(testConfig())

	// Recovery FIRST, exactly as api.Router registers it — otherwise this test
	// would exercise an ordering the service does not have.
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(reg.Middleware())
	r.GET("/boom", func(*gin.Context) { panic("handler exploded") })
	r.GET("/fine", func(c *gin.Context) { c.Status(http.StatusOK) })

	require.Equal(t, http.StatusInternalServerError, get(r, http.MethodGet, "/boom").Code)
	require.Equal(t, http.StatusOK, get(r, http.MethodGet, "/fine").Code)
	require.Equal(t, http.StatusInternalServerError, get(r, http.MethodGet, "/boom").Code)

	families := gather(t, reg)

	inflight := families["http_requests_in_flight"]
	require.NotNil(t, inflight)
	assert.InDelta(t, 0.0, inflight.GetMetric()[0].GetGauge().GetValue(), 0,
		"two panics left the gauge high; the saturation alert would have a permanent floor")

	// The panicking requests must be COUNTED, and counted as failures: a 200
	// here would hide them from GoAgentsHighErrorRate, which is the alert whose
	// whole job is noticing them.
	seen := map[string]float64{}
	requests := families["http_requests_total"]
	require.NotNil(t, requests)
	for _, m := range requests.GetMetric() {
		l := labelsOf(m)
		seen[l["route"]+" "+l["status"]] = m.GetCounter().GetValue()
	}
	assert.Equal(t, map[string]float64{"/boom 500": 2, "/fine 200": 1}, seen)

	// And they must appear in the latency histogram, or p99 is computed over a
	// population that silently excludes the slowest failures.
	duration := families["http_request_duration_seconds"]
	require.NotNil(t, duration)
	var boom uint64
	for _, m := range duration.GetMetric() {
		if labelsOf(m)["route"] == "/boom" {
			boom = m.GetHistogram().GetSampleCount()
		}
	}
	assert.Equal(t, uint64(2), boom, "panicking requests must still be timed")
}

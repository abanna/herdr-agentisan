// Package telemetry wires OpenTelemetry tracing and Prometheus metrics.
//
// Both are real exporters, not stubs: traces go over OTLP to a collector and
// metrics are scraped from a /metrics endpoint. Against the local kind cluster
// (infrastructure/local/k8s) that means an actual collector and an actual Prometheus. The
// same code points at a real backend by changing one environment variable.
//
// Metrics and pprof are served on a SEPARATE admin port from the API. Exposing
// /debug/pprof on a public listener hands an attacker heap dumps and goroutine
// stacks; keeping it on an unpublished port means a Service can decline to
// route it.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
)

// adminReadHeaderTimeout bounds the header read on the admin listener. The
// body and response are deliberately NOT bounded — see AdminServer.
const adminReadHeaderTimeout = 10 * time.Second

// Config is the subset of runtime configuration telemetry needs.
type Config struct {
	ServiceName string
	Version     string
	Commit      string
	Env         string
	// OTLPEndpoint is the collector host:port. Empty disables tracing export,
	// which is what a plain `go run` on a laptop wants.
	OTLPEndpoint string
	// SampleRatio is the head sampling ratio, 0..1.
	SampleRatio float64
}

// Registry holds this process's metrics. A dedicated registry rather than the
// global default keeps test runs isolated: two servers in one test binary
// would panic on duplicate registration against the default.
type Registry struct {
	reg *prometheus.Registry

	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	inflight  prometheus.Gauge
	buildInfo *prometheus.GaugeVec
}

// NewRegistry builds the metric set and records build provenance.
//
// buildInfo is the deployment-observability primitive: every sample carries
// the version and commit serving it, so a dashboard can overlay "what changed"
// on "when the graph moved" without a separate deploy-marker system.
func NewRegistry(cfg Config) *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	r := &Registry{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests by method, route and status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "HTTP request latency by method and route.",
			// Buckets span 1ms to ~4s: this service answers from memory or a
			// local file, so the default buckets would pile into the first.
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 4},
		}, []string{"method", "route"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Requests currently being served.",
		}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "build_info",
			Help: "Build provenance of the running binary; always 1.",
		}, []string{"version", "commit", "env"}),
	}
	reg.MustRegister(r.requests, r.duration, r.inflight, r.buildInfo)
	r.buildInfo.WithLabelValues(cfg.Version, cfg.Commit, cfg.Env).Set(1)
	return r
}

// Middleware records RED metrics per request. It uses gin's matched route
// template, never the raw path: labelling with the raw path would mint a new
// time series per note ID and blow up cardinality.
func (r *Registry) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "" {
			// Unmatched request: bucket them together rather than by URL.
			route = "unmatched"
		}

		r.inflight.Inc()
		start := time.Now()

		// EVERYTHING below runs in a defer, because a panicking handler
		// unwinds straight through this frame. gin.Recovery() is registered
		// OUTSIDE the observers (see api.Router), so it catches the panic in
		// an enclosing frame — it does not stop the unwind here. Without the
		// defer, a panic left inflight permanently +1 and dropped the request
		// from the RED metrics entirely: the process survives, so a handful of
		// panics builds a permanent floor under the saturation alert out of
		// requests that no longer exist.
		completed := false
		defer func() {
			r.inflight.Dec()

			status := c.Writer.Status()
			if !completed {
				// Unwinding. gin.Recovery has not run yet, so the writer still
				// reports its default 200 — recording that would hide panics
				// from the very alert meant to catch them, and the request
				// would show as a success. It is about to become a 500.
				status = http.StatusInternalServerError
			}

			r.duration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
			r.requests.WithLabelValues(
				c.Request.Method, route, strconv.Itoa(status),
			).Inc()
		}()

		c.Next()
		completed = true
	}
}

// AdminHandler serves /metrics and the pprof endpoints. Mount it on a port
// that is not published to users.
func (r *Registry) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{Registry: r.reg}))

	// Registered explicitly rather than by importing net/http/pprof for its
	// side effect, which would attach these to http.DefaultServeMux and leak
	// them onto any other server sharing it.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return mux
}

// AdminServer returns the http.Server that should carry AdminHandler.
//
// WriteTimeout is deliberately zero. `/debug/pprof/profile` blocks for its
// `seconds` parameter (30 by default) before writing a byte, so any write
// deadline shorter than that truncates every CPU profile the endpoint exists
// to produce. ReadHeaderTimeout is what actually bounds a slowloris client,
// and it is set — this is not an oversight, and gosec G112 is satisfied.
func (r *Registry) AdminServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           r.AdminHandler(),
		ReadHeaderTimeout: adminReadHeaderTimeout,
	}
}

// Gatherer exposes the registry for tests that assert on collected metrics.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// probeRoutes are not traced. The kubelet hits /readyz every 5 seconds and
// /healthz every 10, per pod, forever — on an idle service that is essentially
// all the traffic. Tracing it buries every real request in the trace store,
// spends the sampling budget on liveness, and makes "show me recent traces"
// useless. Their health is already carried by metrics and by the probe result
// itself, which is what the kubelet acts on.
var probeRoutes = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
}

// TracingMiddleware returns the gin middleware that opens a server span per
// request and continues an incoming W3C traceparent.
//
// It lives here rather than in internal/api so the router stays independent of
// what observes it: api takes middleware as an option and never imports this
// package.
func TracingMiddleware(serviceName string) gin.HandlerFunc {
	return otelgin.Middleware(serviceName, otelgin.WithGinFilter(func(c *gin.Context) bool {
		// FullPath is the matched route template, so this cannot be dodged by
		// a caller appending a query string or altering the case of the path.
		return !probeRoutes[c.FullPath()]
	}))
}

// SetupTracing installs a global tracer provider exporting over OTLP, and
// returns a shutdown function that flushes pending spans.
//
// With an empty OTLPEndpoint it installs propagation only and returns a no-op
// shutdown, so the binary runs unchanged on a laptop with no collector.
func SetupTracing(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// W3C propagation is installed either way: a service that cannot export
	// its own spans should still pass a trace context through to the next hop.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if cfg.OTLPEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.OTLPEndpoint),
		// Plaintext: the collector is a local sidecar or in-cluster service.
		// Point this at anything crossing a network boundary and this must go.
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp trace exporter: %w", err)
	}

	// resource.Merge REFUSES two different schema URLs, so this semconv import
	// must track the schema resource.Default() declares for the SDK version in
	// go.mod. Drifting them does not fail the build — it fails at startup, the
	// moment an OTLP endpoint is configured, which is the worst place to find
	// out. TestSetupTracingExportsSpansOverOTLP is what turns that into a test
	// failure instead; if it starts reporting "conflicting Schema URL", bump
	// this import to match the SDK rather than dropping the schema.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.Version),
		// Spelled literally rather than via semconv: the constant name for this
		// attribute has moved between semconv versions, and pinning to one
		// spelling breaks the build on the next bump.
		attribute.String("deployment.environment", cfg.Env),
		attribute.String("service.commit", cfg.Commit),
	))
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// ParentBased so an upstream sampling decision is honoured rather than
		// re-rolled, which would produce broken partial traces.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(tp)

	return func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("shutdown tracer provider: %w", err)
		}
		return nil
	}, nil
}

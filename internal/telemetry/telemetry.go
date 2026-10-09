// Package telemetry wires OpenTelemetry tracing.
//
// Traces go over OTLP/HTTP to a collector when one is configured; with no
// endpoint the process still installs W3C propagation and exports nothing,
// which is what a plain `go run` on a laptop wants.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

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
		// Plaintext: the collector is expected on localhost. Point this at
		// anything crossing a network boundary and this must go.
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

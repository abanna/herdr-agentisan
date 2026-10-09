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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/nerds-run/go-agents/internal/telemetry"
)

func testConfig() telemetry.Config {
	return telemetry.Config{
		ServiceName: "go-agents",
		Version:     "v1.2.3",
		Commit:      "deadbeef",
		Env:         "development",
		SampleRatio: 1,
	}
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

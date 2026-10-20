// Package telemetrytest records workflow events in memory, so a test asserts
// exactly what was emitted without a collector or a network.
package telemetrytest

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// Event is an emitted record as a test compares it: its event name, and its
// attributes by key as Go values (string, bool, ...).
type Event struct {
	Name  string
	Attrs map[string]any
}

// Recorder is an in-memory log exporter. Records reach it synchronously, so
// a test reads them as soon as the emitting call has returned.
type Recorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

// NewEvents returns Events that export into a new Recorder.
func NewEvents() (*telemetry.Events, *Recorder) {
	rec := &Recorder{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(rec)))
	return telemetry.NewEvents(lp), rec
}

// Export keeps a copy of each record: the SDK reuses the ones it passes.
func (r *Recorder) Export(_ context.Context, records []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range records {
		r.records = append(r.records, rec.Clone())
	}
	return nil
}

// Shutdown does nothing: there is nothing to release.
func (*Recorder) Shutdown(context.Context) error { return nil }

// ForceFlush does nothing: records are kept as they arrive.
func (*Recorder) ForceFlush(context.Context) error { return nil }

// Records returns a copy of every record exported so far, in order.
func (r *Recorder) Records() []sdklog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdklog.Record, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, rec.Clone())
	}
	return out
}

// Events returns every record exported so far as an Event, in order.
func (r *Recorder) Events() []Event {
	records := r.Records()
	out := make([]Event, 0, len(records))
	for _, rec := range records {
		ev := Event{Name: rec.EventName(), Attrs: map[string]any{}}
		rec.WalkAttributes(func(kv attribute.KeyValue) bool {
			ev.Attrs[string(kv.Key)] = kv.Value.AsInterface()
			return true
		})
		out = append(out, ev)
	}
	return out
}

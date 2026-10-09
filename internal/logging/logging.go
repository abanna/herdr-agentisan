// Package logging builds the zerolog logger used across every binary.
package logging

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/rs/zerolog"
)

// contextKey is unexported so no other package can collide with our key.
type contextKey struct{}

// New builds a logger at the given level. format is "json" or "console".
//
// The destination is wrapped in a scrubbing writer, so redaction applies to
// every field the logger will ever emit — including ones added later by code
// that has never heard of this package. Keeping secrets out by not logging
// them is safety by omission; this makes it enforced. See scrub.go.
func New(w io.Writer, level, format string) (zerolog.Logger, error) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		return zerolog.Nop(), fmt.Errorf("parse log level %q: %w", level, err)
	}
	// Order matters. zerolog emits JSON to ITS writer, and ConsoleWriter is a
	// renderer that consumes that JSON. So the scrubber must sit directly
	// under zerolog — receiving JSON, where key-based redaction works — and
	// hand the scrubbed JSON on to the renderer.
	//
	// Wrapping the other way (scrubber outside ConsoleWriter) was tried and is
	// wrong: the scrubber then sees ANSI-coloured text, key redaction cannot
	// apply, and even the value patterns miss because a colour reset ends in
	// "m", which kills the \b word boundary before a token.
	dest := w // already io.Writer; ConsoleWriter is assignable to it
	if format == "console" {
		dest = zerolog.ConsoleWriter{Out: w, TimeFormat: time.RFC3339}
	}
	return zerolog.New(NewScrubWriter(dest)).Level(lvl).With().Timestamp().Logger(), nil
}

// Into returns a copy of ctx carrying lg.
func Into(ctx context.Context, lg zerolog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, lg)
}

// From returns the logger stored in ctx, or a disabled logger when absent.
// It never returns nil, so callers need no nil check at every use site.
func From(ctx context.Context) zerolog.Logger {
	if lg, ok := ctx.Value(contextKey{}).(zerolog.Logger); ok {
		return lg
	}
	return zerolog.Nop()
}

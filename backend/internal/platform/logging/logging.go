// Package logging owns the backend's structured logging: zerolog setup, context-scoped loggers, and the
// HTTP request-logging middleware.
//
// One hard rule governs everything here, and it is a non-negotiable project rule rather than a style
// preference (CLAUDE.md rule 8, docs/architecture.md §2 "Cross-cutting"): secrets, tokens, password
// hashes, and raw refresh/reset tokens are never logged. In practice that means this package never logs a
// request body, never logs a full URL with its query string, and never logs the request headers that
// carry credentials. Callers adding their own fields are responsible for the same discipline.
package logging

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// Options configures New. It mirrors the relevant slice of config.Config without importing it, so this
// package stays usable from tests and tools that don't build a full Config.
type Options struct {
	// Level is a zerolog level name (trace/debug/info/warn/error/fatal/panic).
	Level string
	// Format is "json" or "console".
	Format string
	// Output defaults to os.Stdout when nil.
	Output io.Writer
}

// New builds the root logger.
func New(opts Options) (zerolog.Logger, error) {
	level, err := zerolog.ParseLevel(opts.Level)
	if err != nil {
		return zerolog.Nop(), fmt.Errorf("logging: unknown level %q: %w", opts.Level, err)
	}

	out := opts.Output
	if out == nil {
		out = os.Stdout
	}

	switch opts.Format {
	case "console":
		out = zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339}
	case "json", "":
		// zerolog writes JSON natively; nothing to wrap.
	default:
		return zerolog.Nop(), fmt.Errorf("logging: unknown format %q (want \"json\" or \"console\")", opts.Format)
	}

	return zerolog.New(out).Level(level).With().Timestamp().Logger(), nil
}

// ctxKey is the private context key under which a request-scoped logger is stored.
type ctxKey struct{}

// disabledLogger is the fallback FromContext hands back when no logger was ever attached.
var disabledLogger = zerolog.Nop()

// WithContext returns a copy of ctx carrying logger.
func WithContext(ctx context.Context, logger zerolog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, &logger)
}

// FromContext returns the logger stored in ctx.
//
// It returns a disabled logger rather than nil or a process-wide global when none is present: a missing
// logger is a wiring bug, and silently writing unattributed lines to stdout hides it. Handlers always run
// under the request-logger middleware, so a real request always has one.
//
// The result is a pointer because zerolog's level methods (Info, Warn, …) take a pointer receiver, so a
// returned value would not be callable directly.
func FromContext(ctx context.Context) *zerolog.Logger {
	if logger, ok := ctx.Value(ctxKey{}).(*zerolog.Logger); ok {
		return logger
	}
	return &disabledLogger
}

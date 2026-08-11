package daemonproc

import (
	"io"

	"github.com/rs/zerolog"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Log rotation budget. Small on purpose: this is a desktop background process, not a server, and a user
// who never looks at these files should not discover them as hundreds of megabytes one day.
const (
	logMaxSizeMB = 10
	logMaxFiles  = 3
	logMaxAgeDay = 28
)

// newLogWriter returns the daemon's rotating log sink.
//
// File-based rather than stderr, matching docs/architecture.md §4's "reused by daemon, CLI, and GUI alike"
// rule and giving the later `norite logs tail` a single place to read. It is *additional* to whatever the
// service manager captures, not a replacement: journald and launchd both still collect the process's
// stderr, and keeping our own copy is what makes the log readable identically on all three platforms
// instead of through three different tools.
func newLogWriter(path string) io.WriteCloser {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    logMaxSizeMB,
		MaxBackups: logMaxFiles,
		MaxAge:     logMaxAgeDay,
		// Compress is off: three files of at most 10MB is not worth spending CPU on, and an uncompressed
		// log is one `tail` away from being readable when someone is debugging a daemon that will not start.
		Compress: false,
	}
}

// newLogger builds the daemon's structured logger over w.
//
// Same library and same field conventions as the backend (internal/platform/logging), so one mental model
// covers both sides. Timestamps are included here rather than left to the service manager because the file
// sink has no journald to add them.
func newLogger(w io.Writer, level zerolog.Level) zerolog.Logger {
	return zerolog.New(w).Level(level).With().Timestamp().Str("component", "daemon").Logger()
}

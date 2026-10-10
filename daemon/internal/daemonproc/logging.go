// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

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
// A file rather than stderr, so `norite logs tail` has one place to read on every platform instead of
// three tools. It is *additional* to whatever the service manager captures, not a replacement: journald
// still collects the process's stderr.
func newLogWriter(path string) io.WriteCloser { return newLogWriterSized(path, logMaxSizeMB) }

// newLogWriterSized is newLogWriter rotating at a size the caller gives, so a test can drive a real
// rotation without writing the tens of megabytes the daemon's own budget would take.
func newLogWriterSized(path string, maxSizeMB int) io.WriteCloser {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    maxSizeMB,
		MaxBackups: logMaxFiles,
		MaxAge:     logMaxAgeDay,
		// Compress is off: three files of at most 10MB is not worth spending CPU on, and `norite logs tail`
		// reads into the newest backup.
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

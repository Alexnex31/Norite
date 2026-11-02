// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"github.com/pion/logging"
	"github.com/rs/zerolog"
)

// pionLogs sends Pion's errors to the server's log at error and its warnings at debug, and drops the rest.
// Pion's own default writes to stderr, past the server's logger and its format, and its debug output is
// per packet. Its warnings are debug here because the manual pass found four on every ordinary
// connection, none meaning anything: a ping before the client's candidates arrive, and three on close
// ("stream is already closed", "dtls fatal: conn is closed", "read/write on closed pipe"). At warn, a
// busy instance's log would be mostly those. Each goes through Msgf, which formats nothing when its level
// is off.
type pionLogs struct{ log zerolog.Logger }

func (f pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{log: f.log.With().Str("pion", scope).Logger()}
}

type pionLogger struct{ log zerolog.Logger }

func (pionLogger) Trace(string)          {}
func (pionLogger) Tracef(string, ...any) {}
func (pionLogger) Debug(string)          {}
func (pionLogger) Debugf(string, ...any) {}
func (pionLogger) Info(string)           {}
func (pionLogger) Infof(string, ...any)  {}

func (l pionLogger) Warn(msg string)                { l.log.Debug().Msg(msg) }
func (l pionLogger) Warnf(format string, a ...any)  { l.log.Debug().Msgf(format, a...) }
func (l pionLogger) Error(msg string)               { l.log.Error().Msg(msg) }
func (l pionLogger) Errorf(format string, a ...any) { l.log.Error().Msgf(format, a...) }

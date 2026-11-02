// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"fmt"

	"github.com/pion/logging"
	"github.com/rs/zerolog"
)

// pionLogs sends Pion's warnings and errors to the server's log, and drops the rest. Pion's own default
// writes to stderr, past the server's logger and its format; and its debug output, per packet, would be
// the loudest thing in the log.
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

func (l pionLogger) Warn(msg string)                { l.log.Warn().Msg(msg) }
func (l pionLogger) Warnf(format string, a ...any)  { l.log.Warn().Msg(fmt.Sprintf(format, a...)) }
func (l pionLogger) Error(msg string)               { l.log.Error().Msg(msg) }
func (l pionLogger) Errorf(format string, a ...any) { l.log.Error().Msg(fmt.Sprintf(format, a...)) }

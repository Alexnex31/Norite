// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/internal/configwatch"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// localSink is the part of the attach server the config watch needs.
type localSink interface {
	Local(eventType string, data json.RawMessage)
}

// watchConfig tells attached clients when the user's config.toml changes (M21).
//
// The daemon does one thing with that file: notice that it changed. It does not parse it, validate it or
// hold what it says, because every setting in it is a client's, and a daemon that interpreted the file for
// its clients would be a second reader that could disagree with the first. A client hears
// ipc.EventConfigUpdate and reads the file itself.
//
// Without the watch the daemon works as it did before it had one, and a client picks a change up when it
// next starts.
func watchConfig(ctx context.Context, sink localSink, log zerolog.Logger) {
	path, err := config.Path()
	if err != nil {
		log.Warn().Err(err).Msg("cannot locate the config file; a change to it will be noticed when a client next starts")
		return
	}
	// The path is from the environment, and a log is read in a terminal.
	log.Info().Str("path", termsafe.Text(path)).Msg("watching the config file")
	err = configwatch.Watch(ctx, path, func() {
		log.Debug().Msg("the config file changed")
		sink.Local(ipc.EventConfigUpdate, json.RawMessage(`{}`))
	})
	if err != nil {
		log.Warn().Err(err).Msg("cannot watch the config file; a change to it will be noticed when a client next starts")
	}
}

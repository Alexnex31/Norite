// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/config"
	"github.com/Alexnex31/Norite/daemon/internal/configwatch"
	"github.com/Alexnex31/Norite/daemon/internal/toggle"
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
	dir, err := config.Dir()
	if err != nil {
		// An error here names a path, which is the environment's text, and a log is read in a terminal.
		log.Warn().Str("error", termsafe.Text(err.Error())).
			Msg("cannot locate the config file; a change to it will be noticed when a client next starts")
		return
	}
	// All three: config.toml, and the file each client reads while the same-machine toggle is on. Which of
	// them anybody reads is the clients' to know, from state.json.
	files := config.FilesIn(dir)
	// The path is from the environment, and a log is read in a terminal.
	log.Info().Str("path", termsafe.Text(files.Shared)).Msg("watching the config file")
	err = configwatch.Watch(ctx, []string{files.Shared, files.TUI, files.GUI}, func() {
		log.Debug().Msg("the config file changed")
		sink.Local(ipc.EventConfigUpdate, json.RawMessage(`{}`))
	})
	if err != nil {
		log.Warn().Str("error", termsafe.Text(err.Error())).
			Msg("cannot watch the config file; a change to it will be noticed when a client next starts")
	}
}

// localRequests answers what clients ask of the daemon itself: the same-machine config toggle (M21), and
// turning the port for scripts on and off (M22).
//
// It is built before the attach server, which needs it, and told about the server afterwards, which it
// needs: a toggle that has moved is announced to every attached client like any other change to the config,
// since the file each one reads has just become another file.
type localRequests struct {
	toggle *toggle.Handler
	sink   atomic.Pointer[localSinkBox]
	// automation answers the requests about the port for scripts (M22). Set before the attach server
	// serves, by whichever branch of startup knows what the daemon is signed in to.
	automation *automationControl
}

type localSinkBox struct{ localSink }

func newLocal(stateDir string, log zerolog.Logger) *localRequests {
	l := &localRequests{}
	dir, err := config.Dir()
	if err != nil {
		// Nowhere to put a config is nowhere to split one. The requests are refused, each saying why.
		log.Warn().Str("error", termsafe.Text(err.Error())).
			Msg("cannot locate the config directory; the config toggle is unavailable")
		return l
	}
	l.toggle = &toggle.Handler{ConfigDir: dir, StateDir: stateDir, Log: log, Changed: func() {
		if box := l.sink.Load(); box != nil {
			box.Local(ipc.EventConfigUpdate, json.RawMessage(`{}`))
		}
	}}
	return l
}

func (l *localRequests) bind(sink localSink) { l.sink.Store(&localSinkBox{sink}) }

// Do answers one request under ipc.LocalPathPrefix.
func (l *localRequests) Do(ctx context.Context, req ipc.Request) ipc.Response {
	if l.automation != nil && l.automation.handles(req.Path) {
		return l.automation.Do(ctx, req)
	}
	if l.toggle == nil {
		return ipc.Response{Error: &ipc.RelayError{Code: ipc.RelayFailed,
			Message: "the daemon could not locate the config directory when it started; see its log"}}
	}
	return l.toggle.Do(ctx, req)
}

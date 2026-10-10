// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonproc

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

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
	// stop ends the daemon's run, as a signal does (M23). Nil answers no stop request.
	stop     func()
	stopOnce sync.Once
	// log is the daemon's own logger, with no subsystem: what a stop is logged to.
	log zerolog.Logger
}

// stopGrace is how long after answering a stop request the daemon begins to stop. The answer is queued
// to the asker's connection like any other, and stopping closes every connection with whatever is queued
// unsent, so without the wait the asker would more often read a closed connection than its answer. An
// asker treats both the same; this makes the answer the usual one.
const stopGrace = 200 * time.Millisecond

type localSinkBox struct{ localSink }

// newLocal takes two loggers because it answers two things: daemon is the daemon's own, for a stop, which
// is no subsystem's event, and log is the config subsystem's.
func newLocal(stateDir string, stop func(), daemon, log zerolog.Logger) *localRequests {
	l := &localRequests{stop: stop, log: daemon}
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
	if req.Path == ipc.PathStop {
		return l.stopRequested(req)
	}
	if l.automation != nil && l.automation.handles(req.Path) {
		return l.automation.Do(ctx, req)
	}
	if l.toggle == nil {
		return ipc.Response{Error: &ipc.RelayError{Code: ipc.RelayFailed,
			Message: "the daemon could not locate the config directory when it started; see its log"}}
	}
	return l.toggle.Do(ctx, req)
}

// stopRequested answers ipc.PathStop: the daemon will stop, and here is the process to wait for.
//
// Asked twice, it answers twice and stops once. Nothing is refused for being signed out or mid-request:
// stopping is the same cancellation a signal makes, and every component already finishes what it must
// before the run returns.
func (l *localRequests) stopRequested(req ipc.Request) ipc.Response {
	if req.Method != http.MethodPost {
		return ipc.Failure(ipc.RelayBadRequest, "the daemon is stopped with POST")
	}
	if l.stop == nil {
		return ipc.Failure(ipc.RelayFailed, "this daemon cannot be stopped through its socket")
	}
	body, err := json.Marshal(ipc.Stopping{PID: os.Getpid()})
	if err != nil {
		return ipc.Failure(ipc.RelayFailed, "the daemon could not encode its answer")
	}
	l.stopOnce.Do(func() {
		l.log.Info().Msg("an attached client asked the daemon to stop")
		time.AfterFunc(stopGrace, l.stop)
	})
	status := http.StatusOK
	return ipc.Response{Status: &status, Body: body}
}

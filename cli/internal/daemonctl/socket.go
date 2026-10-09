// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// stopOutcome is what asking the daemon to stop itself came to.
type stopOutcome int

const (
	// stopNotAsked: nothing answered the request. No daemon is at the socket, or the one there is from
	// before the request existed, or it is a version this command cannot attach to. The service manager's
	// stop is the only one left.
	stopNotAsked stopOutcome = iota
	// stopGone: the daemon agreed, and its process has exited.
	stopGone
	// stopPending: the daemon agreed, or closed the connection as a stopping daemon does, and its process
	// could not be seen to exit: it named none, or it is still there after stopWait.
	stopPending
)

const (
	// stopAskTimeout bounds attaching and asking. A daemon answers in milliseconds when it answers.
	stopAskTimeout = 5 * time.Second
	// stopWait bounds waiting for the process to exit. Stopping waits for a token renewal the instance has
	// already answered to be stored, and for attached clients to be told; both are bounded well under this.
	stopWait = 20 * time.Second
)

// stopCaller is the one request asking needs. *ipc.Client implements it.
type stopCaller interface {
	Do(ctx context.Context, method, path string, body any) (ipc.Result, error)
}

// socketStop asks this user's daemon to stop itself (ipc.PathStop) and waits for its process to exit.
//
// It comes before the service manager's stop because that one is not a signal everywhere: on Windows it
// ends the process where it stands, and a daemon ended between an instance answering a token renewal and
// the answer being stored presents a spent token at its next start, which ends the sign-in (M19, M23).
// A daemon started by hand, which the service manager knows nothing about, is stopped by this alone.
//
// A variable so the commands' tests can say what the socket answered without one.
var socketStop = func(ctx context.Context, version string) stopOutcome {
	askCtx, cancel := context.WithTimeout(ctx, stopAskTimeout)
	defer cancel()
	client, err := ipc.Connect(askCtx, ipc.Options{Client: "norite", Version: version})
	if err != nil {
		return stopNotAsked
	}
	defer func() { _ = client.Close() }()
	return askToStop(askCtx, client, func(pid int) bool { return waitForExit(ctx, pid, stopWait) })
}

// askToStop sends the request on c and reports what came of it. gone waits for a process to exit and
// reports whether it did.
func askToStop(ctx context.Context, c stopCaller, gone func(pid int) bool) stopOutcome {
	res, err := c.Do(ctx, http.MethodPost, ipc.PathStop, nil)
	if err != nil {
		var closed *ipc.CloseError
		// A daemon that began stopping before the answer left closes the connection instead, saying so
		// or not. The request was read, so it is stopping; which process to wait for is not known.
		if errors.Is(err, ipc.ErrClosed) || (errors.As(err, &closed) && closed.Code == ipc.CloseGoingAway) {
			return stopPending
		}
		// Declined, or not understood: a daemon older than the request relays the path and reports
		// whatever came of that.
		return stopNotAsked
	}
	// A status other than 200 is an instance's, reached through a daemon that relayed the path.
	if res.Status != http.StatusOK {
		return stopNotAsked
	}
	var out ipc.Stopping
	// The daemon agreed. A process id that cannot be waited on is still an agreement.
	if err := json.Unmarshal(res.Body, &out); err != nil || out.PID <= 1 {
		return stopPending
	}
	if gone(out.PID) {
		return stopGone
	}
	return stopPending
}

// waitForExit reports whether the process is gone within limit. It signals nothing and reads nothing of
// the process: the id came from the user's own daemon over a socket only their account opens, and the
// worst a wrong one does is make this wait its full length.
func waitForExit(ctx context.Context, pid int, limit time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return processExited(ctx, pid)
}

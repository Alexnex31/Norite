// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemonclient is how a command reaches the instance as the signed-in account: through the daemon,
// which holds the account's tokens and relays the call (ADR 0011, M20). The CLI never holds a token.
//
// Every outcome of a call ends in one of clierr's codes here, once, so no verb decides for itself what a
// refused request or a stopped daemon means:
//
//   - the instance answered 2xx: the body, decoded;
//   - it answered 4xx: clierr.RefusedError, exit 4 — except 401, which is the daemon's credential failing
//     after the relay's retry, and 429, a throttle, which are both unavailable;
//   - the daemon is not running, not signed in, or cannot reach the instance: clierr.UnavailableError, exit 3;
//   - anything else, a 5xx included: an ordinary error, exit 1.
package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/Alexnex31/Norite/cli/internal/apiclient"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Caller performs a relayed request. *ipc.Client implements it; the verbs' tests substitute a fake daemon.
type Caller interface {
	Do(ctx context.Context, method, path string, body any) (ipc.Result, error)
}

// Connect attaches to this user's daemon, for one command's requests. version is this CLI's, for the
// handshake.
//
// It does not judge the sign-in from READY. A daemon that has just started, or is waiting for a keyring to
// unlock, names no account yet and is not signed out, and telling such a user to run `norite login` would
// have them supersede a perfectly good stored sign-in. The relay decides: it answers at once when the
// daemon is signed out, and waits a bounded while when it is starting (M20 /code-review).
//
// It never starts a daemon (E1 in M20's planning): one started from a shell is not the service, and would
// hold the lock the service then fails to take.
func Connect(ctx context.Context, version string) (*ipc.Client, error) {
	c, err := ipc.Connect(ctx, ipc.Options{Client: "norite", Version: version})
	if err != nil {
		var ve *ipc.VersionError
		var ce *ipc.CloseError
		switch {
		case errors.Is(err, ipc.ErrNotRunning):
			return nil, clierr.Unavailable("the daemon is not running; start it with `norite daemon start`, " +
				"or install it first with `norite daemon install`")
		case errors.As(err, &ve):
			return nil, clierr.Unavailable("%s", ve.Error())
		case errors.As(err, &ce):
			return nil, clierr.Unavailable("the daemon refused the connection: %s", termsafe.Text(ce.Reason))
		}
		return nil, clierr.Unavailable("could not attach to the daemon: %s", termsafe.Text(err.Error()))
	}
	return c, nil
}

// Call performs one request and decodes a 2xx answer's body into out, which may be nil to ignore it.
func Call(ctx context.Context, c Caller, method, path string, body, out any) error {
	res, err := c.Do(ctx, method, path, body)
	if err != nil {
		return fromRelay(err)
	}

	switch {
	case res.Status >= 200 && res.Status < 300:
		if out == nil {
			return nil
		}
		if res.Body == nil {
			return fmt.Errorf("the instance answered HTTP %d with no body", res.Status)
		}
		if err := json.Unmarshal(res.Body, out); err != nil {
			return fmt.Errorf("the instance's answer does not decode: %w", err)
		}
		return nil

	case res.Status == http.StatusUnauthorized:
		return clierr.Unavailable("the instance refused the daemon's credential; run `norite login` again")

	case res.Status == http.StatusTooManyRequests:
		// A throttle is transient, so it is "not now" rather than "no": exit 3, which a script may retry
		// after a wait, where 4 would read as the request itself being refused.
		return clierr.Unavailable("the instance is rate-limiting this account; try again shortly")
	}

	// The instance's own words, and a stranger's server: sanitized as they are lifted out (rule 19).
	e, decoded := apiclient.ErrorFromBody(res.Status, res.Body)
	if res.Status >= 400 && res.Status < 500 {
		refused := &clierr.RefusedError{Status: res.Status}
		if decoded {
			refused.Code, refused.Message, refused.RequestID = e.Code, e.Message, e.RequestID
		}
		return refused
	}
	if decoded {
		return fmt.Errorf("the instance failed the request (HTTP %d): %s (request %s)", res.Status,
			e.Message, e.RequestID)
	}
	return fmt.Errorf("the instance failed the request (HTTP %d)", res.Status)
}

// fromRelay maps the daemon's failure to perform a request.
func fromRelay(err error) error {
	var re *ipc.RelayError
	if errors.As(err, &re) {
		switch re.Code {
		case ipc.RelayNotSignedIn, ipc.RelayUnreachable:
			return clierr.Unavailable("%s", termsafe.Text(re.Message))
		}
		return fmt.Errorf("the daemon would not relay the request: %s", termsafe.Text(re.Message))
	}
	// Said once, and saying what a script retrying it needs to know: the request was handed over, so
	// whether it reached the instance is unknown. The first wording repeated itself ("the connection to the
	// daemon ended: the connection to the daemon closed"), found by the M20 manual pass killing a daemon
	// under a running verb.
	const unknown = "; the request may or may not have reached the instance"
	var ce *ipc.CloseError
	if errors.As(err, &ce) {
		return clierr.Unavailable("the daemon closed the connection during the request (%s)%s",
			termsafe.Text(ce.Reason), unknown)
	}
	if errors.Is(err, ipc.ErrClosed) {
		return clierr.Unavailable("the daemon went away during the request — it stopped or crashed%s", unknown)
	}
	return err
}

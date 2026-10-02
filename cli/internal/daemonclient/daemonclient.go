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
//     after the relay's retry, and is unavailable;
//   - the daemon is not running, not signed in, or cannot reach the instance: clierr.UnavailableError, exit 3;
//   - anything else, a 5xx included: an ordinary error, exit 1.
package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Caller performs a relayed request. *ipc.Client implements it; the verbs' tests substitute a fake daemon.
type Caller interface {
	Do(ctx context.Context, method, path string, body any) (ipc.Result, error)
}

// Connect attaches to this user's daemon, for one command's requests, and checks it is signed in. version
// is this CLI's, for the handshake.
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
	if c.Ready().Account == nil {
		_ = c.Close()
		return nil, clierr.Unavailable("the daemon is not signed in; run `norite login`")
	}
	return c, nil
}

// instanceError is the body of an error response from the instance (openapi.yaml's Error).
type instanceError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
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

	case res.Status >= 400 && res.Status < 500:
		refused := &clierr.RefusedError{Status: res.Status}
		var e instanceError
		if res.Body != nil && json.Unmarshal(res.Body, &e) == nil {
			// The instance's own words, and a stranger's server: sanitized as they enter (rule 19).
			refused.Code = termsafe.Text(e.Code)
			refused.Message = termsafe.Text(e.Message)
			refused.RequestID = termsafe.Text(e.RequestID)
		}
		return refused
	}

	if res.Body != nil {
		var e instanceError
		if json.Unmarshal(res.Body, &e) == nil && e.Message != "" {
			return fmt.Errorf("the instance failed the request (HTTP %d): %s (request %s)", res.Status,
				termsafe.Text(e.Message), termsafe.Text(e.RequestID))
		}
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
	var ce *ipc.CloseError
	if errors.As(err, &ce) || errors.Is(err, ipc.ErrClosed) {
		return clierr.Unavailable("the connection to the daemon ended: %s", termsafe.Text(err.Error()))
	}
	return err
}

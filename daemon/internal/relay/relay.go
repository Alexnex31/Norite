// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package relay performs an attach client's REST call with the daemon's own credential (M20). The client
// names a method, a path and a body; the relay builds the request, presents the access token, and returns
// the instance's status and body. The token never leaves this process in any direction but the instance's:
// not to the client, not to the log.
//
// # Where it may reach
//
// Under /api/v1 on the signed-in instance, and nowhere else. The URL is built here, from the credential's
// instance URL with its path prefix (M19's /security-sweep finding), then /api/v1, then the client's path;
// a path that could change the host or climb out of /api/v1 is refused before any request exists.
//
// Three surfaces are refused outright, because each manages credentials rather than using one (B1 in M20's
// planning, and architecture.md §3):
//
//   - /auth/*, which starts and ends sign-ins and changes the second factor, all of it but the three API
//     token routes below;
//   - /instance/*, the operator's surface;
//   - /users/@me/sessions, which lists every device the account is signed in on and signs them out.
//
// They are the routes M11 put behind a user actor and a live session, because a credential that can make or
// unmake credentials escalates itself. Nothing in M20 needs them. Lifting a refusal later is additive, where
// withdrawing a reach scripts rely on is not, and TestEveryContractPathIsDecided makes each new route in
// the contract a decision rather than a default.
//
// # The one exception: API tokens (M22)
//
// /auth/tokens and /auth/tokens/{id} are relayed, and nothing else under /auth is. A script on the
// automation port needs a token, architecture.md has always said one is "minted from any attach client",
// and without this the only way to get one was a second sign-in made by hand. What it concedes is in
// docs/security-ledger.md: any program running as the user can now mint a durable credential through the
// socket. Such a program can already read the refresh token the daemon stores, which reaches more, and
// the instance still asks for a user actor and a live session.
//
// The exception is two exact shapes, matched on the whole path, so nothing beside them comes along.
//
// # A 401 is the session's business
//
// An access token can expire under a request, and the gateway client already answers a 4004 by telling the
// session and taking the renewed token. The relay does the same, once: session.Source is the only owner of
// the refresh token, so two clients refreshing at the same moment is one refresh, never two presentations
// of one rotating token, which reuse detection reads as theft.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/internal/session"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Credentials is what the relay needs from the session. *session.Source implements it.
type Credentials interface {
	Status() (session.Standing, session.Account)
	CurrentUnlessSignedOut(ctx context.Context) (session.Credential, error)
	Rejected(accessToken string)
}

// Options configure a Relay.
type Options struct {
	Credentials Credentials
	// HTTP performs the calls. Nil means one that follows no redirect and gives up after requestTimeout.
	HTTP *http.Client
	// Version goes in the User-Agent.
	Version string
	Log     zerolog.Logger
}

const (
	// signInWait bounds the wait for a usable token when the daemon is still signing in or renewing. A
	// signed-out daemon is not waited for at all. Longer than the session's ten-second floor between
	// refreshes, so a token refused just after a renewal waits for the next one rather than timing out.
	signInWait = 15 * time.Second
)

// Relay performs requests for attach clients. Safe for concurrent use.
type Relay struct {
	creds     Credentials
	http      *http.Client
	userAgent string
	log       zerolog.Logger
}

// New builds a Relay.
func New(opts Options) *Relay {
	client := opts.HTTP
	if client == nil {
		// The session's client, not a copy of its policy: one place decides how the daemon's credential
		// travels, so a later hardening cannot miss the relay (M20 /code-review). Its refusal of redirects
		// matters here too — Go drops Authorization on a cross-host redirect, but a same-host one would carry
		// the token to a path this relay has not checked.
		client = session.NewHTTPClient()
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	return &Relay{creds: opts.Credentials, http: client, userAgent: "norite-daemon/" + version, log: opts.Log}
}

func failure(code, msg string) ipc.Response {
	return ipc.Response{Error: &ipc.RelayError{Code: code, Message: msg}}
}

// Do performs req. Its answer's ID is left for the caller to set.
func (r *Relay) Do(ctx context.Context, req ipc.Request) ipc.Response {
	target, err := Target(req.Path)
	if err != nil {
		return failure(ipc.RelayRefused, err.Error())
	}
	// The contract carries "no body" as JSON null, which arrives here as the four bytes of it.
	if bytes.Equal(bytes.TrimSpace(req.Body), []byte("null")) {
		req.Body = nil
	}
	if req.Body != nil && !json.Valid(req.Body) {
		return failure(ipc.RelayBadRequest, "the request body is not JSON")
	}

	if standing, _ := r.creds.Status(); standing == session.SignedOut {
		return failure(ipc.RelayNotSignedIn, "the daemon is not signed in; run `norite login`")
	}
	cred, failed := r.current(ctx)
	if failed != nil {
		return *failed
	}

	status, body, err := r.send(ctx, req, target, cred)
	if err == nil && status == http.StatusUnauthorized {
		// Expired or refused under the request. The session decides what that means and renews; the retry
		// goes only to the sign-in the first attempt was made as, so a login landing in between does not
		// turn a call made as one account into the same call made as another.
		r.creds.Rejected(cred.AccessToken)
		renewed, failed := r.current(ctx)
		if failed != nil {
			return *failed
		}
		if renewed.Generation != cred.Generation || renewed.InstanceURL != cred.InstanceURL {
			return failure(ipc.RelayNotSignedIn, "the daemon's sign-in changed during the request; try it again")
		}
		status, body, err = r.send(ctx, req, target, renewed)
	}

	switch {
	case errors.Is(err, errTooLarge):
		return failure(ipc.RelayTooLarge, fmt.Sprintf("the instance's answer exceeds %d bytes", ipc.MaxResponseBody))
	case err != nil:
		if ctx.Err() != nil {
			return failure(ipc.RelayUnreachable, "the request was canceled")
		}
		r.log.Debug().Str("error", termsafe.Text(err.Error())).Msg("a relayed request could not reach the instance")
		return failure(ipc.RelayUnreachable, "could not reach the instance: "+termsafe.Text(err.Error()))
	}

	r.log.Debug().Str("method", req.Method).Str("path", termsafe.Text(target.Path)).Int("status", status).
		Msg("relayed a request")
	return ipc.Response{Status: &status, Body: body}
}

// current waits a bounded while for a usable credential.
func (r *Relay) current(ctx context.Context) (session.Credential, *ipc.Response) {
	waitCtx, cancel := context.WithTimeout(ctx, signInWait)
	defer cancel()
	cred, err := r.creds.CurrentUnlessSignedOut(waitCtx)
	if err == nil {
		return cred, nil
	}
	if ctx.Err() != nil {
		f := failure(ipc.RelayUnreachable, "the request was canceled")
		return session.Credential{}, &f
	}
	if errors.Is(err, session.ErrSignedOut) {
		f := failure(ipc.RelayNotSignedIn, "the daemon is not signed in; run `norite login`")
		return session.Credential{}, &f
	}
	f := failure(ipc.RelayUnreachable, "the daemon has no usable session from its instance yet; "+
		"it may be unreachable, and the daemon's log says why")
	return session.Credential{}, &f
}

var errTooLarge = errors.New("response too large")

// send makes one attempt. A body that is not JSON comes back as nil: a proxy in front of an instance answers
// a failure with HTML, and the status is what the client needs from it.
func (r *Relay) send(ctx context.Context, req ipc.Request, target *url.URL, cred session.Credential) (int, json.RawMessage, error) {
	u, err := Build(cred.InstanceURL, target)
	if err != nil {
		return 0, nil, err
	}

	var reader io.Reader
	if req.Body != nil {
		reader = bytes.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), reader)
	if err != nil {
		return 0, nil, err
	}
	hreq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", r.userAgent)
	if req.Body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.http.Do(hreq)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, ipc.MaxResponseBody+1))
	if err != nil {
		return 0, nil, err
	}
	if len(raw) > ipc.MaxResponseBody {
		return 0, nil, errTooLarge
	}
	if len(bytes.TrimSpace(raw)) == 0 || !json.Valid(raw) {
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, raw, nil
}

// refused are the surfaces the relay will not reach, as path prefixes under /api/v1: see the package comment.
var refused = []string{"/auth", "/instance", "/users/@me/sessions"}

// Target checks a client's path and returns it parsed: a path under /api/v1 and its query, nothing that
// could name another host or climb out, and nothing on a refused surface.
func Target(path string) (*url.URL, error) { return target(path, true) }

// ScriptTarget is Target for a request arriving on the automation port (M22): the same rules with none of
// the relay's exceptions, so every refused surface is refused whole.
//
// A script presents an API token, which the instance would refuse on the token routes anyway (they need a
// user actor); refusing here means the port never carries a request to a surface that manages credentials,
// whatever a later instance decides to allow on it. The exceptions are a parameter rather than something
// this function takes back out: an exception added to the relay for another surface must not reach the
// lower tier because nobody thought to subtract it (M22 /code-review).
func ScriptTarget(path string) (*url.URL, error) { return target(path, false) }

func target(path string, exceptions bool) (*url.URL, error) {
	switch {
	case path == "" || path[0] != '/':
		return nil, errors.New("the path must start with /")
	case strings.HasPrefix(path, "//"):
		return nil, errors.New("the path must not start with //, which names a host")
	case strings.ContainsAny(path, "\\#"):
		return nil, errors.New("the path must not contain a backslash or a fragment")
	case len(path) > 2048:
		return nil, errors.New("the path is longer than 2048 bytes")
	}

	u, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("the path does not parse: %w", err)
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return nil, errors.New("the path must be a path, not a URL")
	}
	// A path whose escaping differs from the default — an encoded slash, most usefully — would mean one
	// thing to the check below and another to the router. Nothing a verb sends needs one: ids are digits.
	if u.RawPath != "" {
		return nil, errors.New("the path must not percent-encode its separators")
	}
	for i, seg := range strings.Split(u.Path, "/") {
		switch {
		case seg == "." || seg == "..":
			return nil, errors.New("the path must not contain . or .. segments")
		case seg == "" && i > 0:
			// Empty segments too: `/users//@me/sessions` passes a prefix check, and a proxy in front of an
			// instance that merges slashes delivers it as the surface the check refuses (M20 /code-review).
			return nil, errors.New("the path must not contain an empty segment")
		}
	}

	lower := strings.ToLower(u.Path)
	// The daemon's own requests are answered before they reach here. One that did reach here is refused
	// rather than sent to the instance, which has no business hearing what a client asked of the daemon.
	if local := strings.TrimSuffix(ipc.LocalPathPrefix, "/"); lower == local || strings.HasPrefix(lower, ipc.LocalPathPrefix) {
		return nil, errors.New("the path is the daemon's own, and is not relayed to the instance")
	}
	// On the path as written, not lowered: a refusal errs wide and an exception must not.
	if exceptions && tokenRoute(u.Path) {
		return &url.URL{Path: u.Path, RawQuery: u.RawQuery}, nil
	}
	for _, prefix := range refused {
		if lower == prefix || strings.HasPrefix(lower, prefix+"/") {
			return nil, fmt.Errorf("the daemon does not relay %s: it manages credentials or the instance, "+
				"and stays off the attach socket", prefix)
		}
	}
	return &url.URL{Path: u.Path, RawQuery: u.RawQuery}, nil
}

// tokenRoute reports whether a path is one of the API token routes, the exception to the /auth
// refusal: the collection, or one token named by its id. An id is digits, so the second shape cannot be
// stretched over a sibling route.
func tokenRoute(path string) bool {
	const collection = "/auth/tokens"
	if path == collection {
		return true
	}
	id, ok := strings.CutPrefix(path, collection+"/")
	if !ok || id == "" || len(id) > 20 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Build joins a checked target onto an instance URL: its scheme and host, its path prefix, then /api/v1.
func Build(instanceURL string, target *url.URL) (*url.URL, error) {
	base, err := url.Parse(instanceURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("the stored instance URL is not usable")
	}
	return &url.URL{
		Scheme:   base.Scheme,
		Host:     base.Host,
		Path:     strings.TrimRight(base.Path, "/") + "/api/v1" + target.Path,
		RawQuery: target.RawQuery,
	}, nil
}

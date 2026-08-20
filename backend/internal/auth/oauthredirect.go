package auth

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// The loopback return URI: where this instance sends a browser once the provider has already come back.
//
// # Why this exists at all
//
// A command-line client cannot read the exchange code off a rendered page. Something has to carry it from
// the browser the person consented in to the process that will redeem it, and the answer for a native
// application is a listener on the loopback interface (RFC 8252). The client names it when the flow
// starts; the callback returns to it instead of rendering.
//
// # Why it is a separate file from oauth.go
//
// oauth.go is about what a *provider* is trusted for. This is a value the *client* supplies, and the two
// have different threat models entirely. The provider's redirect is registered out of band and never
// varies; this one arrives on an unauthenticated query string and decides where a credential is delivered.
// Treating them as the same kind of thing is how a redirect validator ends up trusting the wrong half of a
// URL.
//
// # What the checks are actually defending
//
// One thing: that the exchange code cannot be aimed anywhere except a listener on the machine the person
// is sitting at. Everything below follows from that, and the reason the list is long is that a URL has
// many ways to look like one host and resolve as another. Note that the code is *also* useless to whoever
// receives it without the flow verifier, which never leaves the client — so this validation is the second
// of two independent controls, not the only one. It is written as though it were the only one anyway.

// ErrOAuthClientRedirect is a client-supplied return URI this instance will not send a browser to.
//
// Legible for the same reason ErrOAuthFlowChallenge is: it is reachable only by a client that has been
// written wrong, never by an attacker doing something clever, so there is nothing to withhold.
var ErrOAuthClientRedirect = errors.New(
	"client_redirect_uri must be an http URL on a loopback IP literal with an explicit port, " +
		"no userinfo, no query and no fragment")

// maxClientRedirect bounds what is stored and later re-emitted in a Location header.
//
// Generous for the shape this accepts — http://127.0.0.1:65535/ plus a path — and small enough that the
// column and the header stay uninteresting. A client needing more than this is doing something the design
// does not anticipate.
const maxClientRedirect = 256

// ParseOAuthClientRedirect validates a loopback return URI and returns its canonical form.
//
// The empty string is valid and means "no redirect": the callback renders its page, which is what a
// browser gets, what the device-code flow will get, and what every client got before this existed.
//
// The returned value is url.URL's own re-serialization rather than the input. That is deliberate: it makes
// "no query, no fragment, no userinfo" a property of what is *stored*, not merely of what was inspected,
// so a later reader does not have to re-derive whether the parser and the string agreed.
func ParseOAuthClientRedirect(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxClientRedirect {
		return "", ErrOAuthClientRedirect
	}

	// Byte-level before parse-level. A CR or LF here would be a response-splitting attempt against the
	// Location header; net/http refuses to write one, but a value that also becomes a database column and
	// an error string should be refused where it arrives rather than survive on a downstream library's
	// diligence. Restricting to printable ASCII covers that and every other control byte at once, and
	// costs nothing real — a loopback URI has no business carrying anything else.
	for i := range len(raw) {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return "", ErrOAuthClientRedirect
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrOAuthClientRedirect
	}

	// Exactly http, and this one check carries three refusals rather than the one it looks like.
	//
	// Not https: a loopback listener has no certificate anyone can validate, so an https loopback redirect
	// is always a client bug and never a working flow. Not javascript:, data:, file:, or an application's
	// own norite://, which is what stops a Location header becoming a way to invoke something other than a
	// browser navigation. And not the empty scheme, which is what "//evil.example/cb" parses to — a
	// scheme-relative URI that a browser resolves against whatever origin it is already on. An explicit
	// !u.IsAbs() check for that last case was written here first and removed: IsAbs is exactly
	// `Scheme != ""`, so it could never fire ahead of this line, and a guard that cannot fire is a comment
	// claiming a defense that is really being made somewhere else.
	if u.Scheme != "http" {
		return "", ErrOAuthClientRedirect
	}

	// Userinfo is refused outright rather than ignored. "http://127.0.0.1:51763@evil.example/cb" has a
	// host of evil.example — Go parses it correctly, which is exactly why a hand-written "does the host
	// start with 127." check is the classic way to get this wrong. Removing the construct removes the
	// parse-confusion surface, rather than relying on being right about it.
	if u.User != nil {
		return "", ErrOAuthClientRedirect
	}

	// No query and no fragment on the input, which is what lets the callback *assign* its query rather
	// than merge into one. A client-supplied "?code=" beside the real one is a disagreement waiting to
	// happen between whichever parser reads the first and whichever reads the last.
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", ErrOAuthClientRedirect
	}

	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// An explicit port is required. Without one the destination depends on a default nobody stated,
		// and port 80 is not bindable unprivileged on several platforms this ships to.
		//
		// This also catches the authority-less "http:127.0.0.1:5/cb", where url.Parse puts everything in
		// Opaque and leaves Host empty. A separate u.Opaque check was written here and removed once a
		// trace showed it could never fire — SplitHostPort("") fails first.
		return "", ErrOAuthClientRedirect
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", ErrOAuthClientRedirect
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		// A *name*, not an address — and that includes "localhost". A name is resolved by the browser
		// through /etc/hosts and DNS, either of which can be made to point off-machine, so it cannot carry
		// the guarantee this whole function exists to make. RFC 8252 §8.3 says the same. An IP literal is
		// resolved by nobody.
		return "", ErrOAuthClientRedirect
	}
	if !addr.IsLoopback() {
		return "", ErrOAuthClientRedirect
	}
	if addr.Zone() != "" {
		// Load-bearing, and not obviously so: netip reports "::1%eth0" as loopback, so without this line a
		// zoned address is accepted and its zone — free-form text — rides into a Location header. Verified
		// by removing it and watching the zoned row of TestRefusedLoopbackRedirects fail. A zone on a
		// loopback address is meaningless anyway; there is only one loopback interface to name.
		return "", ErrOAuthClientRedirect
	}

	// Canonical path, so the emitted Location is byte-predictable and a test can assert on it.
	if u.Path == "" {
		u.Path = "/"
	} else if !strings.HasPrefix(u.Path, "/") {
		return "", ErrOAuthClientRedirect
	}

	return u.String(), nil
}

// oauthReturnURL puts a one-time exchange code on an already-validated loopback URI.
//
// The query is assigned, never merged: ParseOAuthClientRedirect refuses a URI that carries one, so there
// is exactly one `code` parameter here and no question of which a client's parser will read. Building it
// with url.Values rather than by concatenation is not needed for the values passed today — an exchange
// code is base64url and a fixed error vocabulary is [a-z_] — and is what keeps that true when someone
// adds a third caller.
func oauthReturnURL(redirect string, query url.Values) string {
	u, err := url.Parse(redirect)
	if err != nil {
		// Unreachable: redirect came back from ParseOAuthClientRedirect, which parsed it and returned its
		// own re-serialization. Falling back to the bare URI keeps a mistake here from becoming an open
		// redirect or a panic, and the client fails cleanly on a callback carrying no code.
		return redirect
	}
	u.RawQuery = query.Encode()
	return u.String()
}

// ---------- telling a listener why a sign-in failed ----------

// The error codes a loopback client can receive, and the whole vocabulary.
//
// Fixed, small, and owned by this package — never a provider's text and never a person's. That is the
// rule-19 decision that matters most here: a client's listener is a socket any local process can write to,
// so keeping free-form text off it entirely is strictly better than sanitizing it on the far side. There
// is deliberately no error_description; a client that wants prose writes its own from these.
const (
	oauthErrDeclined             = "access_denied"
	oauthErrEmailUnverified      = "email_unverified"
	oauthErrIdentityLinked       = "identity_linked_elsewhere"
	oauthErrAccountAlreadyLinked = "account_already_linked"
	oauthErrRegistrationClosed   = "registration_closed"
	oauthErrNoEmail              = "no_email"
	oauthErrServer               = "server_error"
)

// OAuthCallbackError is a callback failure that happened after the state was consumed, so it knows where
// the client asked to be returned to.
//
// It wraps rather than replaces, so every errors.Is in renderOAuthFailure keeps working unchanged — the
// page path is not aware this type exists.
type OAuthCallbackError struct {
	Err error
	// Code is from the vocabulary above.
	Code string
	// ClientRedirectURI is empty for a flow that named no listener, in which case this type carries
	// nothing the page path did not already have.
	ClientRedirectURI string
}

func (e *OAuthCallbackError) Error() string { return e.Err.Error() }
func (e *OAuthCallbackError) Unwrap() error { return e.Err }

// oauthErrorCodeFor maps a failure onto the vocabulary.
//
// The default is server_error rather than anything more specific, and unknown failures stay unknown: a
// code is a thing a client branches on, so inventing one per sentinel would make every new sentinel a
// silent contract change.
func oauthErrorCodeFor(err error) string {
	switch {
	case errors.Is(err, ErrOAuthProviderDeclined):
		return oauthErrDeclined
	case errors.Is(err, ErrOAuthEmailUnverified):
		return oauthErrEmailUnverified
	case errors.Is(err, ErrOAuthIdentityLinkedElsewhere):
		return oauthErrIdentityLinked
	case errors.Is(err, ErrOAuthAccountAlreadyLinked):
		return oauthErrAccountAlreadyLinked
	case errors.Is(err, ErrOAuthRegistrationClosed):
		return oauthErrRegistrationClosed
	case errors.Is(err, ErrOAuthNoEmail):
		return oauthErrNoEmail
	default:
		return oauthErrServer
	}
}

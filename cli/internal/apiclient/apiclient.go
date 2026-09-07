// Package apiclient is the CLI's transport to a Norite instance's REST API.
//
// # What is here and what is not
//
// Everything in this package is about *getting bytes to and from an instance safely*: the timeout, the
// redirect policy, the body caps, the error envelope, and the sanitization every value from a stranger's
// server passes through on the way out. What is deliberately *not* here is any knowledge of endpoints —
// no paths, no request or response shapes. Those belong to the command that calls them, so adding one
// never means touching the layer that decides what is safe.
//
// # Why it is its own package
//
// It began inside `norite login` (M7), which was the only command that spoke to an instance directly.
// M10 adds a second — `norite instance bootstrap`, which cannot go through the daemon because it runs
// before any account the daemon could hold a session for exists. Two commands cannot share an unexported
// type, and the alternative to moving it was a second copy of the redirect policy, the body caps and the
// sanitization. A second copy of a security boundary is one that drifts, and the half that drifts is the
// half nobody is looking at.
//
// # Why it is hand-written
//
// `oapi-codegen` is not wired up until M12, and a handful of endpoints do not justify pulling a generated
// client into the CLI ahead of the milestone that decides how generation works. When it is wired, the
// endpoint layer above this is what gets replaced; this package's job — caps, redirects, sanitization —
// is not something a generator provides.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Alexnex31/Norite/cli/internal/termsafe"
)

// RequestTimeout bounds a call to the instance.
//
// Generous enough for a slow link and an argon2id hash on the far side — the login and bootstrap endpoints
// deliberately cost ~100ms of CPU there — and short enough that a wrong hostname fails while someone is
// still watching.
const RequestTimeout = 30 * time.Second

// maxErrorBody caps how much of a failure response is read. The body reaches an error message, and an
// instance that answers a megabyte of HTML — a proxy error page, most likely — must not put all of it on
// someone's terminal.
const maxErrorBody = 8 << 10

// maxResponseBody caps a successful response. Nothing this client asks for is large, and the cap is what
// keeps a hostile or broken instance from being able to decide how much memory this process uses.
const maxResponseBody = 1 << 20

// maxDisplayed caps a value taken from the instance for display, in runes.
//
// The same reasoning as maxErrorBody, applied to the values that survive a request: a username is printed,
// written to the credential record, and printed again by `norite logout`. Filling a screen is how the rest
// of what a command said gets pushed out of view, which is the goal an erase-line sequence has by a
// quicker route. Generous enough that no name anyone chose is affected — an instance's own limit is 32 —
// and the cut is marked, because a name silently shortened to `ada` is a name that reads as somebody
// else's.
const maxDisplayed = 128

// Error is a structured failure from the instance.
//
// Every string on it has already been through termsafe: see errorFrom, which is where they are lifted out
// of the response. A caller may print any field of this without further thought, which is the property
// worth having — sanitizing at the boundary rather than at each of the places a value ends up.
type Error struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the instance answered %d", e.Status)
}

// Client talks to one instance.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a client for one instance's base URL.
//
// The URL is expected to have been through ParseInstanceURL already: it becomes a request target, and a
// value that also directs an action is rejected rather than sanitized (M7).
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: RequestTimeout,
			// Redirects are not followed. A 302 to another host would re-send the Authorization header, or
			// the credential body, to wherever the redirect pointed — and an instance URL that redirects
			// is a misconfiguration worth reporting rather than chasing.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// BaseURL is the instance this client talks to, as it was given.
func (c *Client) BaseURL() string { return c.baseURL }

// Do performs one request and decodes its result.
//
// bearer is sent as an Authorization header when non-empty; body is JSON-encoded when non-nil; out is
// JSON-decoded when non-nil. A non-2xx response becomes an *Error and out is left untouched.
func (c *Client) Do(ctx context.Context, method, path, bearer string, body, out any) error {
	_, err := c.DoStatus(ctx, method, path, bearer, body, out)
	return err
}

// DoStatus is Do, also reporting which 2xx the instance answered with.
//
// Two successful statuses can mean different things on one endpoint, and POST /auth/login is the first
// place that happens here: 200 carries a token pair, 202 carries a demand for a second factor. The
// contract makes that a status distinction rather than a discriminated body deliberately (ADR 0031), so a
// client that can only see "it was a 2xx" decodes the challenge into a token pair and reports two empty
// strings as a broken instance — which is exactly what this CLI did until M11a's follow-up.
//
// The status is returned rather than passed to a callback because the alternative reads worse at both call
// sites, and 0 is never returned alongside a nil error.
func (c *Client) DoStatus(ctx context.Context, method, path, bearer string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encoding the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("building the request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Unwrapped deliberately: url.Error renders the full URL, and this one carries no credential —
		// a password or a token is in the body or a header — so the host and port a person mistyped stay
		// visible, which is the whole diagnostic value of this failure.
		return 0, fmt.Errorf("could not reach %s: %w", c.baseURL, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody)); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, c.errorFrom(resp)
	}

	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("the instance's response could not be read: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// errorFrom turns a failure response into something worth printing.
//
// Every string taken from the response is sanitized as it is lifted out of it. This is the CLI's most
// direct route from a stranger's server to a user's terminal — the message is printed verbatim, prefixed
// with "norite:", by a command that ran against a URL somebody supplied — so an `ESC [ 2 K CR` in it would
// erase the line it was written on and replace a refusal with whatever the server preferred it to say.
// resp.Status is no safer than the body: the reason phrase is the server's text too.
func (c *Client) errorFrom(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Error.Message != "" {
		return &Error{
			Status:    resp.StatusCode,
			Code:      termsafe.Text(envelope.Error.Code),
			Message:   termsafe.Text(envelope.Error.Message),
			RequestID: termsafe.Text(envelope.Error.RequestID),
		}
	}

	// Not this API's error shape. Most often a proxy, a captive portal, or a URL that is not a Norite
	// instance at all — so the status is reported without pretending to have understood the body.
	return &Error{
		Status: resp.StatusCode,
		Message: fmt.Sprintf("%s answered %s, which is not a Norite API response",
			c.baseURL, termsafe.Text(resp.Status)),
	}
}

// ForDisplay makes a value from the instance safe to print, and short enough not to bury what is around it.
//
// Applied where a foreign value *enters* the program rather than where it is printed, so the value is safe
// wherever it goes afterwards — including into a file the daemon reads back later (M7).
func ForDisplay(s string) string {
	s = termsafe.Text(s)

	// Counted in runes, cut on a rune boundary: cutting bytes would split a multi-byte character and put an
	// invalid one on the terminal, which is the thing the sanitizer just finished ruling out.
	if runes := []rune(s); len(runes) > maxDisplayed {
		s = string(runes[:maxDisplayed]) + "…"
	}
	return s
}

// LooksLikeHTTP reports whether a URL will carry a credential in the clear.
func LooksLikeHTTP(instanceURL string) bool {
	return strings.HasPrefix(instanceURL, "http://")
}

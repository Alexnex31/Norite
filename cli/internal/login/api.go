package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Alexnex31/Norite/cli/internal/termsafe"
)

// The small piece of the Norite REST API `norite login` needs.
//
// Deliberately hand-written and tiny rather than generated: `oapi-codegen` is not wired up until M12, and
// two endpoints do not justify pulling a generated client into the CLI ahead of the milestone that decides
// how generation works. When it is wired, this is the first thing to replace.
//
// It is also all the CLI will ever need. From M20 every authenticated action goes through the daemon over
// the local IPC socket (ADR 0011); login is the exception only because it is the one moment a password
// exists, and it exists in the process the person typed it into.

// requestTimeout bounds a call to the instance. Generous enough for a slow link and an argon2id hash on the
// far side — the login endpoint deliberately costs ~100ms of CPU there — and short enough that a wrong
// hostname fails while someone is still watching.
const requestTimeout = 30 * time.Second

// maxErrorBody caps how much of a failure response is read. The body reaches an error message, and an
// instance that answers a megabyte of HTML — a proxy error page, most likely — must not put all of it on
// someone's terminal.
const maxErrorBody = 8 << 10

// maxDisplayed caps a value taken from the instance for display, in runes.
//
// The same reasoning as maxErrorBody, applied to the one value that survives the request: the username is
// printed, written to the record, and printed again by `norite logout`. Filling a screen is how the rest of
// what a command said gets pushed out of view, which is the goal an erase-line sequence has by a quicker
// route. Generous enough that no name anyone chose is affected — this instance's own limit is 32 — and the
// cut is marked, because a name silently shortened to `ada` is a name that reads as somebody else's.
const maxDisplayed = 128

// APIError is a structured failure from the instance.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the instance answered %d", e.Status)
}

// ErrBadCredentials is a login the instance refused.
//
// Its message is the instance's own, which is deliberately identical for an unknown account, a wrong
// password and an OAuth-only account (M4) — repeating it here rather than inventing a friendlier one keeps
// the CLI from disclosing a distinction the backend went to some trouble not to make.
var ErrBadCredentials = errors.New("that email and password did not match an account on this instance")

// client talks to one instance.
type client struct {
	baseURL string
	http    *http.Client
}

func newClient(baseURL string) *client {
	return &client{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: requestTimeout,
			// Redirects are not followed on an authenticated call. A 302 to another host would re-send the
			// Authorization header, or the credential body, to wherever the redirect pointed — and an
			// instance URL that redirects is a misconfiguration worth reporting rather than chasing.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// tokenPair is what a successful login returns.
type tokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
}

// account is the part of GET /users/@me this command shows.
//
// The email the endpoint also returns is deliberately not decoded: this command was given one to sign in
// with and has no use for the instance's copy, and a field nobody reads still reads as a dependency on the
// contract that carries it.
type account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// login exchanges a password for a token pair.
func (c *client) login(ctx context.Context, req loginRequest) (tokenPair, error) {
	var pair tokenPair
	err := c.do(ctx, http.MethodPost, "/api/v1/auth/login", "", req, &pair)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
			return tokenPair{}, ErrBadCredentials
		}
		return tokenPair{}, err
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		// A 200 with a missing half means this is not the API it claims to be — a captive portal or a
		// proxy, most likely. Better said here than as a confusing failure two steps later.
		return tokenPair{}, errors.New("the instance returned an incomplete token pair")
	}
	return pair, nil
}

// oauthExchangeRequest redeems a one-time code from the loopback callback.
type oauthExchangeRequest struct {
	Code         string `json:"code"`
	FlowVerifier string `json:"flow_verifier"`
	DeviceID     string `json:"device_id"`
	DeviceName   string `json:"device_name,omitempty"`
}

// ErrOAuthCodeRefused is the instance declining to redeem a code.
//
// One message for every reason it can decline — unknown, expired, already spent, or issued to a different
// client's flow — because the instance itself does not distinguish them, deliberately, and inventing a
// finer answer here would report a distinction it went to trouble not to make.
var ErrOAuthCodeRefused = errors.New(
	"that sign-in could not be completed; run `norite login` again")

// exchangeOAuthCode trades the one-time code for a token pair.
func (c *client) exchangeOAuthCode(ctx context.Context, req oauthExchangeRequest) (tokenPair, error) {
	var pair tokenPair
	err := c.do(ctx, http.MethodPost, "/api/v1/auth/oauth/exchange", "", req, &pair)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Status {
			case http.StatusUnauthorized:
				return tokenPair{}, ErrOAuthCodeRefused
			case http.StatusTooManyRequests:
				// Worth its own wording: a loopback sign-in is three requests against a bucket the browser
				// shares, since both come from this machine's address. "The instance answered 429" tells
				// somebody nothing they can act on.
				return tokenPair{}, errors.New(
					"this instance is rate-limiting sign-ins; wait a minute and try again")
			}
		}
		return tokenPair{}, err
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		return tokenPair{}, errors.New("the instance returned an incomplete token pair")
	}
	return pair, nil
}

// deviceCodeRequest starts a device-code sign-in.
type deviceCodeRequest struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
}

// The poll's four answers, which are the instance's vocabulary from contracts/openapi.yaml and RFC 8628
// §3.5. Sentinels rather than messages, because the loop branches on two of them and only the last two
// ever reach a person — the same bargain M8 struck with the loopback listener: the instance sends codes,
// this client writes the prose.
var (
	errDeviceAuthorizationPending = errors.New("waiting for approval")
	errDeviceSlowDown             = errors.New("polling too fast")

	// ErrDeviceCodeExpired is a code that ran out, was already redeemed, or was never issued here.
	ErrDeviceCodeExpired = errors.New("that sign-in code has expired; run `norite login` again")

	// ErrDeviceAccessDenied is somebody pressing Deny on the verification page. Distinct from an expiry
	// because it is the one answer a person acted on deliberately, and telling them it "expired" would be
	// untrue and would invite them to try again.
	ErrDeviceAccessDenied = errors.New("that sign-in was denied on the other device")
)

// startDeviceAuth asks for a pair of codes.
func (c *client) startDeviceAuth(ctx context.Context, req deviceCodeRequest) (deviceAuth, error) {
	var out deviceAuth
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/device/code", "", req, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "device_flow_unavailable" {
			// Worth its own wording: this is an operator's configuration problem, and the person running
			// the command needs to know it is not theirs and what else still works.
			return deviceAuth{}, errors.New(
				"this instance is not set up for code sign-in; sign in with a password, or from a " +
					"machine where a browser can open")
		}
		return deviceAuth{}, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return deviceAuth{}, errors.New("the instance returned an incomplete sign-in code")
	}
	return out, nil
}

// pollDeviceAuth asks once whether the authorization has been approved.
func (c *client) pollDeviceAuth(ctx context.Context, deviceCode string) (tokenPair, error) {
	var pair tokenPair
	err := c.do(ctx, http.MethodPost, "/api/v1/auth/device/token", "",
		map[string]string{"device_code": deviceCode}, &pair)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// Branching on the code and never on the message. The code is a fixed vocabulary both sides
			// publish; the message is prose that may be reworded at any time, and on an instance somebody
			// else runs.
			switch apiErr.Code {
			case "authorization_pending":
				return tokenPair{}, errDeviceAuthorizationPending
			case "slow_down":
				return tokenPair{}, errDeviceSlowDown
			case "access_denied":
				return tokenPair{}, ErrDeviceAccessDenied
			case "expired_token":
				return tokenPair{}, ErrDeviceCodeExpired
			}
			if apiErr.Status == http.StatusTooManyRequests {
				return tokenPair{}, errors.New(
					"this instance is rate-limiting sign-ins; wait a minute and try again")
			}
		}
		return tokenPair{}, err
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		return tokenPair{}, errors.New("the instance returned an incomplete token pair")
	}
	return pair, nil
}

// me identifies the account a token belongs to, for display.
func (c *client) me(ctx context.Context, accessToken string) (account, error) {
	var out account
	if err := c.do(ctx, http.MethodGet, "/api/v1/users/@me", accessToken, nil, &out); err != nil {
		return account{}, err
	}

	// Sanitized here, where it enters the program, rather than at the print sites (CLAUDE.md rule 19). This
	// instance's own backend bounds a username to 32 characters of an allow-listed set, but nothing about
	// *this* code path enforces that: `--instance` is a URL somebody handed the person running the command,
	// and it is answered by whatever is at the other end. The username is written to the record file and
	// printed by two commands, so cleaning it once at the boundary is what makes all three safe.
	out.Username = forDisplay(out.Username)

	// Sanitized but never truncated: the ID is an opaque identifier that goes into the record and is
	// compared, not read. A display cap would write a prefix plus an ellipsis to disk — neither the
	// instance's identifier nor recognizable as a cut by whatever reads it next.
	out.ID = termsafe.Text(out.ID)
	return out, nil
}

// forDisplay makes one value from the instance safe to print and to keep.
func forDisplay(s string) string {
	s = termsafe.Text(s)

	// Counted in runes, cut on a rune boundary: cutting bytes would split a multi-byte character and put an
	// invalid one on the terminal, which is the thing the sanitizer just finished ruling out.
	if runes := []rune(s); len(runes) > maxDisplayed {
		s = string(runes[:maxDisplayed]) + "…"
	}
	return s
}

// do performs one request and decodes its result.
func (c *client) do(ctx context.Context, method, path, bearer string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
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
		// the password is in the body — so the host and port a person mistyped stay visible, which is the
		// whole diagnostic value of this failure.
		return fmt.Errorf("could not reach %s: %w", c.baseURL, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody)); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.errorFrom(resp)
	}

	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return fmt.Errorf("the instance's response could not be read: %w", err)
		}
	}
	return nil
}

// errorFrom turns a failure response into something worth printing.
//
// Every string taken from the response is sanitized as it is lifted out of it. This is the CLI's most
// direct route from a stranger's server to a user's terminal — the message is printed verbatim, prefixed
// with "norite:", by a command that ran against a URL somebody supplied — so an `ESC [ 2 K CR` in it would
// erase the line it was written on and replace a refusal with whatever the server preferred it to say.
// resp.Status is no safer than the body: the reason phrase is the server's text too.
func (c *client) errorFrom(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Error.Message != "" {
		return &APIError{
			Status:    resp.StatusCode,
			Code:      termsafe.Text(envelope.Error.Code),
			Message:   termsafe.Text(envelope.Error.Message),
			RequestID: termsafe.Text(envelope.Error.RequestID),
		}
	}

	// Not this API's error shape. Most often a proxy, a captive portal, or a URL that is not a Norite
	// instance at all — so the status is reported without pretending to have understood the body.
	return &APIError{
		Status: resp.StatusCode,
		Message: fmt.Sprintf("%s answered %s, which is not a Norite API response",
			c.baseURL, termsafe.Text(resp.Status)),
	}
}

// looksLikeHTTP reports whether a URL will carry the password in the clear.
func looksLikeHTTP(instanceURL string) bool {
	return strings.HasPrefix(instanceURL, "http://")
}

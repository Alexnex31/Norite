package login

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Alexnex31/Norite/cli/internal/apiclient"
	"github.com/Alexnex31/Norite/cli/internal/termsafe"
)

// APIError is a structured failure from the instance.
//
// An alias rather than a type of its own: the transport produces these, and every caller in this package
// that matches on one wants the transport's. A distinct type here would mean converting at the boundary,
// and a conversion is a place for a field to be dropped.
type APIError = apiclient.Error

// ErrBadCredentials is a login the instance refused.
//
// Its message is the instance's own, which is deliberately identical for an unknown account, a wrong
// password and an OAuth-only account (M4) — repeating it here rather than inventing a friendlier one keeps
// the CLI from disclosing a distinction the backend went to some trouble not to make.
var ErrBadCredentials = errors.New("that email and password did not match an account on this instance")

// client is this command's endpoint layer over the shared transport.
//
// Embedding rather than wrapping, so the endpoint methods below stay methods and the transport's caps,
// redirect policy and sanitization are the only way out to the network. What this type adds is knowledge
// of *which* endpoints exist and what they carry, which is the half that belongs to a command rather than
// to the transport.
type client struct {
	*apiclient.Client
}

func newClient(baseURL string) *client {
	return &client{Client: apiclient.New(baseURL)}
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
	err := c.Do(ctx, http.MethodPost, "/api/v1/auth/login", "", req, &pair)
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
	err := c.Do(ctx, http.MethodPost, "/api/v1/auth/oauth/exchange", "", req, &pair)
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
	// errDeviceSlowDown covers both the instance asking for room and the rate limiter refusing outright.
	// They are the same instruction — wait longer — and the loop's response to them is identical.
	errDeviceSlowDown = errors.New("polling too fast")

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
	if err := c.Do(ctx, http.MethodPost, "/api/v1/auth/device/code", "", req, &out); err != nil {
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
	err := c.Do(ctx, http.MethodPost, "/api/v1/auth/device/token", "",
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
				// Backed off rather than reported, which is the difference between a pause and a lost
				// sign-in. By the time a poll is throttled the person has already opened the page on
				// their phone, typed the code and may well have approved it — giving up there throws
				// that away and sends them back for a new code. 429 is the one status that means "later",
				// and this loop already knows how to wait.
				return tokenPair{}, errDeviceSlowDown
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
	if err := c.Do(ctx, http.MethodGet, "/api/v1/users/@me", accessToken, nil, &out); err != nil {
		return account{}, err
	}

	// Sanitized here, where it enters the program, rather than at the print sites (CLAUDE.md rule 19). This
	// instance's own backend bounds a username to 32 characters of an allow-listed set, but nothing about
	// *this* code path enforces that: `--instance` is a URL somebody handed the person running the command,
	// and it is answered by whatever is at the other end. The username is written to the record file and
	// printed by two commands, so cleaning it once at the boundary is what makes all three safe.
	out.Username = apiclient.ForDisplay(out.Username)

	// Sanitized but never truncated: the ID is an opaque identifier that goes into the record and is
	// compared, not read. A display cap would write a prefix plus an ellipsis to disk — neither the
	// instance's identifier nor recognizable as a cut by whatever reads it next.
	out.ID = termsafe.Text(out.ID)
	return out, nil
}

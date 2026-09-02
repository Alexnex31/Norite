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

// twoFactorChallenge is the 202 an account with a second factor answers a correct password with.
//
// Not a credential: it authorizes nothing without a code, and the instance issued it only because the
// password was already right. It is carried back to /auth/2fa/verify unchanged.
type twoFactorChallenge struct {
	Challenge string    `json:"challenge"`
	ExpiresAt time.Time `json:"expires_at"`
}

// signIn is what a sign-in attempt resolved to: a session, or a demand for a second factor. Never both.
type signIn struct {
	Pair      tokenPair
	Challenge twoFactorChallenge
}

// owesFactor reports whether this attempt stopped one step short of a session.
func (s signIn) owesFactor() bool { return s.Challenge.Challenge != "" }

// login exchanges a password for a token pair, or for a second-factor challenge.
//
// A 429 is deliberately left to the instance's own wording here, unlike the OAuth exchange. A password
// login is one request against the auth bucket, so the instance's message is the informative one — and
// TestAnErrorFromTheInstanceCannotActOnTheTerminal pins that this path surfaces it, sanitized, because
// this is the CLI's most direct route from a stranger's server to a terminal.
func (c *client) login(ctx context.Context, req loginRequest) (signIn, error) {
	return c.resolveSignIn(ctx, "/api/v1/auth/login", req, ErrBadCredentials, nil)
}

// resolveSignIn performs a call that answers either 200 with a pair or 202 with a challenge.
//
// Shared by the password login and the OAuth exchange because both mint a session on the backend and both
// therefore owe the same factor — and because two copies of a two-status branch is where one of them
// quietly keeps treating 202 as success.
//
// refused replaces the instance's 401; tooMany replaces its 429, or is nil to pass the instance's own
// message through. The two callers differ on the second and that difference is deliberate, so it is a
// parameter rather than a shared behavior one of them silently inherited.
func (c *client) resolveSignIn(ctx context.Context, path string, req any, refused, tooMany error,
) (signIn, error) {
	// One flat struct, and deliberately not two embedded ones. A response is one shape or the other and the
	// status says which, so the unused fields stay zero — but tokenPair and twoFactorChallenge both carry
	// `expires_at`, and encoding/json drops fields that conflict at equal depth rather than reporting it.
	// Embedding would therefore have decoded neither expiry, silently, on both paths.
	var body struct {
		AccessToken  string    `json:"access_token"`
		RefreshToken string    `json:"refresh_token"`
		TokenType    string    `json:"token_type"`
		Challenge    string    `json:"challenge"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
	status, err := c.DoStatus(ctx, http.MethodPost, path, "", req, &body)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Status {
			case http.StatusUnauthorized:
				return signIn{}, refused
			case http.StatusTooManyRequests:
				if tooMany != nil {
					return signIn{}, tooMany
				}
			}
		}
		return signIn{}, err
	}

	if status == http.StatusAccepted {
		if body.Challenge == "" {
			return signIn{}, errors.New(
				"the instance asked for a second factor but sent no challenge to answer it with")
		}
		return signIn{Challenge: twoFactorChallenge{
			Challenge: body.Challenge, ExpiresAt: body.ExpiresAt,
		}}, nil
	}

	if body.AccessToken == "" || body.RefreshToken == "" {
		// A 200 with a missing half means this is not the API it claims to be — a captive portal or a
		// proxy, most likely. Better said here than as a confusing failure two steps later.
		return signIn{}, errors.New("the instance returned an incomplete token pair")
	}
	return signIn{Pair: tokenPair{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		TokenType:    body.TokenType,
		ExpiresAt:    body.ExpiresAt,
	}}, nil
}

// twoFactorVerifyRequest completes a sign-in that owed a factor.
type twoFactorVerifyRequest struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

// ErrBadFactorCode is the instance refusing a second-factor code.
//
// One message for a wrong code, an expired one, a spent one and a challenge that has run out, because the
// instance answers all four identically and inventing a finer distinction here would report one it
// deliberately does not make.
var ErrBadFactorCode = errors.New("that code was not accepted")

// errTooManySignIns is the shared wording for a rate-limited sign-in step.
var errTooManySignIns = errors.New("this instance is rate-limiting sign-ins; wait a minute and try again")

// verifyTwoFactor trades a challenge and a code for the token pair the login did not return.
func (c *client) verifyTwoFactor(ctx context.Context, req twoFactorVerifyRequest) (tokenPair, error) {
	var pair tokenPair
	err := c.Do(ctx, http.MethodPost, "/api/v1/auth/2fa/verify", "", req, &pair)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Status {
			case http.StatusUnauthorized:
				return tokenPair{}, ErrBadFactorCode
			case http.StatusTooManyRequests:
				return tokenPair{}, errTooManySignIns
			}
		}
		return tokenPair{}, err
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
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

// exchangeOAuthCode trades the one-time code for a token pair, or for a second-factor challenge.
//
// A provider proves control of a provider account. It does not prove possession of this account's second
// factor, so this call owes one exactly as a password login does — the 429 wording is worth keeping,
// because a loopback sign-in is three requests against a bucket the browser shares (both come from this
// machine's address) and "the instance answered 429" tells somebody nothing they can act on.
func (c *client) exchangeOAuthCode(ctx context.Context, req oauthExchangeRequest) (signIn, error) {
	return c.resolveSignIn(ctx, "/api/v1/auth/oauth/exchange", req, ErrOAuthCodeRefused, errTooManySignIns)
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

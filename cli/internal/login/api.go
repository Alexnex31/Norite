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
type account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
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

// me identifies the account a token belongs to, for display.
func (c *client) me(ctx context.Context, accessToken string) (account, error) {
	var out account
	if err := c.do(ctx, http.MethodGet, "/api/v1/users/@me", accessToken, nil, &out); err != nil {
		return account{}, err
	}
	return out, nil
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
			Code:      envelope.Error.Code,
			Message:   envelope.Error.Message,
			RequestID: envelope.Error.RequestID,
		}
	}

	// Not this API's error shape. Most often a proxy, a captive portal, or a URL that is not a Norite
	// instance at all — so the status is reported without pretending to have understood the body.
	return &APIError{
		Status:  resp.StatusCode,
		Message: fmt.Sprintf("%s answered %s, which is not a Norite API response", c.baseURL, resp.Status),
	}
}

// looksLikeHTTP reports whether a URL will carry the password in the clear.
func looksLikeHTTP(instanceURL string) bool {
	return strings.HasPrefix(instanceURL, "http://")
}

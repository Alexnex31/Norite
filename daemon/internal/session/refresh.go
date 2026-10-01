// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// The two requests the daemon makes with a refresh token: renewing it, and handing back one it cannot keep.
//
// Both carry a credential good for thirty days in their body, which is why everything here is written to
// say as little as possible on failure — the status code, never the reason phrase, the URL or a wrapped
// transport error (rule 8).

// NewHTTPClient builds the client the refresh and the hand-back use.
//
// Redirects are not followed, and that is the load-bearing part rather than a preference. The request body
// carries the account's refresh token and is built from a *bytes.Reader, so net/http fills in GetBody and
// replays it verbatim on a 307 or 308 — to whatever host the redirect names. A misconfigured proxy, a
// self-hoster's stray redirect rule, or a hijacked hostname would be handed a 30-day credential. The CLI's
// client refuses redirects for exactly this reason; this one was left on Go's default of following up to
// ten until M7's review.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: refreshTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// refreshTimeout bounds one refresh. Long enough for a slow link; the caller retries with backoff rather
// than waiting longer, so a hung instance costs one timeout per attempt and never a stuck daemon.
const refreshTimeout = 30 * time.Second

// handBackTimeout bounds the revocation of a token the daemon could not keep. Shorter than the refresh: the
// request is a courtesy, and failing it costs only what failing already cost before it existed.
const handBackTimeout = 10 * time.Second

// tokenPair is the instance's answer to a refresh.
type tokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// errRefused is a refresh the instance answered 401: the token is unknown, expired, revoked or replayed, and
// the instance deliberately does not say which. Distinguished from every other failure because it is the
// only one a retry cannot fix — and the only one that means somebody may have signed in since, so the store
// is worth reading again.
var errRefused = errors.New("the instance refused the stored credential")

// refreshSession exchanges a refresh token for a new pair, and reports the instance's clock as its Date
// header gave it, or the zero time when it gave none.
//
// The Date header is the first sample of the server's clock the daemon gets — the refresh at startup comes
// before any gateway HELLO — and the expiry in the pair is the server's time, not this machine's. It has a
// one-second resolution, which against a refresh margin of minutes is nothing.
func refreshSession(ctx context.Context, client *http.Client, instanceURL, refreshToken string) (
	tokenPair, time.Time, error,
) {
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return tokenPair{}, time.Time{}, fmt.Errorf("encoding the refresh request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		instanceURL+"/api/v1/auth/refresh", bytes.NewReader(body))
	if err != nil {
		return tokenPair{}, time.Time{}, fmt.Errorf("building the refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// The URL is not included: it would be, via url.Error, and the request body carries the refresh
		// token — a wrapped transport error is one library change away from rendering it.
		return tokenPair{}, time.Time{}, errors.New("could not reach the instance")
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); _ = resp.Body.Close() }()

	serverTime, _ := http.ParseTime(resp.Header.Get("Date"))

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return tokenPair{}, serverTime, errRefused
	case resp.StatusCode != http.StatusOK:
		// The status code and nothing else. Not resp.Status, which carries the reason phrase — that is the
		// server's own text, exactly like the body, and this string reaches a log file people read with
		// `cat` in a terminal.
		return tokenPair{}, serverTime, fmt.Errorf("the instance answered the refresh with HTTP %d", resp.StatusCode)
	}

	var pair tokenPair
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pair); err != nil {
		return tokenPair{}, serverTime, errors.New("the instance's response could not be read")
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" || pair.ExpiresAt.IsZero() {
		// Storing half a pair would leave the next start with a token the instance has already rotated
		// away from, and no way back except a fresh login.
		return tokenPair{}, serverTime, errors.New("the instance returned an incomplete token pair")
	}
	return pair, serverTime, nil
}

// handBackToken revokes a refresh token this daemon obtained and then could not keep.
//
// Best-effort by construction, and never fatal: a token it failed to hand back leaves exactly the situation
// that existed before this function did. Both outcomes are logged, because the person reading that log is
// the only one who can revoke it by hand.
//
// Not a full logout of anything the user is using: /auth/logout revokes the single session the presented
// token belongs to, and the token presented here is the one nobody holds.
func handBackToken(parent context.Context, log zerolog.Logger, client *http.Client,
	instanceURL, refreshToken string,
) {
	// Its own budget, detached from the caller's cancellation: a daemon shutting down is exactly when a
	// token is most likely to be left in nobody's hands, and the revocation is what stops that.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), handBackTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		log.Warn().Err(err).Msg("could not build the request to revoke the token that was dropped")
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		instanceURL+"/api/v1/auth/logout", bytes.NewReader(body))
	if err != nil {
		log.Warn().Err(err).Msg("could not build the request to revoke the token that was dropped")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// No URL and no wrapped transport error, for the reason refreshSession gives.
		log.Warn().Msg("could not reach the instance to revoke the token that was dropped; " +
			"it stays valid until it expires")
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).
			Msg("the instance did not accept the revocation of the token that was dropped")
		return
	}
	log.Info().Msg("revoked the renewed credential that the store could not keep")
}

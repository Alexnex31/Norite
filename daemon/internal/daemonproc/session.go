package daemonproc

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

	"github.com/Alexnex31/Norite/daemon/credentials"
)

// Turning the credential `norite login` stored into a live session at startup.
//
// This is the half of Milestone M7 the milestone's own criterion names: the daemon uses the stored token on
// its next launch without re-prompting. There is nothing yet to *do* with the session — the gateway
// connection arrives at M19 — so what this proves is that the credential survives a login, a restart, and
// the trip back to the instance, which is the property every later milestone builds on.
//
// # Why the refresh token is spent at startup rather than kept for later
//
// It rotates: the instance issues a new refresh token with every access token and detects reuse of the old
// one (M4). So the stored value has to be replaced the moment it is spent, and doing that at a single,
// predictable point — startup, before anything else can be attempted — is what keeps the store from
// disagreeing with the server about which token is current.

// newRefreshClient builds the HTTP client the startup refresh uses.
//
// Redirects are not followed, and that is the load-bearing part rather than a preference. The request body
// carries the account's refresh token and is built from a *bytes.Reader, so net/http fills in GetBody and
// replays it verbatim on a 307 or 308 — to whatever host the redirect names. A misconfigured proxy, a
// self-hoster's stray redirect rule, or a hijacked hostname would be handed a 30-day credential. The CLI's
// client refuses redirects for exactly this reason; this one was left on Go's default of following up to
// ten.
func newRefreshClient() *http.Client {
	return &http.Client{
		Timeout: refreshTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// refreshTimeout bounds the startup refresh. Long enough for a slow link, short enough that an instance
// that is down does not hold the daemon in startup: a daemon with no session is still a running daemon,
// and it must reach "ready" either way.
const refreshTimeout = 30 * time.Second

// session is what the daemon holds after a successful refresh.
//
// The access token stays in memory and is never written down. It expires in fifteen minutes, so persisting
// it would add a credential at rest with a lifetime shorter than the interval between the restarts it was
// meant to survive.
type session struct {
	record      credentials.Record
	accessToken string
	expiresAt   time.Time
}

// establishSession loads the stored credential and trades it for a live one.
//
// Every failure here is survivable and none of them stops the daemon. A daemon that refuses to start
// because nobody has logged in yet is a daemon that cannot be installed before its first login — and
// `norite daemon install` deliberately runs before anything else (M3). An unreachable instance is the same
// situation for a different reason: the answer is to keep running and say so, not to exit and let the
// service manager restart into the same failure.
func establishSession(ctx context.Context, log zerolog.Logger, store *credentials.Store,
	client *http.Client,
) *session {
	record, refreshToken, err := store.Load()
	switch {
	case errors.Is(err, credentials.ErrNoCredential):
		log.Info().Msg("no stored credential; run `norite login` to sign in")
		return nil
	case err != nil:
		log.Error().Err(err).Msg("the stored credential could not be read")
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	pair, err := refreshSession(ctx, client, record.InstanceURL, refreshToken)
	if err != nil {
		// Logged without the token and without the error's own body — the refresh endpoint answers a
		// deliberately uniform 401 whether the token is unknown, expired, or replayed, and repeating that
		// here is all there is to say (CLAUDE.md rule 8).
		log.Error().Err(err).
			Str("instance", record.InstanceURL).
			Str("username", record.Username).
			Msg("could not renew the stored session; run `norite login` again if this persists")
		return nil
	}

	// Written back before the session is used. The instance has already rotated the family, so the token in
	// the store is spent from this moment; a crash between here and the next start would otherwise leave
	// the daemon holding a token the server has already retired.
	//
	// ReplaceToken, not Save: the record was read before a network round trip that can take thirty seconds,
	// and a `norite login` or `norite logout` inside that window has already replaced what is on disk. Save
	// would take the stale record as the truth and undo them.
	err = store.ReplaceToken(record, pair.RefreshToken)
	switch {
	case errors.Is(err, credentials.ErrCredentialChanged), errors.Is(err, credentials.ErrNoCredential):
		// Somebody signed in or out while this was in flight, so the session just renewed is not the one
		// this machine is holding any more. Leaving their credential alone is the whole point; the token
		// obtained here is dropped with it. It stays valid at the instance until it expires, and there is
		// no way to hand it back — revoking one session is M11's primitive, and reaching for it from here
		// needs the gateway connection that arrives at M19.
		log.Info().Err(err).Msg("the stored credential changed while it was being renewed; leaving it alone")
		return nil

	case err != nil:
		// The store refused the write, so it still holds the token that was just spent. Presenting a
		// rotated token is what M4's reuse detection reads as theft, and it revokes the whole device family
		// — every other session on this installation, for a disk that was full. Clearing costs one `norite
		// login` and takes nothing else down with it.
		log.Error().Err(err).Msg("the renewed credential could not be stored")
		if clearErr := store.Clear(); clearErr != nil {
			log.Error().Err(clearErr).
				Msg("the spent credential could not be cleared either; run `norite logout` then `norite login`")
		} else {
			log.Warn().Msg("cleared the spent credential; run `norite login` to sign in again")
		}
		return nil
	}

	// The instance and username are the instance's own text, and this log is read with `cat`. They are safe
	// here because `norite login` sanitized them before storing them (cli/internal/termsafe) — not because
	// of anything this side does. zerolog escapes the ASCII controls on its way into JSON and passes C1 and
	// the bidi overrides through untouched, so the day the daemon fetches a name for itself rather than
	// reading one the CLI wrote (M19), it needs that sanitizer on its own side of the line.
	log.Info().
		Str("instance", record.InstanceURL).
		Str("username", record.Username).
		Str("device", record.DeviceName).
		Time("access_token_expires_at", pair.ExpiresAt).
		Msg("signed in with the stored credential")

	return &session{record: record, accessToken: pair.AccessToken, expiresAt: pair.ExpiresAt}
}

// tokenPair is the instance's answer to a refresh.
type tokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// refreshSession exchanges a refresh token for a new pair.
func refreshSession(ctx context.Context, client *http.Client, instanceURL, refreshToken string) (tokenPair, error) {
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return tokenPair{}, fmt.Errorf("encoding the refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		instanceURL+"/api/v1/auth/refresh", bytes.NewReader(body))
	if err != nil {
		return tokenPair{}, fmt.Errorf("building the refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// The URL is not included: it would be, via url.Error, and the request body carries the refresh
		// token — a wrapped transport error is one library change away from rendering it.
		return tokenPair{}, errors.New("could not reach the instance")
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status code and nothing else. Not resp.Status, which carries the reason phrase — that is the
		// server's own text, exactly like the body, and this string reaches a log file people read with
		// `cat` in a terminal. The CLI sanitizes the same value (cli/internal/login/api.go); this module
		// cannot reach that package and does not need to, because the number says everything useful here.
		return tokenPair{}, fmt.Errorf("the instance refused the stored credential (HTTP %d)", resp.StatusCode)
	}

	var pair tokenPair
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pair); err != nil {
		return tokenPair{}, errors.New("the instance's response could not be read")
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		// Storing half a pair would leave the next start with a token the instance has already rotated
		// away from, and no way back except a fresh login.
		return tokenPair{}, errors.New("the instance returned an incomplete token pair")
	}
	return pair, nil
}

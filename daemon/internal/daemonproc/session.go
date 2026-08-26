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

// handBackTimeout bounds the revocation of a token the daemon could not keep. Shorter than the refresh: the
// daemon is already late to "ready" by this point, and the request is a courtesy — one that failing costs
// only what failing already cost before it existed.
const handBackTimeout = 10 * time.Second

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
	// Once, not in a loop. A retry here was machinery duplicating patience the lock already has: withLock
	// waits five seconds for its own reasons, and that file's comment states them — anything approaching
	// that means another process is wedged rather than busy. So a writer that is merely finishing has
	// already been waited out by the time this returns, and further attempts would only re-wait the wedged
	// case, at three times five seconds of startup before the daemon reports ready, with an uncancellable
	// sleep between them.
	err = store.ReplaceToken(record, refreshToken, pair.RefreshToken)
	switch {
	case errors.Is(err, credentials.ErrStoreUnavailable):
		// Nothing could be read and nothing was written, so nothing is known about what is on disk — and
		// the likeliest holder of that lock is a `norite login` part-way through storing a fresh
		// credential, which is precisely what must not be cleared. Clearing here logged people out of the
		// session they had just created, because a keyring prompt can hold the lock past the wait.
		//
		// Said plainly rather than reassuringly: what is on disk is, unless somebody else replaced it, the
		// token this refresh just spent, and presenting a spent token is what gets a device family revoked.
		// There is nothing better to do about it from here — the one repair, clearing it, is the thing that
		// must not happen while another writer may be mid-write.
		log.Warn().Err(err).
			Msg("could not store the renewed credential; the stored one may now be spent — if the next " +
				"start reports a refused credential, run `norite login` again")
		return nil

	case errors.Is(err, credentials.ErrCredentialChanged), errors.Is(err, credentials.ErrNoCredential):
		// Somebody signed in or out while this was in flight, so the session just renewed is not the one
		// this machine is holding any more. Leaving their credential alone is the whole point.
		//
		// The token obtained here is therefore dropped — but not simply abandoned. Left alone it stays
		// valid at the instance for the full refresh TTL with nobody holding it, which is a live
		// credential in nobody's hands, and the whole subject of M11 is not leaving those lying around.
		// So it is handed back first.
		//
		// To the instance the *local* record names, not whatever is on disk now: a `norite login` that
		// collided here may have pointed the store at an entirely different instance, and this token
		// belongs to the one it was issued by.
		//
		// This comment used to say handing it back was impossible — that revoking one session was M11's
		// primitive and reaching for it needed M19's gateway connection. Both halves were wrong.
		// POST /auth/logout has revoked exactly one session by presenting its refresh token since M4, and
		// the client to call it with is the one that just performed the refresh.
		log.Info().Err(err).Msg("the stored credential changed while it was being renewed; leaving it alone")
		handBackToken(ctx, log, client, record.InstanceURL, pair.RefreshToken)
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
		// And hand back the token that was just obtained, for the same reason as the branch above and with
		// a stronger claim: after Clear there is definitively no local holder, where a colliding login at
		// least leaves somebody signed in. This one is the plain orphan — a thirty-day credential nothing
		// will ever present, created by a disk that was full.
		handBackToken(ctx, log, client, record.InstanceURL, pair.RefreshToken)
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

// handBackToken revokes a refresh token this daemon obtained and then could not keep.
//
// Best-effort by construction, and never fatal. The daemon is starting; a token it failed to hand back
// leaves exactly the situation that existed before this function did, and refusing to start over it would
// turn a tidy-up into an outage. Both outcomes are logged, because the person reading that log is the only
// one who can revoke it by hand.
//
// Not a full logout of anything the user is using: /auth/logout revokes the single session the presented
// token belongs to, and the token presented here is the one nobody holds.
func handBackToken(parent context.Context, log zerolog.Logger, client *http.Client,
	instanceURL, refreshToken string,
) {
	// Its own budget, not the refresh's leftovers. The caller's context is capped at refreshTimeout and has
	// already paid for a network round trip and a credential-store write that can wait on a lock — on a
	// slow link there may be milliseconds left, and the request would fail before it was sent, indistinguish-
	// ably from an unreachable instance. That is exactly the case this function exists for.
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
		// No URL and no wrapped transport error, for the reason refreshSession gives: the request body is
		// a refresh token and a url.Error is one library change away from rendering it.
		log.Warn().Msg("could not reach the instance to revoke the token that was dropped; " +
			"it stays valid until it expires")
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); _ = resp.Body.Close() }()

	// The status code and nothing else — not resp.Status, which carries the server's own reason phrase
	// into a log file people read in a terminal.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).
			Msg("the instance did not accept the revocation of the token that was dropped")
		return
	}
	log.Info().Msg("revoked the renewed credential that the login superseded")
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

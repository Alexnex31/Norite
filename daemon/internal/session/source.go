// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session keeps the daemon signed in: it reads the credential `norite login` stored, trades it for
// an access token, and keeps that token live for as long as the daemon runs.
//
// M7 spent the refresh token once, at startup, and that was enough while nothing used the access token. From
// M19 the daemon holds a gateway connection for hours or weeks, and two things make once insufficient: every
// reconnect authenticates afresh with an access token that lives fifteen minutes, and a refresh token unused
// for thirty days expires (auth.RefreshTokenTTL), so a daemon connected for a month would come back signed
// out. So a Source refreshes ahead of expiry, for as long as it runs, and M20's relay spends what it keeps.
//
// # One goroutine owns the credential
//
// Run is the only code that loads, refreshes or writes back. Everything else asks: Current for a token,
// Rejected when the gateway refused one, Revoked when the gateway says the sign-in ended, Reload when the
// store may have changed. A refresh token is spent the moment it is presented, and two goroutines presenting
// the same one is exactly what the instance's reuse detection reads as theft (M4) — so there is one presenter.
//
// # What the store holds, and what this process holds
//
// These are two values, not one, and conflating them is the bug this package is shaped around. After a
// refresh the daemon holds the new token; whether the store does depends on the write-back, which can fail
// because a keyring hesitated. ReplaceToken writes only if the store still holds the token named as spent —
// so the daemon must name the token the *store* holds, not the one it last presented, or a single failed
// write turns every later one into ErrCredentialChanged, which reads as "somebody logged in".
package session

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/internal/backoff"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Credential is what a caller of Current gets: who is signed in where, and a token to prove it.
type Credential struct {
	InstanceURL string
	UserID      string
	Username    string
	DeviceName  string

	// AccessToken is never logged and never written down (rule 8). It lives fifteen minutes, shorter than
	// the interval between the restarts persisting it would let it survive.
	AccessToken string
	// ExpiresAt is the instance's time, not this machine's.
	ExpiresAt time.Time

	// Generation changes whenever the daemon adopts a credential from the store — a new login, or the first
	// one. Anything derived from the previous sign-in (a gateway session, the state built from its events)
	// belongs to a generation and is discarded when it changes, even when the account is the same: a new
	// login supersedes the device's previous sign-in at the instance (M18).
	Generation uint64
}

// Options configures a Source.
type Options struct {
	Store *credentials.Store
	// HTTP is the client the refresh and hand-back use. Nil means NewHTTPClient.
	HTTP *http.Client
	Log  zerolog.Logger
	// Clock is this machine's clock. Nil means the real one; tests skew and advance it.
	Clock Clock
	// RetryMin and RetryMax bound the backoff between failed attempts. Zero means the defaults.
	RetryMin, RetryMax time.Duration
}

const (
	defaultRetryMin = time.Second
	defaultRetryMax = 5 * time.Minute

	// minRefreshGap is the least time between two refreshes, whatever asks for them. It is what stops a
	// clock this estimate has wrong, or a gateway refusing every token, from becoming a loop that rotates
	// the account's refresh token as fast as the network allows.
	minRefreshGap = 10 * time.Second

	// expirySlack is how close to expiry a token may be and still be handed out. A token used for IDENTIFY
	// has a round trip ahead of it, and the server's clock is an estimate.
	expirySlack = 30 * time.Second
)

type phase int

const (
	phaseStarting  phase = iota // no usable token yet: loading, renewing, or between sign-ins
	phaseSignedOut              // nothing to sign in with until the store changes
	phaseLive
)

// Source keeps one account signed in. See the package comment.
type Source struct {
	store    *credentials.Store
	http     *http.Client
	log      zerolog.Logger
	clock    Clock
	server   *serverClock
	retryMin time.Duration
	retryMax time.Duration

	// Requests to Run. Buffered by one and sent without blocking, so asking twice is asking once.
	refreshNow chan struct{}
	reload     chan struct{}
	revoked    chan struct{}

	mu        sync.Mutex
	changed   chan struct{} // closed and replaced whenever what Current can answer changes
	phase     phase
	cred      Credential
	stale     bool      // the gateway refused the current access token
	refreshAt time.Time // the instance's time
	// ended is closed when the sign-in of generation liveGen is over: signed out, refused, or replaced.
	// See Ended.
	ended       chan struct{}
	endedClosed bool
	liveGen     uint64

	// Owned by Run alone.
	record      credentials.Record
	current     string // the refresh token this process holds
	stored      string // the refresh token the store holds, as far as this process knows
	dead        string // a refresh token the instance refused, so reading it back is not a sign-in
	have        bool   // current is live, as far as anyone has told us
	gen         uint64
	lastRefresh time.Time // this machine's clock
}

// New builds a Source. Nothing happens until Run.
func New(opts Options) *Source {
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	client := opts.HTTP
	if client == nil {
		client = NewHTTPClient()
	}
	s := &Source{
		store:      opts.Store,
		http:       client,
		log:        opts.Log,
		clock:      clock,
		server:     &serverClock{local: clock},
		retryMin:   opts.RetryMin,
		retryMax:   opts.RetryMax,
		refreshNow: make(chan struct{}, 1),
		reload:     make(chan struct{}, 1),
		revoked:    make(chan struct{}, 1),
		changed:    make(chan struct{}),
		ended:      closedChan, endedClosed: true,
	}
	if s.retryMin <= 0 {
		s.retryMin = defaultRetryMin
	}
	if s.retryMax <= 0 {
		s.retryMax = defaultRetryMax
	}
	return s
}

// Current returns a usable credential, waiting until there is one.
//
// "Usable" is judged on the instance's clock (serverClock): not refused, and not within expirySlack of
// expiring. Past the point a refresh is due it still answers with the token it has, and asks Run to refresh;
// it waits only when the token in hand is no good at all. A daemon that is signed out waits here until the
// store changes, which is what a gateway client blocked on it should do.
func (s *Source) Current(ctx context.Context) (Credential, error) {
	for {
		s.mu.Lock()
		if s.phase == phaseLive {
			now := s.server.now()
			if !s.stale && now.Before(s.cred.ExpiresAt.Add(-expirySlack)) {
				if !now.Before(s.refreshAt) {
					nudge(s.refreshNow)
				}
				c := s.cred
				s.mu.Unlock()
				return c, nil
			}
			nudge(s.refreshNow)
		}
		changed := s.changed
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		case <-changed:
		}
	}
}

// Rejected reports that the instance refused accessToken — the gateway's 4004. If it is still the current
// token, Current stops handing it out and Run refreshes; a refusal of a token already replaced is stale news.
func (s *Source) Rejected(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == phaseLive && s.cred.AccessToken == accessToken {
		s.stale = true
		nudge(s.refreshNow)
	}
}

// Revoked reports that the instance ended this sign-in — the gateway's 4011.
//
// The ordinary cause on a running daemon is `norite login` on this machine, which supersedes the device's
// previous sign-in (M18) and then stores its own credential. So the store is read first: a credential
// somebody else wrote is adopted, and one nobody changed is checked by presenting it, which the instance
// refuses if the sign-in really is over.
func (s *Source) Revoked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale = true
	nudge(s.revoked)
}

// Reload asks Run to read the store again, because something may have changed it. A store that turns out
// unchanged costs nothing further; one that was cleared ends the session (a logout); one holding a different
// credential replaces it (a login).
func (s *Source) Reload() { nudge(s.reload) }

// ObserveServerTime records the instance's clock — HELLO's server_time, sampled before the token is checked.
func (s *Source) ObserveServerTime(t time.Time) { s.server.observe(t) }

// Ended returns a channel that is closed once the sign-in of the given generation is over — signed out,
// refused, or replaced by another — and already closed if it is over now.
//
// It is how the gateway connection learns that the account it is streaming is no longer signed in. A
// logout normally closes the connection from the instance's side too, through its hand-back, but the
// hand-back is best-effort, and a connection that outlived its sign-in would go on filling the daemon's
// state with an account nobody is signed in as (M19 /code-review).
func (s *Source) Ended(generation uint64) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.liveGen || s.endedClosed {
		return closedChan
	}
	return s.ended
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// dropSignIn records that the token this process holds is no longer a live sign-in.
func (s *Source) dropSignIn() {
	s.have = false
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.endedClosed {
		close(s.ended)
		s.endedClosed = true
	}
}

func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run keeps the account signed in until ctx is done.
//
// It never fails. No credential, an unreadable store, an unreachable instance and a refused token are all
// states a running daemon waits out — refusing to run would mean the daemon cannot be installed before its
// first login, and `norite daemon install` deliberately runs first (M3).
func (s *Source) Run(ctx context.Context) {
	defer s.settleStore()
	refused := false
	for {
		if !s.signIn(ctx, refused) {
			return
		}
		refused = s.keepLive(ctx)
		if ctx.Err() != nil {
			return
		}
	}
}

// settleStore makes one last attempt, as the daemon stops, to store the token this process holds when the
// store holds an older one — left there by a write that failed mid-session. Without it a restart would
// present the spent token, and reuse detection would revoke this device's sign-in (M19 /code-review).
func (s *Source) settleStore() {
	if !s.have || s.current == s.stored {
		return
	}
	if err := s.store.ReplaceToken(s.record, s.stored, s.current); err != nil {
		s.log.Error().Err(err).Msg("could not store the renewed credential before stopping; the next start " +
			"may find it spent and need `norite login`")
		return
	}
	s.stored = s.current
	s.log.Info().Msg("stored the renewed credential before stopping")
}

// signIn reads the store until it holds a credential this process may present. It reports false only when
// ctx is done.
//
// refused says the token this process held was just refused, so a store nobody has changed since holds
// nothing worth presenting.
func (s *Source) signIn(ctx context.Context, refused bool) bool {
	retry := &backoff.Backoff{Min: s.retryMin, Max: s.retryMax}
	for {
		record, token, err := s.store.Load()
		switch {
		case errors.Is(err, credentials.ErrNoCredential):
			// A logout, or nobody has signed in yet. A token this process still holds is the one live
			// credential for this device and nobody else will ever present it, so it goes back to the
			// instance that issued it rather than staying valid for thirty days in nobody's hands.
			if s.have {
				handBackToken(ctx, s.log, s.http, s.record.InstanceURL, s.current)
				s.dropSignIn()
			}
			s.signOut(zerolog.InfoLevel, "no stored credential; run `norite login` to sign in")
			if !s.waitForStore(ctx, nil) {
				return false
			}
			continue

		case err != nil:
			// The case M7 deferred and M19 closes: a systemd user unit starting before the session keyring
			// unlocks reads a record naming "keyring" and fails to read the secret. Tried once, that left
			// the daemon without a session for its whole life. So it is tried again, until it works.
			delay := retry.Next()
			s.log.Error().Err(err).Dur("retry_in", delay).
				Msg("the stored credential could not be read; trying again")
			if !s.waitForStore(ctx, s.clock.After(delay)) {
				return false
			}
			continue
		}

		// Whether the store still holds what this process last knew it to — asked without regard to whether
		// that token is still live, because after a refusal it is not, and that is exactly when it matters.
		known := s.stored != "" && token == s.stored && sameSignIn(record, s.record)
		switch {
		case token == s.dead || (refused && known):
			// What the instance just refused, or what survived a clear that failed, read back. Nothing here
			// will sign in until somebody does.
			msg := "the stored credential is spent and could not be replaced; run `norite login` again"
			if refused {
				msg = "the instance refused the stored credential; run `norite login` again"
			}
			s.dead = token
			s.dropSignIn()
			s.signOut(zerolog.WarnLevel, msg)
			if !s.waitForStore(ctx, nil) {
				return false
			}
			continue

		case s.have && known:
			return true
		}

		// Somebody else wrote this credential — a login, or this is the first read. What this process held
		// before belongs to a sign-in that is over: superseded at the instance if the login was to the same
		// one, and live with nobody holding it if the login went elsewhere. Handing it back is right in the
		// second case and harmless in the first.
		if s.have {
			handBackToken(ctx, s.log, s.http, s.record.InstanceURL, s.current)
		}
		if s.have {
			s.dropSignIn()
		}
		s.record, s.current, s.stored, s.have = record, token, token, true
		s.gen++
		// A credential somebody just stored is not the loop minRefreshGap guards against, and a person who
		// has just run `norite login` should not wait it out.
		s.lastRefresh = time.Time{}
		s.set(func() {
			s.phase, s.cred, s.stale = phaseStarting, Credential{}, false
			s.ended, s.endedClosed, s.liveGen = make(chan struct{}), false, s.gen
		})
		return true
	}
}

// keepLive refreshes now and again before each expiry, until the session ends. It reports whether it ended
// because the instance refused the token.
func (s *Source) keepLive(ctx context.Context) (refused bool) {
	retry := &backoff.Backoff{Min: s.retryMin, Max: s.retryMax}
	for {
		if !s.throttle(ctx) {
			return false
		}

		// Detached from ctx, and finished even when the daemon is stopping. A refresh the instance has
		// answered has already spent the token presented; canceled before its write-back, the store would
		// keep the spent one, and the next start would present it — which reuse detection reads as theft and
		// answers by revoking this device's sign-in. refreshSession bounds it at refreshTimeout either way.
		//
		// The server's clock is sampled as of the request's start, not its end. The instance stamped its Date
		// somewhere in between, so this errs toward its clock being later than it is — toward refreshing
		// early, never toward handing out a token the instance already considers expired.
		started := s.clock.Now()
		pair, serverTime, err := refreshSession(context.WithoutCancel(ctx), s.http, s.record.InstanceURL, s.current)
		s.server.observeAt(serverTime, started)
		switch {
		case errors.Is(err, errRefused):
			// The uniform 401: unknown, expired, revoked or replayed. Possibly a login on this machine
			// superseded this sign-in a moment ago, so the store is read before anything is concluded.
			s.dead = s.current
			s.dropSignIn()
			s.set(func() { s.phase, s.cred = phaseStarting, Credential{} })
			s.log.Info().Msg("the instance refused the session's refresh token; reading the store again")
			return true

		case err != nil:
			// Unreachable, a 5xx, or an answer that made no sense. The token in hand is still the current
			// one — nothing was spent that the instance acknowledged — so keep it and try again later.
			// Current goes on handing out the access token until it expires.
			delay := retry.Next()
			s.log.Error().Err(err).
				Str("instance", termsafe.Text(s.record.InstanceURL)).
				Dur("retry_in", delay).
				Msg("could not renew the session; trying again")
			if !s.waitLive(ctx, s.clock.After(delay)) {
				return false
			}
			continue
		}
		retry.Reset()
		// Only a refresh that succeeded counts toward the floor: it is rotation the floor exists to bound, and
		// a failed attempt rotated nothing — its spacing is the backoff's business.
		s.lastRefresh = started

		if !s.writeBack(ctx, pair) {
			return false
		}

		first := false
		s.set(func() {
			first = s.phase != phaseLive
			now := s.server.now()
			s.phase, s.stale = phaseLive, false
			s.cred = Credential{
				InstanceURL: s.record.InstanceURL,
				UserID:      s.record.UserID,
				Username:    s.record.Username,
				DeviceName:  s.record.DeviceName,
				AccessToken: pair.AccessToken,
				ExpiresAt:   pair.ExpiresAt,
				Generation:  s.gen,
			}
			s.refreshAt = refreshDue(pair.ExpiresAt, now)
		})

		if first {
			// The record's text is the instance's, read back out of a file a person can edit, so it is foreign
			// again (rule 19) — the CLI sanitized it on the way in, and this side no longer relies on that.
			s.log.Info().
				Str("instance", termsafe.Text(s.record.InstanceURL)).
				Str("username", termsafe.Text(s.record.Username)).
				Str("device", termsafe.Text(s.record.DeviceName)).
				Time("access_token_expires_at", pair.ExpiresAt).
				Msg("signed in with the stored credential")
		} else {
			s.log.Debug().Time("access_token_expires_at", pair.ExpiresAt).Msg("renewed the session")
		}

		if !s.waitUntilDue(ctx) {
			return false
		}
	}
}

// writeBack stores the renewed refresh token. It reports false when the session cannot continue and the
// store has to be read again.
func (s *Source) writeBack(ctx context.Context, pair tokenPair) bool {
	err := s.store.ReplaceToken(s.record, s.stored, pair.RefreshToken)
	switch {
	case err == nil:
		s.current, s.stored = pair.RefreshToken, pair.RefreshToken
		return true

	case errors.Is(err, credentials.ErrCredentialChanged), errors.Is(err, credentials.ErrNoCredential):
		// Somebody signed in or out while this was in flight, so the session just renewed is not the one
		// this machine holds any more. Their credential is left alone, and the token obtained here — live,
		// and held by nobody else — goes back to the instance that issued it, not to whatever the store
		// names now.
		s.log.Info().Err(err).Msg("the stored credential changed while it was being renewed; leaving it alone")
		handBackToken(ctx, s.log, s.http, s.record.InstanceURL, pair.RefreshToken)
		s.dropSignIn()
		s.set(func() { s.phase, s.cred = phaseStarting, Credential{} })
		return false

	default:
		// Unreadable (ErrStoreUnavailable) or refused. M7 handed the renewed token back when the store could
		// not be read, and cleared the store when a write was refused, because the spent token it still holds
		// is what reuse detection reads as theft at the next start. Both were right for a daemon starting and
		// are wrong for one running: each signed it out over a keyring that hesitated once (M19, and
		// /code-review for the refused write).
		//
		// Kept instead, and safe either way. If the lock's holder is a login on this machine, it superseded
		// this sign-in at the instance, so this token is about to be refused and the store read again. If
		// not, this process holds the only live token, and the next refresh writes again — naming the token
		// the store still holds as the one spent, which is what makes that write possible. What a restart
		// would present is settled at shutdown: Run tries the write one last time, and a failure there costs
		// what clearing would have, one `norite login`.
		s.current = pair.RefreshToken
		s.log.Warn().Err(err).
			Msg("could not store the renewed credential; keeping the session and storing it on the next renewal")
		return true
	}
}

// waitUntilDue waits for the next refresh. It reports false when the session has to be re-established from
// the store, or ctx is done.
func (s *Source) waitUntilDue(ctx context.Context) bool {
	for {
		s.mu.Lock()
		due := s.refreshAt.Sub(s.server.now())
		stale := s.stale
		s.mu.Unlock()
		if due <= 0 || stale {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-s.clock.After(due):
			// Recomputed rather than trusted: a HELLO since may have moved the estimate of the server's clock.
		case <-s.refreshNow:
			// Not obeyed blindly: the loop re-checks. A nudge is buffered, so one sent while the last refresh
			// was due is still waiting after that refresh completes, and obeying it would rotate the token a
			// second time for nothing.
		case <-s.reload:
			if s.storeChanged() {
				return false
			}
		case <-s.revoked:
			if s.storeChanged() {
				return false
			}
			return true // verified by refreshing: refused if the sign-in really is over
		}
	}
}

// waitLive waits out a backoff while a session is live. It reports false when the session has to be
// re-established, or ctx is done.
func (s *Source) waitLive(ctx context.Context, after <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-after:
		return true
	// Not refreshNow: a refresh is already failing, and somebody asking for a token does not make the next
	// attempt likelier to succeed. Obeying it let every reconnect skip the backoff (M19 /code-review).
	case <-s.reload:
		return !s.storeChanged()
	case <-s.revoked:
		return !s.storeChanged()
	}
}

// waitForStore waits for a reason to read the store again: after, if not nil, or a request. It reports false
// only when ctx is done.
func (s *Source) waitForStore(ctx context.Context, after <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-after:
	case <-s.reload:
	case <-s.revoked:
	}
	return true
}

// throttle holds a refresh back until minRefreshGap has passed since the last one.
func (s *Source) throttle(ctx context.Context) bool {
	if s.lastRefresh.IsZero() {
		return true
	}
	wait := minRefreshGap - s.clock.Now().Sub(s.lastRefresh)
	if wait <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.clock.After(wait):
		return true
	}
}

// storeChanged reads the store and reports whether it no longer holds this process's sign-in. An unreadable
// store is not evidence of a change — the same judgement writeBack makes.
func (s *Source) storeChanged() bool {
	record, token, err := s.store.Load()
	switch {
	case errors.Is(err, credentials.ErrNoCredential):
		return true
	case err != nil:
		return false
	}
	return token != s.stored || !sameSignIn(record, s.record)
}

func (s *Source) signOut(level zerolog.Level, msg string) {
	already := false
	s.set(func() {
		already = s.phase == phaseSignedOut
		s.phase, s.cred, s.stale = phaseSignedOut, Credential{}, false
	})
	if !already {
		s.log.WithLevel(level).Msg(msg)
	}
}

// set changes what Current can answer, and wakes everybody waiting in it.
func (s *Source) set(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
	close(s.changed)
	s.changed = make(chan struct{})
}

// sameSignIn reports whether two records describe one sign-in: the same account, on the same instance, from
// the same device.
func sameSignIn(a, b credentials.Record) bool {
	return a.InstanceURL == b.InstanceURL && a.UserID == b.UserID && a.DeviceID == b.DeviceID
}

// refreshDue picks when to renew a token expiring at expiresAt, now being the instance's time: a quarter of
// its remaining life before expiry — 3¾ minutes of a fifteen-minute token, which a slow network and a
// backoff both fit inside — and never sooner than minRefreshGap from now.
func refreshDue(expiresAt, now time.Time) time.Time {
	due := expiresAt.Add(-expiresAt.Sub(now) / 4)
	if floor := now.Add(minRefreshGap); due.Before(floor) {
		return floor
	}
	return due
}

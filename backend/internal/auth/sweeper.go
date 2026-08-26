package auth

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
)

// Expiring the short-lived rows this package creates.
//
// # Why this exists here rather than in a later milestone
//
// Four comments in this package used to say expired rows were pruned by "M11's cleanup job". M11 is the
// session-revocation primitive, and no milestone in the roadmap ever swept anything — so password reset
// tokens, OAuth states and OAuth exchange codes accumulated for the life of the instance, and the code
// pointed at a milestone that was never going to fix it.
//
// Two of those three tables are written by *unauthenticated* endpoints (a reset request, an authorization
// start), which is what makes it worth closing now rather than later: growth is bounded by nothing but the
// rate limiter, and the rows are dead the moment they expire.
//
// # Why a ticker and not an opportunistic delete on the request path
//
// Sweeping inside StartOAuth or RequestPasswordReset would tie cleanup to traffic — an instance that goes
// quiet keeps its garbage forever, and a busy one pays for a DELETE on a user-facing path. A ticker does
// the same work in the same statements, off the request path, and is one place to add the next table to.

// SweepInterval is how often expired rows are removed.
//
// Ten minutes against TTLs of two minutes (exchange codes) to an hour (reset tokens): far more often than
// needed to keep the tables small, and rare enough that the work is invisible. Nothing depends on the
// timing — every one of these rows is already refused by its query's WHERE clause the moment it expires, so
// this reclaims space rather than enforcing anything.
const SweepInterval = 10 * time.Minute

// SweepResult counts what one pass removed.
type SweepResult struct {
	ResetTokens   int64
	OAuthStates   int64
	ExchangeCodes int64
	DeviceCodes   int64
	// Invites is the one sweep here that removes something a person created on purpose, rather than
	// garbage a protocol left behind. Still right — an expired invite is refused by RedeemInstanceInvite
	// regardless, so the row affects nothing but an administrator's list — and worth naming separately so
	// a count that looks surprising can be traced to the right table.
	Invites int64
	// VerificationTokens is M10's other TTL table. Unlike the invites above, these are ordinary garbage:
	// an unfollowed verification link is dead the moment it expires.
	VerificationTokens int64
	// Sessions is the biggest of these by a wide margin, and the one that was missing longest — from M4
	// until M11. Every refresh inserts a row and revokes its predecessor, so this table grows with traffic
	// rather than with sign-ins: about ninety-six rows a day per active device, none of which anything
	// deleted. Expired only; a revoked-but-unexpired row is still what tells replay apart from an unknown
	// token, which is what DeleteExpiredSessions' comment explains.
	Sessions int64
}

// Total is how many rows the pass removed altogether.
func (r SweepResult) Total() int64 {
	return r.ResetTokens + r.OAuthStates + r.ExchangeCodes + r.DeviceCodes + r.Invites +
		r.VerificationTokens + r.Sessions
}

// SweepExpired removes every expired row this package owns.
//
// Not transactional, deliberately: independent deletes with nothing to be consistent *between*, and
// wrapping them would hold one connection for all of them against a pool that is small on purpose
// (§15.3). A failure part-way leaves the rest for the next pass, ten minutes later.
func (s *Service) SweepExpired(ctx context.Context) (SweepResult, error) {
	var out SweepResult
	var err error

	if out.ResetTokens, err = s.queries.DeleteExpiredPasswordResetTokens(ctx); err != nil {
		return out, fmt.Errorf("sweeping expired reset tokens: %w", err)
	}
	if out.OAuthStates, err = s.queries.DeleteExpiredOAuthStates(ctx); err != nil {
		return out, fmt.Errorf("sweeping expired oauth states: %w", err)
	}
	if out.ExchangeCodes, err = s.queries.DeleteExpiredOAuthExchangeCodes(ctx); err != nil {
		return out, fmt.Errorf("sweeping expired oauth exchange codes: %w", err)
	}
	if out.VerificationTokens, err = s.queries.DeleteExpiredEmailVerificationTokens(ctx); err != nil {
		return out, fmt.Errorf("sweeping expired email verification tokens: %w", err)
	}
	if out.Invites, err = s.queries.DeleteExpiredInstanceInvites(ctx); err != nil {
		return out, fmt.Errorf("sweeping expired instance invites: %w", err)
	}
	if out.DeviceCodes, err = s.queries.DeleteExpiredDeviceCodes(ctx); err != nil {
		return out, fmt.Errorf("sweeping expired device codes: %w", err)
	}
	if out.Sessions, err = s.queries.DeleteExpiredSessions(ctx); err != nil {
		return out, fmt.Errorf("sweeping expired sessions: %w", err)
	}
	return out, nil
}

// RunSweeper sweeps on a schedule until ctx is canceled.
//
// A sweep failure is logged and never fatal. These rows are already unusable — every query that reads them
// checks expiry in SQL — so a database that cannot serve a DELETE has a problem this loop is not the place
// to escalate, and an instance that refused to run because it could not tidy up would be worse than one
// carrying a few extra rows.
func (s *Service) RunSweeper(ctx context.Context) {
	log := logging.FromContext(ctx)

	// A random first delay, so replicas of the flagship do not all sweep on the same tick and contend on
	// the same rows. Up to one full interval: with a handful of replicas that is enough separation, and the
	// worst case is one interval of extra garbage on a cold start.
	first := time.NewTimer(time.Duration(rand.Int64N(int64(SweepInterval)))) //nolint:gosec // not a credential
	defer first.Stop()

	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}

	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		result, err := s.SweepExpired(ctx)
		switch {
		case ctx.Err() != nil:
			// Shutdown canceled the query mid-flight. Not a failure, and logging it as one would put an
			// alarming line in every clean stop.
			return
		case err != nil:
			log.Error().Err(err).Msg("sweeping expired auth rows failed")
		case result.Total() > 0:
			// Silent when there is nothing to do, which is most passes on a quiet instance.
			log.Debug().
				Int64("reset_tokens", result.ResetTokens).
				Int64("oauth_states", result.OAuthStates).
				Int64("oauth_exchange_codes", result.ExchangeCodes).
				Int64("device_codes", result.DeviceCodes).
				Int64("instance_invites", result.Invites).
				Int64("verification_tokens", result.VerificationTokens).
				Int64("sessions", result.Sessions).
				Msg("swept expired auth rows")
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

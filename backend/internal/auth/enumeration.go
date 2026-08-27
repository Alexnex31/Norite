package auth

import (
	"context"
	"time"
)

// Making an always-202 endpoint take the same time whether or not it found an account.
//
// # The gap this closes, measured
//
// POST /auth/password/reset/request and POST /auth/verify/request answer identically whichever branch they
// take, which is the half that was built. The other half was asserted rather than checked, and the comment
// on RequestPasswordReset said so outright: that the mail goes to a background queue, so "the request does
// the same work whether or not it found an account", leaving only an indexed single-row read either way.
//
// That was false. A found account with a password generates a token, takes a snowflake, and commits a
// transaction containing an UPDATE and an INSERT — a WAL flush the other branch never pays for. Measured
// over thirty calls each on a local Postgres:
//
//	unknown address   46.9 µs
//	known address    265.2 µs   (5.65x)
//
// Two hundred microseconds, consistent, and a distribution shift rather than noise. Reset actually
// distinguishes three states, not two: unknown, an OAuth-only account with no password to reset (fast,
// early return), and an account with a password (slow) — so the same measurement separates "has an
// account" from "signs in with Google", which is the detail ADR 0024's merged refusal message exists to
// withhold.
//
// # Why a floor rather than symmetric work
//
// The equalizer Register uses — do the expensive thing before the branch, so both paths pay it — has no
// analogue here. The expensive thing is writing a row to password_reset_tokens, which has a foreign key to
// users, so the not-found branch has nothing to write it against. Writing rows for addresses that do not
// exist would be a worse answer than the leak.
//
// So the endpoint takes a fixed budget and returns when the budget is spent. Crude, and it is the standard
// mitigation for exactly this shape. The cost is one sleeping goroutine per request on an endpoint already
// bounded to twenty requests a minute per /64, holding no memory and no connection — cheaper than the
// argon2id slot the login path spends on a missing account for the same reason.
//
// # What it does not close
//
// A slow path that exceeds the floor starts leaking again: under enough database load the write can take
// longer than enumerationFloor, and the floor stops equalizing precisely when the instance is busiest.
// Raising it trades latency for margin. Stated rather than discovered, because the failure mode is silent
// — the endpoint keeps answering correctly while the property it is asserting quietly stops holding.
//
// The floor is also not a defense against an attacker who can measure the *server's* work directly rather
// than the response; nothing at this layer is.

// enumerationFloor is the fixed budget. Two orders of magnitude above the gap it hides, and imperceptible
// to somebody who has just clicked "forgot password".
const enumerationFloor = 50 * time.Millisecond

// padToEnumerationFloor blocks until start plus the floor has elapsed.
//
// Returns early if the caller's context ends, since a client that has hung up cannot be leaked to and
// holding the goroutine past that point would only make a flood cheaper to mount.
func (s *Service) padToEnumerationFloor(ctx context.Context, start time.Time) {
	remaining := s.enumerationFloor - time.Since(start)
	if remaining <= 0 {
		return
	}

	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

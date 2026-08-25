package auth

import (
	"context"
	"fmt"

	"github.com/Alexnex31/Norite/backend/internal/db"
)

// Revoking every claim an account holds.
//
// # Why this is one function
//
// CLAUDE.md rule 17: a ban or a self-service account deletion must invoke the general-purpose
// revoke-all-sessions primitive, never a separately-implemented cleanup path. The failure that rule
// describes is not hypothetical — it is what this function is assembled from. Every step below was added
// to the password-reset path separately, by somebody noticing one more outstanding claim the previous
// steps missed:
//
//   - M4 revoked sessions, which is the obvious one;
//   - M5 added API tokens, because an intruder who minted one keeps it across a password change;
//   - M6 added OAuth exchange codes, because a callback page left open still trades its code for a pair;
//   - M9 added approved device authorizations, for the same reason one milestone later.
//
// Four callers each remembering four things is four chances to remember three. So the list lives here, and
// reset, sign-out-everywhere-else, M72's bans and account deletion call it rather than reproducing it.
//
// # What it does not do yet
//
// architecture.md §2 defines the primitive as three things, and this is one of them. The other two do not
// exist to be called:
//
//   - **Force-close live gateway connections (M18).** This matters more than it looks. Revoking a session
//     stops the *next* refresh, and an access token expires within AccessTokenTTL, so the REST surface is
//     bounded at fifteen minutes by construction (§17.10). A WebSocket is not: it authenticates once at
//     IDENTIFY and then stays open for as long as the client keeps it, so without an explicit close a
//     revoked account keeps receiving events indefinitely. When M18 lands, the close belongs *here*, in
//     this function, so every caller gets it without being edited.
//   - **Revoke every linked device's E2E device-link trust (M101, ADR 0014).** Same placement, same
//     reason.
//
// Deliberately not an interface with nil implementations. Two seams whose shapes are guesses would be two
// wrong shapes; a list of statements with the missing ones written into it is one place to add a line.

// RevocationScope bounds what a revocation spares.
//
// The zero value spares nothing, which is what a reset, a ban and a deletion all want. Sparing is the
// exception and has exactly one user today.
type RevocationScope struct {
	// KeepDeviceID spares one device's session family. Empty spares none.
	//
	// A device rather than a session, because a session is one row of a rotating family: sparing the row
	// the caller is currently using would sign them out at their next refresh instead of immediately,
	// which is a worse bug than signing them out now — it looks like it worked.
	KeepDeviceID string
}

// RevocationResult counts what one revocation removed.
//
// Counted rather than discarded so a caller can say what happened. "Signed out 3 other devices and revoked
// 2 API tokens" is a sentence only the database can supply, and a client that has to guess will guess
// wrong on the two cases that matter — nothing to revoke, and more than the user expected.
type RevocationResult struct {
	Sessions      int64
	APITokens     int64
	ExchangeCodes int64
	DeviceCodes   int64
}

// Total is how many claims the revocation removed altogether.
func (r RevocationResult) Total() int64 {
	return r.Sessions + r.APITokens + r.ExchangeCodes + r.DeviceCodes
}

// revokeEverything revokes every outstanding claim on an account.
//
// Takes a *db.Queries rather than opening its own transaction, so a caller can compose it into one that is
// already doing something else — which is the whole point at the reset path, where the password change and
// the revocation must land together or not at all.
func revokeEverything(ctx context.Context, q *db.Queries, userID int64, scope RevocationScope,
) (RevocationResult, error) {
	var out RevocationResult
	var err error

	if scope.KeepDeviceID == "" {
		out.Sessions, err = q.RevokeAllSessionsForUser(ctx, userID)
	} else {
		out.Sessions, err = q.RevokeAllSessionsForUserExceptDevice(ctx,
			db.RevokeAllSessionsForUserExceptDeviceParams{UserID: userID, DeviceID: scope.KeepDeviceID})
	}
	if err != nil {
		return out, fmt.Errorf("revoking sessions: %w", err)
	}

	// API tokens go with sessions, and the case that decides it is the one where this is happening
	// *because* the account was compromised: an attacker who minted a token while they had access would
	// otherwise keep it, and the password change would restore the owner's access while leaving the
	// intruder's credential working. The cost is real and accepted — somebody who merely forgot their
	// password has to re-mint their bots — so every caller says so rather than letting it be discovered.
	//
	// Not scoped by KeepDeviceID: an API token belongs to an account, not to a device. There is no
	// "this token is the one I am using" to spare, and a caller signing out its other devices is making a
	// security decision about the account rather than about one machine.
	if out.APITokens, err = q.RevokeAllAPITokensForUser(ctx, userID); err != nil {
		return out, fmt.Errorf("revoking API tokens: %w", err)
	}

	// An OAuth exchange code is not a session, so the two calls above miss it, and it is the one credential
	// in this codebase that gets rendered onto a screen — leaving it redeemable would mean an intruder with
	// a callback page open still collects a fresh token pair minutes after the action meant to lock them
	// out.
	if out.ExchangeCodes, err = q.RevokeOAuthExchangeCodesForUser(ctx, userID); err != nil {
		return out, fmt.Errorf("revoking oauth exchange codes: %w", err)
	}

	// And a device authorization already approved but not yet collected, which is the same kind of
	// outstanding claim for the same reason. One still waiting for approval belongs to nobody yet and is
	// correctly untouched.
	if out.DeviceCodes, err = q.RevokeDeviceCodesForUser(ctx, &userID); err != nil {
		return out, fmt.Errorf("revoking device codes: %w", err)
	}

	return out, nil
}

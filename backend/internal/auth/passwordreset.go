package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/mail"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// PasswordResetTTL is how long a reset link works.
//
// One hour, which is the shortest span that still survives a mail relay's greylisting delay plus a person
// noticing the message. The token is the whole proof of identity for changing a password, so this is the
// window in which a leaked email — forwarded, backed up, or read from a shared machine — is worth
// something to whoever finds it.
const PasswordResetTTL = time.Hour

// Password reset errors.
var (
	// ErrResetUnavailable means this instance has no mail relay, so it cannot send a reset link at all.
	//
	// Deliberately distinct from a credential failure: it does not depend on the address requested, so
	// reporting it discloses nothing about who has an account, and an operator who never configured SMTP
	// has not chosen for reset to fail silently.
	ErrResetUnavailable = errors.New("password reset is unavailable: this instance has no email relay configured")

	// ErrInvalidResetToken covers every way a reset token can fail: unknown, expired, already spent, or
	// issued to an address the account no longer uses. One error because the client is told one thing.
	ErrInvalidResetToken = errors.New("invalid or expired password reset token")
)

// RequestPasswordReset issues a reset token and emails it, if the address belongs to an account.
//
// # Why this returns nil for an unknown address
//
// The endpoint answers 202 either way (docs/architecture.md "Auth design"). Unlike registration — which
// necessarily discloses whether a name is taken — a reset request has no reason to tell the caller whether
// an address is registered, and doing so turns the endpoint into an account-enumeration oracle for any
// address someone cares to try.
//
// Timing does not leak it either, and that takes deliberate work rather than falling out of the design.
// This comment used to claim the opposite — that handing the mail to a background queue left the two
// branches doing the same work, with only an indexed single-row read between them. It was false, and being
// false is what kept anyone from looking: a found account generates a token and commits a transaction the
// not-found branch never pays for, measured at 265 µs against 47 µs. See padToEnumerationFloor, which is
// what makes the sentence above true, and what it still does not cover.
func (s *Service) RequestPasswordReset(ctx context.Context, rawEmail string) error {
	if s.mailer == nil || !s.mailer.Enabled() {
		// Ahead of the floor deliberately: this answers 503 for every address alike, so it discloses
		// nothing about any of them and there is nothing to pad.
		return ErrResetUnavailable
	}
	defer s.padToEnumerationFloor(ctx, time.Now())

	email := strings.TrimSpace(strings.ToLower(rawEmail))

	user, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No account. Nothing to send, and nothing to say.
			return nil
		}
		return fmt.Errorf("looking up account for reset: %w", err)
	}

	// An OAuth-only account has no password to reset. Silent for the same reason as above: "that account
	// signs in with Google" is exactly the detail an enumeration attempt is looking for.
	if user.PasswordHash == nil {
		logging.FromContext(ctx).Debug().
			Str("user_id", snowflake.ID(user.ID).String()).
			Msg("password reset requested for an account with no password")
		return nil
	}

	raw, hash, err := GeneratePasswordResetToken()
	if err != nil {
		return err
	}
	id, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("generating reset token ID: %w", err)
	}

	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		// Every older token for this account is spent first, so the newest link is the only one that
		// works. Without it, each request an anxious user makes leaves another live token behind.
		if _, err := q.InvalidateOutstandingResetTokens(ctx, user.ID); err != nil {
			return fmt.Errorf("invalidating previous reset tokens: %w", err)
		}

		_, err := q.CreatePasswordResetToken(ctx, db.CreatePasswordResetTokenParams{
			ID:        int64(id),
			UserID:    user.ID,
			TokenHash: hash,
			SentTo:    user.Email,
			ExpiresAt: timestamptz(s.now().Add(PasswordResetTTL)),
		})
		if err != nil {
			return fmt.Errorf("creating reset token: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// After the commit, never inside it (the same ordering rule gateway dispatch follows, CLAUDE.md
	// rule 5): a transaction that rolls back after the mail is queued would send a link to a token that
	// does not exist.
	if err := s.mailer.Enqueue(passwordResetMessage(s.publicBaseURL, user, raw)); err != nil {
		// The token is already committed and the caller already has its 202, so this cannot fail the
		// request. Logged at warn because a user is now waiting for an email that will not arrive.
		logging.FromContext(ctx).Warn().Err(err).
			Str("user_id", snowflake.ID(user.ID).String()).
			Msg("password reset token was issued but its email could not be queued")
	}
	return nil
}

// ConfirmPasswordReset spends a token and sets a new password.
func (s *Service) ConfirmPasswordReset(ctx context.Context, rawToken, newPassword string) error {
	hash, err := ParsePasswordResetToken(rawToken)
	if err != nil {
		// Wrong shape entirely — rejected without a database round trip.
		return ErrInvalidResetToken
	}

	token, err := s.queries.GetPasswordResetTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidResetToken
		}
		return fmt.Errorf("looking up reset token: %w", err)
	}

	user, err := s.queries.GetUserByID(ctx, token.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The account was deleted between request and confirm.
			return ErrInvalidResetToken
		}
		return fmt.Errorf("looking up account for reset: %w", err)
	}

	// The token was mailed to a specific address. If the account's address has changed since, this link is
	// no longer proof of anything: whoever controls the old mailbox is not necessarily the account holder.
	if !strings.EqualFold(token.SentTo, user.Email) {
		logging.FromContext(ctx).Warn().
			Str("user_id", snowflake.ID(user.ID).String()).
			Msg("reset token refused: the account's email changed after the link was sent")
		return ErrInvalidResetToken
	}

	// Hashed before the transaction opens. argon2id takes ~100ms and holds 64 MiB; doing it inside would
	// hold a pool connection for the duration, and the pool is deliberately small (§15.3).
	newHash, err := HashPassword(ctx, newPassword)
	if err != nil {
		return err
	}

	return database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		// Spending the token is the first statement and its own guard: single-use, unexpired, checked in
		// SQL. A second confirm racing this one matches zero rows here and fails before touching the
		// password.
		if _, err := q.ConsumePasswordResetToken(ctx, token.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalidResetToken
			}
			return fmt.Errorf("consuming reset token: %w", err)
		}

		if _, err := q.SetUserPassword(ctx, db.SetUserPasswordParams{
			ID:           token.UserID,
			PasswordHash: &newHash,
		}); err != nil {
			return fmt.Errorf("setting new password: %w", err)
		}

		// Everything the old password could reach is revoked in the same transaction, through the one
		// primitive every caller shares (CLAUDE.md rule 17). The reasoning for each step lives on that
		// function rather than here: this path is one of its callers now, not its owner.
		if _, err := revokeEverything(ctx, q, token.UserID, RevocationScope{}); err != nil {
			return err
		}

		return nil
	})
}

// passwordResetMessage builds the email.
//
// Plain text, and the link is the only thing in it that varies. Nothing about the account is echoed — not
// the username, not the address — because a reset mail is the one message guaranteed to be readable by
// whoever holds a mailbox someone may no longer control.
func passwordResetMessage(baseURL string, user db.User, rawToken string) mail.Message {
	return mail.Message{
		Kind:    mail.KindPasswordReset,
		To:      user.Email,
		Subject: "Reset your Norite password",
		Body: "Someone asked to reset the password for your Norite account.\n\n" +
			"Open this link to choose a new one:\n\n" +
			"    " + resetLink(baseURL, rawToken) + "\n\n" +
			"The link works once and expires in one hour.\n\n" +
			"Resetting your password signs you out everywhere and revokes any API tokens on the account, " +
			"so bots and scripts will need new ones.\n\n" +
			"If you did not ask for this, you can ignore this message — nothing has changed yet.\n",
	}
}

// resetLink builds the URL in the email.
//
// The token goes in the query string rather than the path so the page can read it without the router
// having to treat an opaque credential as a path segment — and it is URL-encoded, because a token is
// base64url and must survive whatever rewrites it on the way to the user.
func resetLink(baseURL, rawToken string) string {
	return strings.TrimSuffix(baseURL, "/") + "/reset?token=" + url.QueryEscape(rawToken)
}

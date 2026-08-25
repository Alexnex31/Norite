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

// Email verification — the capability this instance was missing, and the reason three separate things were
// wrong before M10.
//
// Registration answered 409 on a taken address, which made the instance enumerable, and it did so because
// there was no way to accept a registration and sort it out by mail. ADR 0024 had to merge its two
// unverified-address refusals into one message for the same reason. And an account whose provider will not
// vouch for its address could not sign in at all, because the only evidence available came from the
// provider. All three come down to this file not existing.

// EmailVerificationTTL is how long a verification link works.
//
// Twenty-four hours, against password reset's one. The two are not the same kind of value even though they
// share a shape: a reset link is the entire proof of identity for changing a password, so its window is
// sized against a leaked mailbox. A verification link only marks an address confirmed — holding one lets
// somebody finish a registration that already required the password they chose — and it is followed on a
// person's own schedule, often on a different device, sometimes the next morning.
const EmailVerificationTTL = 24 * time.Hour

// Verification errors.
var (
	// ErrEmailNotVerified means the account exists and the password was right, but the address has not
	// been confirmed.
	//
	// Reported only *after* credentials check out. Before that it would be an oracle for "this address is
	// registered but unverified"; after it, the caller has already proved they hold the password, so it
	// discloses nothing they did not know.
	ErrEmailNotVerified = errors.New("this account's email address has not been verified yet")

	// ErrInvalidVerificationToken covers every way a verification link can fail: unknown, expired, already
	// used, or issued to an address the account no longer has. One error because the client is told one
	// thing.
	ErrInvalidVerificationToken = errors.New("invalid or expired verification link")
)

// VerificationRequired reports whether this instance can hold accounts to a verified address at all.
//
// False when there is no mail relay, and that is the accepted limitation this milestone ships with: an
// instance with no way to send mail cannot verify anything, so registration marks new accounts verified on
// creation and the enumeration hole stays open there. Refusing to register instead would trade a working
// instance for nothing — the hole cannot be closed by any route without a relay.
//
// Stated in three places rather than left to be discovered: the wizard warns when SMTP is declined, the
// server logs it once at startup, and docs/architecture.md §14 records it. The failure mode to guard
// against is this becoming quiet, not it existing.
func (s *Service) VerificationRequired() bool {
	return s.mailer != nil && s.mailer.Enabled()
}

// sendVerification issues a token and queues the mail, inside the caller's transaction.
//
// The queue call is deliberately *not* here: it must happen after the commit, for the reason rule 5 gives
// for gateway events — a transaction that rolls back after the mail is queued sends a link to a token that
// does not exist. The caller gets the message back and enqueues it afterwards.
func (s *Service) buildVerification(ctx context.Context, q *db.Queries, user db.User) (mail.Message, error) {
	raw, hash, err := GenerateEmailVerificationToken()
	if err != nil {
		return mail.Message{}, err
	}
	id, err := s.ids.Next()
	if err != nil {
		return mail.Message{}, fmt.Errorf("generating verification token ID: %w", err)
	}

	// Older tokens are spent first, so the newest link is the only one that works.
	if _, err := q.InvalidateOutstandingVerificationTokens(ctx, user.ID); err != nil {
		return mail.Message{}, fmt.Errorf("invalidating previous verification tokens: %w", err)
	}

	if _, err := q.CreateEmailVerificationToken(ctx, db.CreateEmailVerificationTokenParams{
		ID:        int64(id),
		UserID:    user.ID,
		TokenHash: hash,
		SentTo:    user.Email,
		ExpiresAt: timestamptz(s.now().Add(EmailVerificationTTL)),
	}); err != nil {
		return mail.Message{}, fmt.Errorf("creating verification token: %w", err)
	}

	return verificationMessage(s.publicBaseURL, user, raw), nil
}

// RequestEmailVerification re-sends a verification link, if the address belongs to an unverified account.
//
// Always reports success, exactly as RequestPasswordReset does and for the same reason: this endpoint has
// no business telling a caller whether an address is registered, or whether the account behind it is
// verified. Both would be usable to enumerate.
func (s *Service) RequestEmailVerification(ctx context.Context, rawEmail string) error {
	if !s.VerificationRequired() {
		// No relay, so nothing to send — and on such an instance every account is already verified on
		// creation, which makes this a no-op rather than a failure.
		return nil
	}

	email := strings.TrimSpace(strings.ToLower(rawEmail))
	log := logging.FromContext(ctx)

	user, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("looking up account for verification: %w", err)
	}
	if user.EmailVerifiedAt.Valid {
		log.Debug().Str("user_id", snowflake.ID(user.ID).String()).
			Msg("verification requested for an already-verified address")
		return nil
	}

	var msg mail.Message
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		msg, err = s.buildVerification(ctx, s.queries.WithTx(tx), user)
		return err
	})
	if err != nil {
		return err
	}

	// After the commit, never inside it.
	if err := s.mailer.Enqueue(msg); err != nil {
		log.Warn().Err(err).Str("user_id", snowflake.ID(user.ID).String()).
			Msg("queueing a verification email failed")
	}
	return nil
}

// ConfirmEmailVerification marks an address verified.
func (s *Service) ConfirmEmailVerification(ctx context.Context, rawToken string) error {
	hash, err := ParseEmailVerificationToken(rawToken)
	if err != nil {
		return ErrInvalidVerificationToken
	}

	token, err := s.queries.GetEmailVerificationTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidVerificationToken
		}
		return fmt.Errorf("looking up verification token: %w", err)
	}

	return database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		// Single-use and expiry live in this statement's WHERE clause, so two people following the same
		// link produce exactly one winner.
		spent, err := q.ConsumeEmailVerificationToken(ctx, token.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalidVerificationToken
			}
			return fmt.Errorf("consuming verification token: %w", err)
		}

		// The address must still be the one the mail went to. Registering with an address you control,
		// changing it to one you do not, and then following your own link would otherwise mark somebody
		// else's address verified — which is exactly the takeover the linking rule exists to prevent.
		if _, err := q.MarkEmailVerified(ctx, db.MarkEmailVerifiedParams{
			ID:    spent.UserID,
			Email: spent.SentTo,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInvalidVerificationToken
			}
			return fmt.Errorf("marking email verified: %w", err)
		}
		return nil
	})
}

// verificationMessage is the mail a new account gets.
func verificationMessage(baseURL string, user db.User, rawToken string) mail.Message {
	return mail.Message{
		Kind:    mail.KindEmailVerification,
		To:      user.Email,
		Subject: "Confirm your Norite address",
		Body: "Someone created a Norite account with this address.\n\n" +
			"Open this link to confirm it and finish signing up:\n\n" +
			"    " + verificationLink(baseURL, rawToken) + "\n\n" +
			"The link works once and expires in 24 hours.\n\n" +
			"Until it is confirmed, the account cannot be signed in to.\n\n" +
			"If this was not you, you can ignore this message — the account stays unusable and no one " +
			"can sign in to it.\n",
	}
}

// registrationNoticeMessage is what an address that *already* has an account receives instead.
//
// This is the half of the anti-enumeration design that makes it honest rather than merely quiet.
// Registration answers identically either way, so somebody has to be told the two cases differ — and the
// only party entitled to know is whoever controls the address. It is also the one warning a person gets
// that somebody is probing for their account.
//
// It deliberately carries no link and asks for nothing. A "was this you?" button would be a phishing
// template written by us, and there is nothing to confirm: no account was created and nothing changed.
func registrationNoticeMessage(baseURL string, email string) mail.Message {
	return mail.Message{
		Kind:    mail.KindRegistrationNotice,
		To:      email,
		Subject: "Someone tried to register with your address",
		Body: "Someone tried to create a Norite account with this address, but it already has one.\n\n" +
			"Nothing has changed and no new account was created.\n\n" +
			"If it was you, sign in instead — or reset your password at:\n\n" +
			"    " + strings.TrimSuffix(baseURL, "/") + "/reset\n\n" +
			"If it was not you, there is nothing to do. Whoever tried does not have access to your " +
			"account, and this message is the only thing that happened.\n",
	}
}

// verificationLink builds the URL in the email.
//
// The token goes in the query string rather than the path, and URL-encoded, for the reasons resetLink
// records: the page reads it without the router treating an opaque credential as a path segment, and it is
// base64url, which must survive whatever rewrites it on the way to the user.
func verificationLink(baseURL, rawToken string) string {
	return strings.TrimSuffix(baseURL, "/") + "/verify?token=" + url.QueryEscape(rawToken)
}

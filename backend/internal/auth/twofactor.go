package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// The second factor, and the one property that makes it hold.
//
// # Why a proof is a value rather than a check
//
// M11 learned the same lesson twice. Its revocation list was assembled from four milestones each
// remembering one more outstanding claim, so it became one function. Its liveness rule shipped as
// per-handler checks, missed POST /auth/tokens, became one middleware — and then turned out to have left
// /instance outside its own wording, which an external review found. A rule written as N call sites has N
// chances to miss one, and this package has now missed one three times.
//
// So the factor is not a check that session-minting code is expected to call. It is a *value that
// session-minting code cannot proceed without*, and which nothing outside this file can construct:
// factorProof has no exported fields, no constructor but the two below, and the zero value authorizes
// nothing. startSession and ApproveDeviceAuthorization take one. A future third way to start a session
// will not compile until its author has obtained a proof, which is the only form of "don't forget" that
// actually works.
//
// # Where the gate is not
//
// RedeemDeviceCode mints a session and takes no proof, deliberately. The waiting CLI has held its device
// code since before a browser was involved and there is nobody at that terminal to type six digits; the
// factor was proved in the browser, at approval, which is why ApproveDeviceAuthorization is the function
// that requires the proof. Gating redemption would demand a code from a machine with nobody at it.
//
// Refresh takes no proof either. The session it rotates proved the factor when it was established, and
// asking again every fifteen minutes would make the factor a nuisance rather than a control.
//
// ConfirmPasswordReset needs nothing because it starts no session: a reset changes the password and
// revokes what the old one could reach, and the next login still owes the factor. That is the classic
// bypass and it is closed by the shape of the code rather than by a check — but it has a test anyway,
// because "closed by construction" is a claim that stops being true quietly.

// totpIssuer is the label an authenticator app shows beside the account.
//
// Fixed rather than derived from the instance's public base URL: it is what a person reads in a list of
// accounts on their phone, and an instance that changed its hostname would otherwise silently orphan every
// enrollment's label while the codes kept working.
const totpIssuer = "Norite"

// recoveryCodeLength is the length of one recovery code, in characters from userCodeAlphabet.
//
// Ten characters of a 32-symbol alphabet is fifty bits, which is not guessable and is short enough to read
// off a screen and type into another — the same trade the device flow's user code and M10's invite code
// make, at the length each of those needs.
const recoveryCodeLength = 10

// recoveryCodeCount is how many are issued at once.
const recoveryCodeCount = 10

// factorProof is evidence that any second factor on an account has been satisfied.
//
// Unexported with unexported fields, and constructible only by factorSatisfied and proveFactor below. The
// zero value proves nothing and every consumer refuses it. See this file's header for why the enforcement
// is a type rather than a convention.
type factorProof struct {
	userID int64
	proved bool
}

// authorizes reports whether this proof was obtained for the given account.
//
// The user check is not ceremony. Without it a proof obtained for one account would satisfy a session
// being minted for another, which is the shape of every confused-deputy bug — and the two ids travel
// separately through the sign-in paths precisely because the challenge carries one and the caller supplies
// the other.
func (p factorProof) authorizes(userID int64) bool {
	return p.proved && p.userID != 0 && p.userID == userID
}

// factorSatisfied returns a proof for an account that owes no second factor.
//
// ErrTwoFactorRequired when one is owed — which is not an error in the usual sense, and callers on the
// sign-in paths treat it as a branch rather than a failure.
//
// An *unconfirmed* enrollment is not a factor. That is what stops somebody who started enrolling and closed
// the tab from being locked out of their own account, and it is why the query returns unconfirmed rows
// rather than hiding them.
func (s *Service) factorSatisfied(ctx context.Context, userID int64) (factorProof, error) {
	enrollment, err := s.queries.GetTOTPForUser(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return factorProof{userID: userID, proved: true}, nil
	case err != nil:
		return factorProof{}, fmt.Errorf("looking up the second factor: %w", err)
	}

	if !enrollment.ConfirmedAt.Valid {
		return factorProof{userID: userID, proved: true}, nil
	}
	return factorProof{}, ErrTwoFactorRequired
}

// hasConfirmedFactor reports whether an account has a live second factor, for display.
//
// Separate from factorSatisfied because "should I show a Disable button" and "may this sign-in proceed"
// are different questions, and answering the first through an error value would invite a caller to treat
// the second as cosmetic.
func (s *Service) hasConfirmedFactor(ctx context.Context, userID int64) (bool, error) {
	enrollment, err := s.queries.GetTOTPForUser(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("looking up the second factor: %w", err)
	}
	return enrollment.ConfirmedAt.Valid, nil
}

// proveFactor verifies a code — a TOTP code, or one of the account's recovery codes.
//
// Both kinds are accepted at one entry point on purpose. A person who has lost their phone types what they
// have and it works; making them choose a different endpoint first would mean the recovery path is the one
// nobody can find at the moment they need it.
//
// TOTP is tried first because it is the ordinary case and costs no database write. A recovery code is only
// consulted when the code is not a live TOTP value, which also means a six-digit string can never
// accidentally spend a recovery code — they are different lengths from different alphabets.
func (s *Service) proveFactor(ctx context.Context, userID int64, code string) (factorProof, error) {
	enrollment, err := s.queries.GetTOTPForUser(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No enrollment at all: there is nothing to prove and nothing that should be accepted as proof.
		// Reported as an invalid code rather than as "no factor", because a caller that reached here with
		// a code believed there was one, and the difference is not theirs to learn.
		return factorProof{}, ErrInvalidFactorCode
	case err != nil:
		return factorProof{}, fmt.Errorf("looking up the second factor: %w", err)
	}
	if !enrollment.ConfirmedAt.Valid {
		return factorProof{}, ErrInvalidFactorCode
	}

	secret, err := s.issuer.openTOTPSecret(enrollment.SecretEncrypted)
	if err != nil {
		return factorProof{}, err
	}

	// Skew of one period either side, which is thirty seconds here. It is what makes the factor usable on
	// a phone whose clock is a little off, and it is the standard allowance; widening it multiplies the
	// codes that are live at once, which is the only thing bounding a guess.
	ok, err := totp.ValidateCustom(code, secret, s.now().UTC(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err == nil && ok {
		return factorProof{userID: userID, proved: true}, nil
	}

	return s.proveRecoveryCode(ctx, userID, code)
}

// proveRecoveryCode spends one of the account's recovery codes.
func (s *Service) proveRecoveryCode(ctx context.Context, userID int64, raw string) (factorProof, error) {
	normalized, ok := normalizeTypedCode(raw, recoveryCodeLength)
	if !ok {
		return factorProof{}, ErrInvalidFactorCode
	}

	// Single-use lives in the statement, not here — two requests presenting the same code both reach it
	// and Postgres serializes them on the row, so exactly one matches. See ConsumeRecoveryCode.
	if _, err := s.queries.ConsumeRecoveryCode(ctx, db.ConsumeRecoveryCodeParams{
		UserID:   userID,
		CodeHash: string(HashToken(normalized)),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return factorProof{}, ErrInvalidFactorCode
		}
		return factorProof{}, fmt.Errorf("spending a recovery code: %w", err)
	}
	return factorProof{userID: userID, proved: true}, nil
}

// BeginTOTPEnrollment mints a secret and returns it, once.
//
// The row is written unconfirmed, so nothing about the account changes until a code proves the
// authenticator works. Starting again replaces an unconfirmed enrollment; an account that already has a
// confirmed one is refused here, because replacing a live factor is a change to the account's security
// state and goes through the disable path, which requires proving the factor first.
func (s *Service) BeginTOTPEnrollment(ctx context.Context, userID int64, accountName string,
) (secret, uri string, err error) {
	confirmed, err := s.hasConfirmedFactor(ctx, userID)
	if err != nil {
		return "", "", err
	}
	if confirmed {
		return "", "", ErrTwoFactorAlreadyEnabled
	}

	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: accountName})
	if err != nil {
		return "", "", fmt.Errorf("generating a totp secret: %w", err)
	}

	sealed, err := s.issuer.sealTOTPSecret(key.Secret())
	if err != nil {
		return "", "", err
	}
	if _, err := s.queries.UpsertTOTPEnrollment(ctx, db.UpsertTOTPEnrollmentParams{
		UserID:          userID,
		SecretEncrypted: sealed,
	}); err != nil {
		return "", "", fmt.Errorf("storing the enrollment: %w", err)
	}

	return key.Secret(), key.URL(), nil
}

// ConfirmTOTPEnrollment proves the authenticator works and turns the factor on.
//
// Returns the recovery codes, which exist from this moment and are shown exactly once. Generating them
// here rather than at enrollment is deliberate: codes handed out for an enrollment that was never confirmed
// are codes somebody wrote down for a factor they do not have.
func (s *Service) ConfirmTOTPEnrollment(ctx context.Context, userID int64, code string) ([]string, error) {
	enrollment, err := s.queries.GetTOTPForUser(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNoTwoFactorEnrollment
	case err != nil:
		return nil, fmt.Errorf("looking up the enrollment: %w", err)
	}
	if enrollment.ConfirmedAt.Valid {
		return nil, ErrTwoFactorAlreadyEnabled
	}

	secret, err := s.issuer.openTOTPSecret(enrollment.SecretEncrypted)
	if err != nil {
		return nil, err
	}
	ok, err := totp.ValidateCustom(code, secret, s.now().UTC(), totp.ValidateOpts{
		Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil || !ok {
		return nil, ErrInvalidFactorCode
	}

	raw, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}

	// One transaction: the factor becomes required and its recovery codes exist together, or neither
	// happens. A confirmation that committed without codes would leave somebody one lost phone away from
	// an account nobody can reach.
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)
		if _, err := q.ConfirmTOTP(ctx, userID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTwoFactorAlreadyEnabled
			}
			return fmt.Errorf("confirming the enrollment: %w", err)
		}
		return s.writeRecoveryCodes(ctx, q, userID, hashes)
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// DisableTwoFactor turns the factor off, and requires the factor to do it.
//
// The step-up is the point. M11 accepts that an access token outlives its session by up to fifteen minutes
// (§17.10), so a stolen session inside that window would otherwise be able to remove the very control that
// stands between an intruder and the account. Requiring a current code means an attacker must hold the
// factor to disable the factor.
//
// Every other session goes with it, through revokeEverything rather than a cleanup path of its own
// (rule 17): the sessions that predate this were established under different rules, and the person turning
// the factor off is making a decision about the account rather than about this machine.
func (s *Service) DisableTwoFactor(ctx context.Context, userID snowflake.ID, currentSessionID snowflake.ID,
	code string,
) (RevocationResult, error) {
	if _, err := s.proveFactor(ctx, int64(userID), code); err != nil {
		return RevocationResult{}, err
	}

	currentDevice, err := s.currentDeviceID(ctx, userID, currentSessionID)
	if err != nil {
		return RevocationResult{}, err
	}

	var out RevocationResult
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)
		if _, err := q.DeleteTOTPForUser(ctx, int64(userID)); err != nil {
			return fmt.Errorf("removing the enrollment: %w", err)
		}
		if _, err := q.DeleteRecoveryCodesForUser(ctx, int64(userID)); err != nil {
			return fmt.Errorf("removing the recovery codes: %w", err)
		}
		out, err = revokeEverything(ctx, q, int64(userID), RevocationScope{KeepDeviceID: currentDevice})
		return err
	})
	if err != nil {
		return RevocationResult{}, err
	}
	return out, nil
}

// RegenerateRecoveryCodes replaces the whole set, and requires the factor to do it.
//
// Same step-up as disabling, for the same reason: a set of codes an attacker generated is a set of codes
// that outlives every other thing this account can do about them.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID int64, code string) ([]string, error) {
	if _, err := s.proveFactor(ctx, userID, code); err != nil {
		return nil, err
	}

	raw, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}

	// Replaced in one transaction, so there is no instant in which the account has no codes at all.
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)
		if _, err := q.DeleteRecoveryCodesForUser(ctx, userID); err != nil {
			return fmt.Errorf("clearing the old recovery codes: %w", err)
		}
		return s.writeRecoveryCodes(ctx, q, userID, hashes)
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// RemainingRecoveryCodes is how many of the account's codes are unspent.
func (s *Service) RemainingRecoveryCodes(ctx context.Context, userID int64) (int64, error) {
	n, err := s.queries.CountLiveRecoveryCodes(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("counting recovery codes: %w", err)
	}
	return n, nil
}

// writeRecoveryCodes inserts a fresh set inside the caller's transaction.
func (s *Service) writeRecoveryCodes(ctx context.Context, q *db.Queries, userID int64,
	hashes []TokenHash,
) error {
	for _, hash := range hashes {
		id, err := s.ids.Next()
		if err != nil {
			return fmt.Errorf("generating a recovery code ID: %w", err)
		}
		if _, err := q.CreateRecoveryCode(ctx, db.CreateRecoveryCodeParams{
			ID:       int64(id),
			UserID:   userID,
			CodeHash: string(hash),
		}); err != nil {
			return fmt.Errorf("storing a recovery code: %w", err)
		}
	}
	return nil
}

// generateRecoveryCodes mints a fresh set and their stored hashes.
//
// The raw values are returned once, to be shown once. Nothing recoverable from the database can be
// presented as one (rule 8), which is the same property every opaque credential in this package has.
func generateRecoveryCodes() (raw []string, hashes []TokenHash, err error) {
	raw = make([]string, 0, recoveryCodeCount)
	hashes = make([]TokenHash, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		code, err := randomCode(recoveryCodeLength)
		if err != nil {
			return nil, nil, fmt.Errorf("generating a recovery code: %w", err)
		}
		raw = append(raw, code)
		hashes = append(hashes, HashToken(code))
	}
	return raw, hashes, nil
}

// twoFactorChallengeTTL bounds how long a half-finished sign-in stays resumable.
//
// Long enough to find a phone, short enough that a challenge left on a shared machine is not a standing
// invitation. It is not a credential on its own — it is issued only after a correct password — but it is
// one half of one.
const twoFactorChallengeTTL = 5 * time.Minute

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/logging"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// The OAuth sign-in flow, in three legs:
//
//	StartOAuth      mints state + PKCE verifier, returns where to send the user
//	CompleteOAuth   consumes the state, exchanges the code, decides what happened
//	ExchangeOAuth   trades the one-time code from the previous leg for a token pair
//
// The split exists because the middle leg lands in a browser, and a browser must never be handed a token
// pair: a redirect carrying one puts a credential into the address bar, the history, the Referer header,
// and every proxy log on the way. Only a single-use code crosses that boundary.

// OAuthExchangeCodeTTL is how long the code from a callback stays redeemable. Short because the client
// redeems it immediately — the only reason it exists is to avoid putting tokens in a URL.
const OAuthExchangeCodeTTL = 2 * time.Minute

// OAuthSignupTTL is how long an unfinished signup can sit before the person has to start again.
const OAuthSignupTTL = 30 * time.Minute

// OAuth service errors.
var (
	// ErrOAuthLinkRequired is the refusal that carries the whole linking rule: this provider account's
	// email belongs to an existing Norite account, but the provider will not vouch for it.
	//
	// Distinct from every other failure on purpose. It is the one case where the person genuinely does
	// own both accounts and needs to be told what to do about it, and collapsing it into a generic error
	// would leave them with a sign-in that simply never works and no way to find out why.
	ErrOAuthLinkRequired = errors.New(
		"an account already uses this email address, and the provider has not verified it: " +
			"sign in with your password and link this provider from settings")

	// ErrOAuthEmailUnverified is the same refusal one step earlier: the provider will not vouch for the
	// address, and no account owns it yet.
	//
	// Separate from ErrOAuthLinkRequired because the advice differs — there is no existing account to sign
	// into with a password, so the way forward is to verify the address at the provider or register here
	// directly.
	ErrOAuthEmailUnverified = errors.New(
		"the provider has not verified this email address: verify it with the provider and try again, " +
			"or create an account with a password instead")

	// ErrOAuthIdentityLinkedElsewhere is this provider account already belonging to a different Norite
	// account — most often one that has since been deleted.
	ErrOAuthIdentityLinkedElsewhere = errors.New(
		"this provider account is already linked to a different Norite account")

	// ErrOAuthAccountAlreadyLinked is the mirror image: the Norite account this would link to already has a
	// different account at the same provider.
	ErrOAuthAccountAlreadyLinked = errors.New(
		"that account is already linked to a different account at this provider")

	// ErrOAuthExchangeCode covers an unknown, expired, or already-spent exchange code.
	ErrOAuthExchangeCode = errors.New("invalid or expired sign-in code")

	// ErrOAuthSignupToken covers a bad continuation token from the username step.
	ErrOAuthSignupToken = errors.New("this sign-up has expired; start again")

	// ErrOAuthRegistrationClosed is an invite-only instance refusing to create an account. Linking an
	// existing account is still allowed — the gate is on new accounts, not on providers.
	ErrOAuthRegistrationClosed = errors.New("registration on this instance requires an invite code")
)

// OAuthOutcome is what happened at the callback.
type OAuthOutcome struct {
	// ExchangeCode is set when the person is signed in and a client may now collect a token pair.
	ExchangeCode string

	// SignupToken is set when there is no account yet and a username must be chosen. Mutually exclusive
	// with ExchangeCode.
	SignupToken string
	// SuggestedUsername is a starting point for the username field, never a value that gets used without
	// the person seeing it.
	SuggestedUsername string
	// Email is shown on the signup page so the person can see which account they are about to create.
	Email string
}

// SignedIn reports whether the flow completed against an existing account.
func (o OAuthOutcome) SignedIn() bool { return o.ExchangeCode != "" }

// StartOAuth begins an authorization request and returns the URL to send the user to.
func (s *Service) StartOAuth(ctx context.Context, providerName string) (string, error) {
	provider, err := s.oauth.Get(providerName)
	if err != nil {
		return "", err
	}

	rawState, stateHash, err := GenerateOAuthState()
	if err != nil {
		return "", err
	}
	verifier := GenerateOAuthVerifier()

	id, err := s.ids.Next()
	if err != nil {
		return "", fmt.Errorf("generating oauth state ID: %w", err)
	}

	if _, err := s.queries.CreateOAuthState(ctx, db.CreateOAuthStateParams{
		ID:           int64(id),
		StateHash:    stateHash,
		Provider:     string(provider.Name()),
		CodeVerifier: verifier,
		ExpiresAt:    timestamptz(s.now().Add(OAuthStateTTL)),
	}); err != nil {
		return "", fmt.Errorf("recording oauth state: %w", err)
	}

	return provider.AuthCodeURL(rawState, verifier), nil
}

// CompleteOAuth handles the provider's callback.
func (s *Service) CompleteOAuth(ctx context.Context, providerName, rawState, code string) (OAuthOutcome, error) {
	provider, err := s.oauth.Get(providerName)
	if err != nil {
		return OAuthOutcome{}, err
	}

	stateHash, err := ParseOAuthState(rawState)
	if err != nil {
		return OAuthOutcome{}, ErrOAuthState
	}

	// Spent before the code is exchanged, not after. A callback replayed — a refresh, a back button, or
	// someone who captured the redirect — must not reach the provider a second time with the same
	// verifier, and the WHERE clause is what guarantees only one caller gets past this point.
	state, err := s.queries.ConsumeOAuthState(ctx, stateHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OAuthOutcome{}, ErrOAuthState
		}
		return OAuthOutcome{}, fmt.Errorf("consuming oauth state: %w", err)
	}

	// The state was minted for one provider. Without this, a state issued for Google could be presented on
	// GitHub's callback, and the code would be exchanged against the wrong client.
	if state.Provider != string(provider.Name()) {
		return OAuthOutcome{}, ErrOAuthState
	}

	identity, err := provider.Identity(ctx, code, state.CodeVerifier)
	if err != nil {
		return OAuthOutcome{}, err
	}

	return s.resolveOAuthIdentity(ctx, identity)
}

// resolveOAuthIdentity decides what an authenticated provider identity means for this instance.
func (s *Service) resolveOAuthIdentity(ctx context.Context, identity OAuthIdentity) (OAuthOutcome, error) {
	log := logging.FromContext(ctx)

	// Already linked: the ordinary sign-in, and the only path that consults nothing but the provider's ID.
	existing, err := s.queries.GetOAuthIdentity(ctx, db.GetOAuthIdentityParams{
		Provider:       string(identity.Provider),
		ProviderUserID: identity.UserID,
	})
	if err == nil {
		code, err := s.issueOAuthExchangeCode(ctx, existing.UserID)
		if err != nil {
			return OAuthOutcome{}, err
		}
		return OAuthOutcome{ExchangeCode: code}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return OAuthOutcome{}, fmt.Errorf("looking up oauth identity: %w", err)
	}

	// Not linked. Does the address belong to someone already?
	email := strings.TrimSpace(strings.ToLower(identity.Email))
	user, err := s.queries.GetUserByEmail(ctx, email)
	switch {
	case err == nil:
		// This is the decision the whole milestone turns on.
		//
		// A provider that has verified the address is asserting that the person completing this flow
		// controls it, which is the same evidence a password login rests on. A provider that has not is
		// asserting nothing: anyone can type someone else's address into an account at a provider that
		// never checks, and auto-linking on that would hand them the matching Norite account.
		if !identity.EmailVerified {
			log.Warn().
				Str("provider", string(identity.Provider)).
				Str("user_id", snowflake.ID(user.ID).String()).
				Msg("oauth sign-in refused: provider has not verified an address that matches an account")
			return OAuthOutcome{}, ErrOAuthLinkRequired
		}

		if err := s.linkOAuthIdentity(ctx, user.ID, identity); err != nil {
			return OAuthOutcome{}, err
		}
		code, err := s.issueOAuthExchangeCode(ctx, user.ID)
		if err != nil {
			return OAuthOutcome{}, err
		}
		return OAuthOutcome{ExchangeCode: code}, nil

	case errors.Is(err, pgx.ErrNoRows):
		// Nobody owns the address — which does not mean nobody owns the *mailbox*. Creating an account from
		// an address the provider will not vouch for is the same takeover the branch above refuses, one step
		// earlier: it registers someone else's address to whoever typed it, and CreateOAuthUser records it as
		// verified, a claim this instance has no basis for.
		//
		// Refused here rather than left to parseOAuthSignupToken, which also checks it. That check is a
		// backstop against a future caller minting a token differently; reaching it from this path meant
		// rendering "choose your username", accepting one, and answering "this sign-up has expired" — a
		// dead end no amount of retrying escaped.
		if !identity.EmailVerified {
			log.Warn().
				Str("provider", string(identity.Provider)).
				Msg("oauth sign-up refused: provider has not verified the address")
			return OAuthOutcome{}, ErrOAuthEmailUnverified
		}

		// Nothing is written until a username is chosen — see issueOAuthSignupToken.
		if s.registrationMode != RegistrationOpen {
			return OAuthOutcome{}, ErrOAuthRegistrationClosed
		}
		token, err := s.issueOAuthSignupToken(identity)
		if err != nil {
			return OAuthOutcome{}, err
		}
		return OAuthOutcome{
			SignupToken:       token,
			SuggestedUsername: suggestUsername(identity),
			Email:             email,
		}, nil

	default:
		return OAuthOutcome{}, fmt.Errorf("looking up account by email: %w", err)
	}
}

// linkOAuthIdentity records the link between a provider account and an existing Norite account.
func (s *Service) linkOAuthIdentity(ctx context.Context, userID int64, identity OAuthIdentity) error {
	id, err := s.ids.Next()
	if err != nil {
		return fmt.Errorf("generating oauth identity ID: %w", err)
	}

	_, err = s.queries.CreateOAuthIdentity(ctx, db.CreateOAuthIdentityParams{
		ID:             int64(id),
		UserID:         userID,
		Provider:       string(identity.Provider),
		ProviderUserID: identity.UserID,
		Email:          identity.Email,
	})
	switch {
	case err == nil:
		return nil
	case uniqueViolation(err) != "":
		return s.classifyLinkConflict(ctx, userID, identity)
	default:
		return fmt.Errorf("linking oauth identity: %w", err)
	}
}

// classifyLinkConflict works out what a unique violation on oauth_identities actually meant.
//
// The table has two unique constraints and they mean opposite things, so treating every violation as
// success — which is what this was — signs the person in while the link they just authorized goes
// unrecorded. Nothing visibly breaks, which is the problem: every later sign-in silently falls back to the
// email-match path, and ADR 0024's "once linked, sign-in consults the provider's immutable user ID and
// nothing else" never takes effect for that identity.
//
// Decided by reading the row back rather than by matching pgErr.ConstraintName. Those names are Postgres'
// own, derived from the column list, so a later migration that renames or restates a constraint would turn
// this into a silent misclassification rather than something that fails to compile.
func (s *Service) classifyLinkConflict(ctx context.Context, userID int64, identity OAuthIdentity) error {
	existing, err := s.queries.GetOAuthIdentityIncludingDeleted(ctx,
		db.GetOAuthIdentityIncludingDeletedParams{
			Provider:       string(identity.Provider),
			ProviderUserID: identity.UserID,
		})
	switch {
	case err == nil && existing.UserID == userID:
		// Two callbacks raced for the same first link. The winner wrote exactly the row this call wanted, so
		// the loser has nothing left to do and the sign-in proceeds.
		return nil

	case err == nil:
		// UNIQUE (provider, provider_user_id): this provider account belongs to someone else. Reachable
		// because GetOAuthIdentity hides identities owned by soft-deleted accounts, so a deleted account's
		// link can collide with a live account that happens to share the address. Refusing matches what
		// password registration already does with a deleted account's address, rather than inventing a
		// second answer for the same situation.
		return ErrOAuthIdentityLinkedElsewhere

	case errors.Is(err, pgx.ErrNoRows):
		// No row for this provider account, so the violation was the other constraint,
		// UNIQUE (user_id, provider): the account already has a different account at this provider.
		return ErrOAuthAccountAlreadyLinked

	default:
		return fmt.Errorf("resolving oauth link conflict: %w", err)
	}
}

// CompleteOAuthSignup creates the account a signup token stands for, once a username has been chosen.
func (s *Service) CompleteOAuthSignup(ctx context.Context, signupToken, rawUsername string) (string, error) {
	identity, err := s.parseOAuthSignupToken(signupToken)
	if err != nil {
		return "", err
	}

	username := NormalizeUsername(rawUsername)
	if !ValidUsername(username) {
		return "", ErrInvalidUsername
	}

	// Re-checked here rather than trusted from the callback: the token is valid for half an hour, and the
	// instance could have been switched to invite-only in between.
	if s.registrationMode != RegistrationOpen {
		return "", ErrOAuthRegistrationClosed
	}

	email := strings.TrimSpace(strings.ToLower(identity.Email))

	userID, err := s.ids.Next()
	if err != nil {
		return "", fmt.Errorf("generating user ID: %w", err)
	}
	identityID, err := s.ids.Next()
	if err != nil {
		return "", fmt.Errorf("generating oauth identity ID: %w", err)
	}

	var user db.User
	err = database.RunInTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.queries.WithTx(tx)

		// The account and its identity are created together or not at all. An account with no identity
		// would be unreachable — no password, no provider — and an identity with no account cannot exist.
		created, err := q.CreateOAuthUser(ctx, db.CreateOAuthUserParams{
			ID:       int64(userID),
			Username: username,
			Email:    email,
			// The provider's display name is not used here. It is free text from a third party that would
			// land in every client's UI, and the person has just told us what to call them.
			DisplayName: username,
			// The provider verified this address, which is the only reason a new account is being created
			// from it at all — recording it as verified is the same fact, not a new claim.
			EmailVerifiedAt: timestamptz(s.now()),
		})
		if err != nil {
			if constraint := uniqueViolation(err); constraint != "" {
				if strings.Contains(constraint, "username") {
					return ErrUsernameTaken
				}
				// The address was claimed between the callback and now — by a password registration, or by
				// another provider flow.
				return ErrEmailTaken
			}
			return fmt.Errorf("creating account: %w", err)
		}
		user = created

		if _, err := q.CreateOAuthIdentity(ctx, db.CreateOAuthIdentityParams{
			ID:             int64(identityID),
			UserID:         created.ID,
			Provider:       string(identity.Provider),
			ProviderUserID: identity.UserID,
			Email:          identity.Email,
		}); err != nil {
			if uniqueViolation(err) != "" {
				// The same signup completed twice. The first one made the account; this transaction rolls
				// back and the caller is told to start again rather than being handed someone else's row.
				return ErrOAuthSignupToken
			}
			return fmt.Errorf("linking oauth identity: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	return s.issueOAuthExchangeCode(ctx, user.ID)
}

// ExchangeOAuthCode trades a one-time code for a token pair.
//
// This is where the device_id finally arrives: a browser does not have one, and the client that ends up
// holding the tokens is the thing that knows its own device identity (ADR 0011).
func (s *Service) ExchangeOAuthCode(ctx context.Context, rawCode string, in LoginInput) (TokenPair, error) {
	deviceID, err := normalizeDeviceID(in.DeviceID)
	if err != nil {
		return TokenPair{}, err
	}

	hash, err := ParseOAuthExchangeCode(rawCode)
	if err != nil {
		return TokenPair{}, ErrOAuthExchangeCode
	}

	code, err := s.queries.ConsumeOAuthExchangeCode(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, ErrOAuthExchangeCode
		}
		return TokenPair{}, fmt.Errorf("consuming oauth exchange code: %w", err)
	}

	// The same session machinery a password login uses, including superseding this device's previous
	// family — an OAuth sign-in is a login, and nothing about it should produce a different kind of
	// session.
	return s.startSession(ctx, snowflake.ID(code.UserID), deviceID, in.DeviceName, in.IP)
}

// issueOAuthExchangeCode mints the one-time code a client trades for tokens.
func (s *Service) issueOAuthExchangeCode(ctx context.Context, userID int64) (string, error) {
	raw, hash, err := GenerateOAuthExchangeCode()
	if err != nil {
		return "", err
	}
	id, err := s.ids.Next()
	if err != nil {
		return "", fmt.Errorf("generating exchange code ID: %w", err)
	}

	if _, err := s.queries.CreateOAuthExchangeCode(ctx, db.CreateOAuthExchangeCodeParams{
		ID:        int64(id),
		CodeHash:  hash,
		UserID:    userID,
		ExpiresAt: timestamptz(s.now().Add(OAuthExchangeCodeTTL)),
	}); err != nil {
		return "", fmt.Errorf("recording exchange code: %w", err)
	}
	return raw, nil
}

// suggestUsername offers a starting point for the username field.
//
// Only ever a suggestion: it is prefilled into a form the person edits and submits, never used on their
// behalf. That distinction is what keeps the email's local part out of a permanent public identifier
// unless they actively choose it.
func suggestUsername(identity OAuthIdentity) string {
	for _, candidate := range []string{identity.DisplayName, localPart(identity.Email)} {
		normalized := NormalizeUsername(candidate)
		// Spaces and punctuation are common in a display name and are not username characters, so a
		// filtered version is offered rather than nothing at all.
		filtered := filterToUsername(normalized)
		if ValidUsername(filtered) {
			return filtered
		}
	}
	return ""
}

// filterToUsername drops everything ValidUsername would reject, so a display name like "Ada Lovelace"
// becomes a usable suggestion rather than being discarded.
func filterToUsername(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case isUsernameRune(r):
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_.-")
}

// localPart returns the portion of an address before the @.
func localPart(email string) string {
	if at := strings.IndexByte(email, '@'); at > 0 {
		return email[:at]
	}
	return ""
}

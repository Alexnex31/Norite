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
	// ErrOAuthEmailUnverified is the whole linking rule in one refusal: the provider will not vouch for this
	// address, so it reaches no account — neither an existing one nor a new one.
	//
	// One error and one message for both cases, and that is the security-relevant part. Two messages is the
	// obvious design and it reports whether an address is registered to anyone who can present it
	// unverified at a provider, which GitHub permits for any address you care to type. So the message
	// carries both routes forward instead, and which one applies is the person's to know: they are the only
	// party to this exchange who already knows whether they have an account here.
	//
	// It stays long and specific rather than collapsing to something generic, because this is the one
	// refusal where the person genuinely owns both accounts and can act — told nothing useful, they press a
	// button that never works.
	ErrOAuthEmailUnverified = errors.New(
		"the provider has not verified this email address. Verify it with the provider and try again — " +
			"or, if you already have a Norite account using this address, sign in with your password and " +
			"link this provider from settings")

	// ErrOAuthIdentityLinkedElsewhere is this provider account already belonging to a different Norite
	// account — most often one that has since been deleted.
	ErrOAuthIdentityLinkedElsewhere = errors.New(
		"this provider account is already linked to a different Norite account")

	// ErrOAuthAccountAlreadyLinked is the mirror image: the Norite account this would link to already has a
	// different account at the same provider.
	ErrOAuthAccountAlreadyLinked = errors.New(
		"that account is already linked to a different account at this provider")

	// ErrOAuthFlowChallenge is a client starting a flow without a usable binding.
	//
	// Unlike most refusals in this package this one is meant to be legible to whoever is building the
	// client: it can only be reached by a caller that has not been written yet or has been written wrong,
	// never by an attacker doing something clever, so there is nothing to withhold.
	ErrOAuthFlowChallenge = errors.New(
		"flow_challenge must be the base64url-encoded SHA-256 of a flow verifier")

	// ErrOAuthExchangeCode covers an unknown, expired, already-spent, or wrongly-bound exchange code.
	ErrOAuthExchangeCode = errors.New("invalid or expired sign-in code")

	// ErrOAuthSignupToken covers a bad continuation token from the username step.
	ErrOAuthSignupToken = errors.New("this sign-up has expired; start again")

	// ErrOAuthRegistrationClosed is an invite-only instance refusing to create an account. Linking an
	// existing account is still allowed — the gate is on new accounts, not on providers.
	ErrOAuthRegistrationClosed = errors.New("registration on this instance requires an invite code")

	// ErrOAuthSignupForDevice is a device-flow sign-up presented at the JSON completion endpoint.
	//
	// That flow finishes on the verification page's approval step, which is a browser screen this endpoint
	// has no way to reach and no way to describe. Refused before anything is created rather than after:
	// the alternative was creating the account and answering 200 with an empty code, which the contract
	// says is required and which a client would then try to redeem.
	ErrOAuthSignupForDevice = errors.New(
		"this sign-up was started from a device verification page and has to be finished there")

	// ErrOAuthProviderDeclined is the provider reporting its own failure on the callback — someone pressing
	// "cancel" on the consent screen, almost always. An ordinary abandonment rather than a fault.
	ErrOAuthProviderDeclined = errors.New("the sign-in was not completed at the provider")
)

// OAuthOutcome is what happened at the callback.
type OAuthOutcome struct {
	// UserID is the account this flow resolved to, or zero when there is no account yet and a username
	// must be chosen. It is what SignedIn reports on.
	UserID int64

	// ExchangeCode is set when a client may now collect a token pair. Set on every signed-in flow except
	// a device one, which has no client waiting on a code — see DeviceCodeID.
	ExchangeCode string

	// DeviceCodeID is set when this flow was started from the device verification page, and it changes
	// where the callback ends: an approval page rather than a code or a redirect.
	//
	// No exchange code is minted on that path. The waiting client already holds a credential — its device
	// code — and issuing a second redeemable value for the same sign-in would mean one nobody collects,
	// sitting in the table until it expires.
	DeviceCodeID int64

	// SignupToken is set when there is no account yet and a username must be chosen. Mutually exclusive
	// with ExchangeCode.
	SignupToken string
	// SuggestedUsername is a starting point for the username field, never a value that gets used without
	// the person seeing it.
	SuggestedUsername string
	// Email is shown on the signup page so the person can see which account they are about to create.
	Email string

	// ClientRedirectURI is the loopback listener this flow was started with, or empty for a flow that
	// has nowhere to return to and gets a rendered page instead.
	//
	// Set on both branches above, not just the signed-in one: the signup branch has to carry it further
	// still, across the username form, or a client's sign-up would complete in a browser and never reach
	// the process waiting for it.
	ClientRedirectURI string
}

// SignedIn reports whether the flow resolved to an account rather than to a sign-up.
//
// On UserID rather than on ExchangeCode, which is what it used to read: a device flow signs somebody in
// and mints no code, so the two stopped meaning the same thing at M9.
func (o OAuthOutcome) SignedIn() bool { return o.UserID != 0 }

// StartOAuthInput is what a client supplies to begin a sign-in.
//
// A struct rather than three positional strings, and the reason is that they are all strings: a reordered
// argument list would still compile and would send the challenge where the redirect belongs. LoginInput
// beside it is the precedent, and M9 adds a fourth field here.
type StartOAuthInput struct {
	Provider      string
	FlowChallenge string
	// ClientRedirectURI is a loopback listener to return to instead of rendering the callback's page.
	// Optional: a browser has nowhere to be sent, and neither does a device flow.
	ClientRedirectURI string
	// DeviceToken is an entry continuation from the verification page, present exactly when this flow was
	// started by somebody finishing a device-code sign-in (M9).
	//
	// Mutually exclusive with FlowChallenge, and one of the two is required. That is not FlowChallenge
	// becoming optional — see StartOAuth.
	DeviceToken string
}

// StartOAuth begins an authorization request and returns the URL to send the user to.
//
// The flow challenge is required, not optional. An optional binding is not a binding: the attack it
// prevents is constructed by whoever starts the flow, so anyone who wanted to skip the check could simply
// start a flow without one.
func (s *Service) StartOAuth(ctx context.Context, in StartOAuthInput) (string, error) {
	provider, err := s.oauth.Get(in.Provider)
	if err != nil {
		return "", err
	}

	challenge, deviceCodeID, err := s.oauthFlowBinding(ctx, in)
	if err != nil {
		return "", err
	}

	// Validated before a state is minted or a row written. /authorize is unauthenticated, so a malformed
	// value should cost a parse and nothing else — no crypto/rand draw, no insert, no row for the sweeper.
	redirect, err := ParseOAuthClientRedirect(in.ClientRedirectURI)
	if err != nil {
		return "", err
	}
	if deviceCodeID != 0 && redirect != "" {
		// A device flow finishes on a page this instance renders, so there is no listener for it to be
		// returned to. Accepting both would mean a flow with two exits and a question about which wins.
		return "", ErrOAuthFlowChallenge
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
		ID:            int64(id),
		StateHash:     stateHash,
		Provider:      string(provider.Name()),
		CodeVerifier:  verifier,
		FlowChallenge: challenge,
		ExpiresAt:     timestamptz(s.now().Add(OAuthStateTTL)),
		// Canonical form, not what the client sent — see ParseOAuthClientRedirect.
		ClientRedirectUri: redirect,
		DeviceCodeID:      optionalID(deviceCodeID),
	}); err != nil {
		return "", fmt.Errorf("recording oauth state: %w", err)
	}

	return provider.AuthCodeURL(rawState, verifier), nil
}

// oauthFlowBinding resolves what this flow is bound to, and to what it will return.
//
// Exactly one of the two inputs, never both and never neither. A client redeeming a code presents a flow
// challenge; a browser finishing a device sign-in presents the continuation the verification page gave it.
// Requiring one of them keeps GenerateOAuthFlowVerifier's rule intact — an optional binding is not a
// binding, because whoever wanted to skip it would simply start a flow without one.
//
// # What is stored in flow_challenge on the device path, and why it is not a challenge
//
// The SHA-256 of the continuation, so the column stays NOT NULL and a state row can be traced back to the
// page visit that started it. Nothing verifies it later, and nothing should: there is no second party to
// bind on this path. A challenge exists because a code produced by a flow must be redeemable only by the
// client that began it — and a device flow produces no code. Somebody who stole this state and completed
// it in their own browser would be shown an approval page for their own provider account, authorizing the
// device they already control, which is not an attack. The risk on this path is a person being talked into
// approving, and that is what the approval page is for.
func (s *Service) oauthFlowBinding(ctx context.Context, in StartOAuthInput) (TokenHash, int64, error) {
	if (in.FlowChallenge == "") == (in.DeviceToken == "") {
		return nil, 0, ErrOAuthFlowChallenge
	}

	if in.DeviceToken != "" {
		entry, err := s.parseDeviceToken(in.DeviceToken, deviceEntryTokenType)
		if err != nil {
			return nil, 0, err
		}

		// The row is checked, not assumed, because the token outlives it. A continuation is good for ten
		// minutes from the moment a code is entered and a device code for twenty from the moment it is
		// issued, so one minted at minute nineteen is still valid after its row has expired and been
		// swept. Without this, that person's click on "Google" reached CreateOAuthState with a foreign key
		// pointing at nothing and got a 500 with a request ID — where the identical not-yet-swept case
		// gets "that sign-in step has expired; start again". Same action, two answers, decided by when the
		// sweeper last ran.
		if _, err := s.deviceCodeByID(ctx, entry.DeviceCodeID); err != nil {
			if errors.Is(err, ErrDeviceUserCode) {
				return nil, 0, ErrDeviceContinuation
			}
			return nil, 0, err
		}
		return HashToken(in.DeviceToken), entry.DeviceCodeID, nil
	}

	challenge, err := ParseOAuthFlowChallenge(in.FlowChallenge)
	if err != nil {
		return nil, 0, ErrOAuthFlowChallenge
	}
	return challenge, 0, nil
}

// optionalID renders a zero id as SQL NULL, which is what a flow with no device attached stores.
func optionalID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

// OAuthCallbackInput is what arrives on the provider's redirect back to this instance.
//
// A struct for the same reason StartOAuthInput is one: every field is a string, and the state and the
// authorization code are the two most consequential of them.
type OAuthCallbackInput struct {
	Provider string
	State    string
	Code     string
	// ProviderError is the provider's own `error` parameter, if it sent one.
	//
	// Handled here rather than in the handler so that the state row is consumed first and the client's
	// listener is therefore known. A declined consent that only rendered a page would leave a waiting CLI
	// looking hung until its own timeout, for the most ordinary outcome there is.
	ProviderError string
}

// CompleteOAuth handles the provider's callback.
func (s *Service) CompleteOAuth(ctx context.Context, in OAuthCallbackInput) (OAuthOutcome, error) {
	provider, err := s.oauth.Get(in.Provider)
	if err != nil {
		return OAuthOutcome{}, err
	}

	stateHash, err := ParseOAuthState(in.State)
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

	// Everything from here on knows where the client is waiting, so every failure is wrapped and can be
	// reported to it rather than only to a page nobody is reading.
	fail := func(err error) (OAuthOutcome, error) {
		return OAuthOutcome{}, &OAuthCallbackError{
			Err:               err,
			Code:              oauthErrorCodeFor(err),
			ClientRedirectURI: state.ClientRedirectUri,
		}
	}

	// The provider's own refusal, checked after the state is spent. Spending it is correct rather than
	// wasteful: the authorization code is gone either way and the person declined, so there is nothing
	// left to retry with this state.
	if in.ProviderError != "" {
		return fail(ErrOAuthProviderDeclined)
	}

	identity, err := provider.Identity(ctx, in.Code, state.CodeVerifier)
	if err != nil {
		return fail(err)
	}

	// The redirect comes from the row, never from in — the callback's own URL is written by whoever is
	// presenting it, and this value decides where a credential is delivered. Fixed when the flow started;
	// see the column's comment in 000006.
	outcome, err := s.resolveOAuthIdentity(ctx, identity, oauthDestination{
		Challenge:    state.FlowChallenge,
		Redirect:     state.ClientRedirectUri,
		DeviceCodeID: state.DeviceCodeID,
	})
	if err != nil {
		return fail(err)
	}
	return outcome, nil
}

// oauthDestination is where a completed flow is meant to end, read out of the state row the callback
// consumed and never out of the callback's own URL.
//
// A struct because there are now three of them and two are strings. Which one is set decides the shape of
// the answer: a loopback redirect, a device approval page, or the rendered code page every browser gets.
type oauthDestination struct {
	Challenge    TokenHash
	Redirect     string
	DeviceCodeID *int64
}

// forDevice reports whether this flow was started from the device verification page.
func (d oauthDestination) forDevice() bool { return d.DeviceCodeID != nil }

// resolveOAuthIdentity decides what an authenticated provider identity means for this instance.
func (s *Service) resolveOAuthIdentity(ctx context.Context, identity OAuthIdentity,
	dest oauthDestination,
) (OAuthOutcome, error) {
	log := logging.FromContext(ctx)

	// Already linked: the ordinary sign-in, and the only path that consults nothing but the provider's ID.
	existing, err := s.queries.GetOAuthIdentity(ctx, db.GetOAuthIdentityParams{
		Provider:       string(identity.Provider),
		ProviderUserID: identity.UserID,
	})
	if err == nil {
		return s.signedInOutcome(ctx, existing.UserID, dest)
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
			// The log is the one place these two cases are distinguishable, and deliberately so: an
			// operator investigating needs to know which happened, and a log line is not an answer to the
			// caller.
			log.Warn().
				Str("provider", string(identity.Provider)).
				Str("user_id", snowflake.ID(user.ID).String()).
				Msg("oauth sign-in refused: provider has not verified an address that matches an account")
			return OAuthOutcome{}, ErrOAuthEmailUnverified
		}

		if err := s.linkOAuthIdentity(ctx, user.ID, identity); err != nil {
			return OAuthOutcome{}, err
		}
		return s.signedInOutcome(ctx, user.ID, dest)

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
		token, err := s.issueOAuthSignupToken(identity, dest)
		if err != nil {
			return OAuthOutcome{}, err
		}
		return OAuthOutcome{
			SignupToken:       token,
			SuggestedUsername: suggestUsername(identity),
			Email:             email,
			ClientRedirectURI: dest.Redirect,
			DeviceCodeID:      derefID(dest.DeviceCodeID),
		}, nil

	default:
		return OAuthOutcome{}, fmt.Errorf("looking up account by email: %w", err)
	}
}

// signedInOutcome builds what the callback returns once a flow has resolved to an account.
//
// The split between the two shapes lives here rather than in the handler, because it is a statement about
// what was minted: a device flow ends on a page this instance renders and the person has still to approve,
// so there is no code, and minting one anyway would leave a redeemable value nobody collects.
func (s *Service) signedInOutcome(ctx context.Context, userID int64, dest oauthDestination,
) (OAuthOutcome, error) {
	if dest.forDevice() {
		return OAuthOutcome{UserID: userID, DeviceCodeID: *dest.DeviceCodeID}, nil
	}

	code, err := s.issueOAuthExchangeCode(ctx, userID, dest.Challenge)
	if err != nil {
		return OAuthOutcome{}, err
	}
	return OAuthOutcome{UserID: userID, ExchangeCode: code, ClientRedirectURI: dest.Redirect}, nil
}

// derefID reads an optional device-code id back out.
func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
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

// OAuthSignupResult is a completed sign-up: something redeemable, and where to deliver it.
type OAuthSignupResult struct {
	// UserID is the account that was just created.
	UserID int64
	// ExchangeCode is what the client trades for a token pair, or empty on a device flow — see
	// signedInOutcome for why nothing is minted there.
	ExchangeCode string
	// ClientRedirectURI is the listener the flow was started with, carried across the form inside the
	// continuation token. Empty for a flow that named none.
	ClientRedirectURI string
	// DeviceCodeID is the authorization waiting on this sign-up, or zero. Also carried inside the
	// signature rather than in the form.
	DeviceCodeID int64
}

// CompleteOAuthSignup creates the account a signup token stands for, once a username has been chosen.
func (s *Service) CompleteOAuthSignup(ctx context.Context, signupToken, rawUsername string,
) (OAuthSignupResult, error) {
	continuation, err := s.parseOAuthSignupToken(signupToken)
	if err != nil {
		// Before the token parsed there is no redirect to know, so this one cannot be reported to a
		// listener — the same boundary CompleteOAuth has around state consumption.
		return OAuthSignupResult{}, err
	}
	identity := continuation.Identity
	dest := oauthDestination{
		Challenge:    continuation.Challenge,
		Redirect:     continuation.ClientRedirectURI,
		DeviceCodeID: optionalID(continuation.DeviceCodeID),
	}

	// Failures from here on know where a client is waiting, and the ones that end the flow say so. A
	// username that can simply be retyped is deliberately *not* wrapped: the form re-renders, the person
	// fixes it, and the listener is still waiting for the eventual success.
	fail := func(err error) (OAuthSignupResult, error) {
		return OAuthSignupResult{}, &OAuthCallbackError{
			Err:               err,
			Code:              oauthErrorCodeFor(err),
			ClientRedirectURI: continuation.ClientRedirectURI,
		}
	}

	username := NormalizeUsername(rawUsername)
	if !ValidUsername(username) {
		return OAuthSignupResult{}, ErrInvalidUsername
	}

	// Re-checked here rather than trusted from the callback: the token is valid for half an hour, and the
	// instance could have been switched to invite-only in between. That is exactly the window in which a
	// waiting client would otherwise sit out its full timeout for a decision already made.
	if s.registrationMode != RegistrationOpen {
		return fail(ErrOAuthRegistrationClosed)
	}

	email := strings.TrimSpace(strings.ToLower(identity.Email))

	userID, err := s.ids.Next()
	if err != nil {
		return fail(fmt.Errorf("generating user ID: %w", err))
	}
	identityID, err := s.ids.Next()
	if err != nil {
		return fail(fmt.Errorf("generating oauth identity ID: %w", err))
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
		// ErrUsernameTaken is the one the form can still recover from, so it stays unwrapped and
		// re-renders. Everything else here — the address claimed since the callback, a second submission
		// of the same signup, a database failure — ends the flow in the browser, and a client waiting on a
		// listener has to be told rather than left to time out.
		if errors.Is(err, ErrUsernameTaken) {
			return OAuthSignupResult{}, err
		}
		return fail(err)
	}

	// A device sign-up mints no code, exactly as a device sign-in does not: the person still has to
	// approve, and the waiting client already holds the credential it will redeem.
	if dest.forDevice() {
		return OAuthSignupResult{UserID: user.ID, DeviceCodeID: continuation.DeviceCodeID}, nil
	}

	code, err := s.issueOAuthExchangeCode(ctx, user.ID, dest.Challenge)
	if err != nil {
		return fail(err)
	}
	return OAuthSignupResult{
		UserID:            user.ID,
		ExchangeCode:      code,
		ClientRedirectURI: continuation.ClientRedirectURI,
	}, nil
}

// ExchangeOAuthCode trades a one-time code for a token pair.
//
// This is where the device_id finally arrives: a browser does not have one, and the client that ends up
// holding the tokens is the thing that knows its own device identity (ADR 0011).
func (s *Service) ExchangeOAuthCode(ctx context.Context, rawCode, rawVerifier string,
	in LoginInput,
) (TokenPair, error) {
	deviceID, err := normalizeDeviceID(in.DeviceID)
	if err != nil {
		return TokenPair{}, err
	}

	hash, err := ParseOAuthExchangeCode(rawCode)
	if err != nil {
		return TokenPair{}, ErrOAuthExchangeCode
	}

	// Shape-checked before the code is spent, so a client with a bug does not burn its own sign-in on a
	// malformed verifier. An attacker gains nothing from the ordering: they do not hold a verifier of any
	// shape, and reaching the comparison below is what they cannot do.
	presented, err := ParseOAuthFlowVerifier(rawVerifier)
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

	// The binding check, and the reason this whole parameter exists: this code is redeemable only by the
	// client that started the flow it came from.
	//
	// Checked after the code is spent, deliberately. A mismatch is the login-CSRF attempt itself — someone
	// redeeming a code produced by a flow they did not begin — and burning the code as it fails is the
	// outcome to want: the attacker's code dies in the victim's hands rather than staying live for a
	// second attempt.
	//
	// Reported as an ordinary bad code. The client is told the same thing for unknown, expired, spent and
	// unbound, exactly as everywhere else in this package; the distinction is worth a log line here because
	// it is the one that means somebody is being attacked.
	if !presented.Equal(code.FlowChallenge) {
		logging.FromContext(ctx).Warn().
			Msg("oauth exchange refused: the code was not issued to the client redeeming it")
		return TokenPair{}, ErrOAuthExchangeCode
	}

	// The same session machinery a password login uses, including superseding this device's previous
	// family — an OAuth sign-in is a login, and nothing about it should produce a different kind of
	// session.
	return s.startSession(ctx, snowflake.ID(code.UserID), deviceID, in.DeviceName, in.IP)
}

// issueOAuthExchangeCode mints the one-time code a client trades for tokens.
func (s *Service) issueOAuthExchangeCode(ctx context.Context, userID int64,
	challenge TokenHash,
) (string, error) {
	raw, hash, err := GenerateOAuthExchangeCode()
	if err != nil {
		return "", err
	}
	id, err := s.ids.Next()
	if err != nil {
		return "", fmt.Errorf("generating exchange code ID: %w", err)
	}

	if _, err := s.queries.CreateOAuthExchangeCode(ctx, db.CreateOAuthExchangeCodeParams{
		ID:            int64(id),
		CodeHash:      hash,
		UserID:        userID,
		FlowChallenge: challenge,
		ExpiresAt:     timestamptz(s.now().Add(OAuthExchangeCodeTTL)),
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

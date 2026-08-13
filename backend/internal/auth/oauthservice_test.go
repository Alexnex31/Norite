package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oauthService builds a service wired to a stub provider, so a flow can be driven end to end without
// Google or GitHub — and, more to the point, with an unverified email on demand, which is the case the
// whole linking rule turns on and which no real provider will produce for a test.
func oauthService(t *testing.T, mode RegistrationMode) (*Service, *stubProvider) {
	t.Helper()

	svc, _ := newService(t, mode)
	stub := newStubProvider(t)
	svc.oauth = stub.providers()
	svc.publicBaseURL = "https://chat.example.com"
	return svc, stub
}

// signIn drives /authorize and /callback against the stub and returns the outcome.
func signIn(t *testing.T, svc *Service, stub *stubProvider, provider string) (OAuthOutcome, error) {
	t.Helper()

	authURL, err := svc.StartOAuth(t.Context(), provider)
	require.NoError(t, err)

	state := stateFromURL(t, authURL)
	return svc.CompleteOAuth(t.Context(), provider, state, "authorization-code")
}

// stateFromURL pulls the state parameter back out of the authorize URL, which is the only place it exists
// in raw form — the database holds only its hash.
func stateFromURL(t *testing.T, authURL string) string {
	t.Helper()
	_, after, found := strings.Cut(authURL, "state=")
	require.True(t, found, "the authorize URL must carry a state parameter: %s", authURL)
	state, _, _ := strings.Cut(after, "&")
	require.NotEmpty(t, state)
	return state
}

// asGoogle points the stub at a Google identity.
func (s *stubProvider) asGoogle(sub, email string, verified bool) {
	s.googleInfo = map[string]any{
		"sub": sub, "email": email, "email_verified": verified, "name": "Ada Lovelace",
	}
}

// ---------- the linking rule ----------

// The decision this milestone turns on. A provider that has verified the address is asserting the person
// controls it, which is the same evidence a password login rests on. A provider that has not is asserting
// nothing — anyone can put someone else's address on an account at a provider that never checks.
func TestOAuthLinksToAnExistingAccountOnlyWhenVerified(t *testing.T) {
	t.Run("verified links and signs in", func(t *testing.T) {
		svc, stub := oauthService(t, RegistrationOpen)
		user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
		stub.asGoogle("google-1", "ada@example.com", true)

		outcome, err := signIn(t, svc, stub, "google")
		require.NoError(t, err)
		require.True(t, outcome.SignedIn(), "a verified match must sign in, not start a signup")

		pair, err := svc.ExchangeOAuthCode(t.Context(), outcome.ExchangeCode, LoginInput{DeviceID: "laptop"})
		require.NoError(t, err)
		assert.NotEmpty(t, pair.AccessToken)

		// ...and the tokens belong to the account that already existed, not a new one.
		actor, err := svc.AuthenticateAccessToken(t.Context(), pair.AccessToken)
		require.NoError(t, err)
		assert.Equal(t, user.ID, int64(actor.UserID))
	})

	t.Run("unverified is refused with something actionable", func(t *testing.T) {
		svc, stub := oauthService(t, RegistrationOpen)
		registerAndLogin(t, svc, "ada@example.com", "laptop")
		stub.asGoogle("google-1", "ada@example.com", false)

		_, err := signIn(t, svc, stub, "google")
		require.ErrorIs(t, err, ErrOAuthLinkRequired,
			"an unverified address matching an account is an account-takeover attempt until proven otherwise")

		// Nothing was linked, so a later verified attempt still has to do the linking itself.
		var links int
		require.NoError(t, svc.pool.QueryRow(t.Context(),
			"SELECT count(*) FROM oauth_identities").Scan(&links))
		assert.Zero(t, links, "a refused sign-in must not leave a link behind")
	})
}

// Once linked, sign-in consults only the provider's user ID — the email is not re-checked, so changing it
// at the provider cannot detach or redirect an existing link.
func TestASecondSignInUsesTheLinkNotTheEmail(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	stub.asGoogle("google-1", "ada@example.com", true)
	first, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)
	require.True(t, first.SignedIn())

	// The same provider account, now reporting a different and unverified address.
	stub.asGoogle("google-1", "moved@example.com", false)
	second, err := signIn(t, svc, stub, "google")
	require.NoError(t, err, "an established link must keep working regardless of the address")
	require.True(t, second.SignedIn())

	pair, err := svc.ExchangeOAuthCode(t.Context(), second.ExchangeCode, LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)
	actor, err := svc.AuthenticateAccessToken(t.Context(), pair.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, user.ID, int64(actor.UserID))
}

// ---------- signup ----------

// Nothing is written to users until a username is chosen. That is the whole reason the continuation token
// exists rather than a pending account row.
func TestSignupCreatesNothingUntilAUsernameIsChosen(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	stub.asGoogle("google-99", "newcomer@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	require.False(t, outcome.SignedIn(), "there is no account yet, so there is nothing to sign in to")
	require.NotEmpty(t, outcome.SignupToken)
	assert.Equal(t, "newcomer@example.com", outcome.Email)

	// The database is untouched: no half-made account, no identity row.
	var users, identities int
	require.NoError(t, svc.pool.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&users))
	require.NoError(t, svc.pool.QueryRow(t.Context(), "SELECT count(*) FROM oauth_identities").Scan(&identities))
	assert.Zero(t, users, "an unfinished signup must leave no account behind")
	assert.Zero(t, identities)

	// Completing it creates account and link together.
	code, err := svc.CompleteOAuthSignup(t.Context(), outcome.SignupToken, "newcomer")
	require.NoError(t, err)

	pair, err := svc.ExchangeOAuthCode(t.Context(), code, LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)

	actor, err := svc.AuthenticateAccessToken(t.Context(), pair.AccessToken)
	require.NoError(t, err)

	created, err := svc.GetUser(t.Context(), actor.UserID)
	require.NoError(t, err)
	assert.Equal(t, "newcomer", created.Username)
	assert.Equal(t, "newcomer@example.com", created.Email)
	assert.Nil(t, created.PasswordHash, "an OAuth account has no password, not an empty one")
	assert.True(t, created.EmailVerifiedAt.Valid, "the provider verified it; that is the same fact")
}

// The username goes through exactly the rule password registration uses. Anything looser would make the
// provider a way around it.
func TestSignupAppliesTheUsernameRule(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	stub.asGoogle("google-99", "newcomer@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	for name, username := range map[string]string{
		"contains a space":  "ada lovelace",
		"contains a tab":    "ada\tlovelace",
		"bidi override":     "ada\u202elovelace",
		"too short":         "a",
		"disallowed symbol": "ada+bot",
		"empty":             "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CompleteOAuthSignup(t.Context(), outcome.SignupToken, username)
			assert.ErrorIs(t, err, ErrInvalidUsername)
		})
	}

	// ...and a normalized one is accepted and stored in its normalized form.
	code, err := svc.CompleteOAuthSignup(t.Context(), outcome.SignupToken, "ﬁnn")
	require.NoError(t, err)

	pair, err := svc.ExchangeOAuthCode(t.Context(), code, LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)
	actor, _ := svc.AuthenticateAccessToken(t.Context(), pair.AccessToken)
	created, err := svc.GetUser(t.Context(), actor.UserID)
	require.NoError(t, err)
	assert.Equal(t, "finn", created.Username, "NFKC applies here exactly as it does at registration")
}

func TestSignupRefusesATakenUsername(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-99", "newcomer@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	_, err = svc.CompleteOAuthSignup(t.Context(), outcome.SignupToken, "ada")
	assert.ErrorIs(t, err, ErrUsernameTaken)
}

// An invite-only instance refuses to create accounts through a provider, exactly as it refuses password
// registration — but linking an existing account is still allowed, because the gate is on new accounts.
func TestInviteOnlyRefusesSignupButPermitsLinking(t *testing.T) {
	t.Run("new account is refused", func(t *testing.T) {
		svc, stub := oauthService(t, RegistrationInvite)
		stub.asGoogle("google-99", "newcomer@example.com", true)

		_, err := signIn(t, svc, stub, "google")
		assert.ErrorIs(t, err, ErrOAuthRegistrationClosed)
	})

	t.Run("existing account still links", func(t *testing.T) {
		svc, stub := oauthService(t, RegistrationInvite)

		// Registration is closed, so the account is made directly — the state an invite-only instance
		// reaches once someone has been let in.
		svc.registrationMode = RegistrationOpen
		registerAndLogin(t, svc, "ada@example.com", "laptop")
		svc.registrationMode = RegistrationInvite

		stub.asGoogle("google-1", "ada@example.com", true)
		outcome, err := signIn(t, svc, stub, "google")
		require.NoError(t, err, "closing registration must not lock existing accounts out of their providers")
		assert.True(t, outcome.SignedIn())
	})
}

// ---------- state and codes ----------

// A callback replayed — a refresh, a back button, someone who captured the redirect — must not reach the
// provider a second time with the same verifier.
func TestAStateIsSingleUse(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	authURL, err := svc.StartOAuth(t.Context(), "google")
	require.NoError(t, err)
	state := stateFromURL(t, authURL)

	_, err = svc.CompleteOAuth(t.Context(), "google", state, "code")
	require.NoError(t, err)

	_, err = svc.CompleteOAuth(t.Context(), "google", state, "code")
	assert.ErrorIs(t, err, ErrOAuthState, "a spent state must not start a second exchange")
}

// A state issued for one provider must not be presentable at another's callback, or a code would be
// exchanged against the wrong client.
func TestAStateIsBoundToItsProvider(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	stub.githubUser = map[string]any{"id": 7, "login": "ada"}
	stub.githubEmails = []map[string]any{{"email": "ada@example.com", "primary": true, "verified": true}}

	authURL, err := svc.StartOAuth(t.Context(), "google")
	require.NoError(t, err)

	_, err = svc.CompleteOAuth(t.Context(), "github", stateFromURL(t, authURL), "code")
	assert.ErrorIs(t, err, ErrOAuthState)
}

func TestUnknownStatesAreRefused(t *testing.T) {
	svc, _ := oauthService(t, RegistrationOpen)

	for name, state := range map[string]string{
		"empty":        "",
		"garbage":      "not-a-state",
		"wrong prefix": "nrp_" + strings.Repeat("A", 43),
		"never issued": "nos_" + strings.Repeat("A", 43),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CompleteOAuth(t.Context(), "google", state, "code")
			assert.ErrorIs(t, err, ErrOAuthState)
		})
	}
}

// The exchange code is what crosses the browser, so it has to be worthless the second time.
func TestAnExchangeCodeIsSingleUse(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	_, err = svc.ExchangeOAuthCode(t.Context(), outcome.ExchangeCode, LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)

	_, err = svc.ExchangeOAuthCode(t.Context(), outcome.ExchangeCode, LoginInput{DeviceID: "laptop"})
	assert.ErrorIs(t, err, ErrOAuthExchangeCode)
}

func TestExchangeRequiresADeviceID(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	// Same requirement as a password login: a session with no device identity would share a refresh family
	// with every other such session, and rotation would log them all out (ADR 0011).
	_, err = svc.ExchangeOAuthCode(t.Context(), outcome.ExchangeCode, LoginInput{})
	assert.Error(t, err)
}

// ---------- the signup token ----------

// The `typ` claim is what stops a token minted for one purpose being spent at another. Without it an
// ordinary access token — signed with the same key — would be accepted as a signup for an account of the
// holder's choosing.
func TestAnAccessTokenIsNotASignupToken(t *testing.T) {
	svc, _ := oauthService(t, RegistrationOpen)
	_, pair := registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := svc.CompleteOAuthSignup(t.Context(), pair.AccessToken, "someone")
	assert.ErrorIs(t, err, ErrOAuthSignupToken,
		"an access token must not be spendable as a signup continuation")
}

func TestSignupTokensAreRejectedWhenTampered(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	stub.asGoogle("google-99", "newcomer@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	// A token signed by a different key, otherwise well-formed.
	forger, err := NewTokenIssuer([]byte("a-different-key-of-at-least-32-bytes"))
	require.NoError(t, err)
	other := *svc
	other.issuer = forger
	forged, err := other.issueOAuthSignupToken(OAuthIdentity{
		Provider: ProviderGoogle, UserID: "google-1", Email: "victim@example.com", EmailVerified: true,
	})
	require.NoError(t, err)

	for name, token := range map[string]string{
		"foreign key": forged,
		"garbage":     "not-a-token",
		"empty":       "",
		"truncated":   outcome.SignupToken[:len(outcome.SignupToken)-4],
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CompleteOAuthSignup(t.Context(), token, "someone")
			assert.ErrorIs(t, err, ErrOAuthSignupToken)
		})
	}
}

// A signup token cannot authenticate an ordinary request either — the same check in the other direction.
func TestASignupTokenIsNotAnAccessToken(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	stub.asGoogle("google-99", "newcomer@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	_, err = svc.AuthenticateAccessToken(t.Context(), outcome.SignupToken)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// ---------- the username suggestion ----------

// Only ever a suggestion: it is prefilled into a form the person edits. That is what keeps an email's
// local part out of a permanent public identifier unless they actively choose it.
func TestUsernameSuggestions(t *testing.T) {
	cases := map[string]struct {
		identity OAuthIdentity
		want     string
	}{
		"display name wins": {
			identity: OAuthIdentity{DisplayName: "Ada Lovelace", Email: "ada.l@example.com"},
			want:     "Ada_Lovelace",
		},
		"falls back to the email local part": {
			identity: OAuthIdentity{Email: "ada.lovelace@example.com"},
			want:     "ada.lovelace",
		},
		"strips characters the rule rejects": {
			// The space still becomes an underscore; only the characters with no sensible mapping are
			// dropped, so the suggestion stays recognizable as the name it came from.
			identity: OAuthIdentity{DisplayName: "Ada (Lovelace)!", Email: "a@b.co"},
			want:     "Ada_Lovelace",
		},
		"non-latin survives": {
			identity: OAuthIdentity{DisplayName: "田中", Email: "t@example.com"},
			want:     "田中",
		},
		"nothing usable": {
			identity: OAuthIdentity{DisplayName: "!!!", Email: "!@example.com"},
			want:     "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := suggestUsername(tc.identity)
			assert.Equal(t, tc.want, got)
			if got != "" {
				assert.True(t, ValidUsername(got), "a suggestion the form would reject is worse than none")
			}
		})
	}
}

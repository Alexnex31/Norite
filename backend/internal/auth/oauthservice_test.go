package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

// ---------- account lifecycle ----------

// A soft-deleted account keeps its rows so authored content still renders as "Deleted User" — and that
// includes its oauth_identities row. Without the join in GetOAuthIdentity, a deleted account signed
// straight back in and collected a token pair, while password login and API tokens both refused it.
func TestASoftDeletedAccountCannotSignInThroughAProvider(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	linked, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)
	require.True(t, linked.SignedIn())

	_, err = svc.pool.Exec(t.Context(), "UPDATE users SET deleted_at = now() WHERE id = $1", user.ID)
	require.NoError(t, err)

	// The identity row still exists, so this is entirely a question of whether the lookup checks.
	outcome, err := signIn(t, svc, stub, "google")
	if err == nil {
		require.False(t, outcome.SignedIn(),
			"a deleted account must not be signed back in by its surviving provider link")
	}

	// It falls through to the signup path and fails there on the address, which the deleted row still
	// holds — the same answer password registration gives for a deleted account's email.
	if outcome.SignupToken != "" {
		_, err = svc.CompleteOAuthSignup(t.Context(), outcome.SignupToken, "reborn")
		assert.ErrorIs(t, err, ErrEmailTaken)
	}
}

// ---------- expiry ----------

// Every consumable value in this flow has a TTL enforced in SQL, and nothing tested any of them: the
// single-use tests all spend a fresh value.
func TestOAuthValuesExpire(t *testing.T) {
	t.Run("state", func(t *testing.T) {
		svc, stub := oauthService(t, RegistrationOpen)
		registerAndLogin(t, svc, "ada@example.com", "laptop")
		stub.asGoogle("google-1", "ada@example.com", true)

		authURL, err := svc.StartOAuth(t.Context(), "google")
		require.NoError(t, err)

		// Age the row rather than moving a clock: the WHERE clause compares against the database's now(),
		// so a service-clock override would satisfy nothing.
		_, err = svc.pool.Exec(t.Context(),
			"UPDATE oauth_states SET expires_at = now() - interval '1 minute'")
		require.NoError(t, err)

		_, err = svc.CompleteOAuth(t.Context(), "google", stateFromURL(t, authURL), "code")
		assert.ErrorIs(t, err, ErrOAuthState)
	})

	t.Run("exchange code", func(t *testing.T) {
		svc, stub := oauthService(t, RegistrationOpen)
		registerAndLogin(t, svc, "ada@example.com", "laptop")
		stub.asGoogle("google-1", "ada@example.com", true)

		outcome, err := signIn(t, svc, stub, "google")
		require.NoError(t, err)

		_, err = svc.pool.Exec(t.Context(),
			"UPDATE oauth_exchange_codes SET expires_at = now() - interval '1 minute'")
		require.NoError(t, err)

		_, err = svc.ExchangeOAuthCode(t.Context(), outcome.ExchangeCode, LoginInput{DeviceID: "laptop"})
		assert.ErrorIs(t, err, ErrOAuthExchangeCode)
	})

	t.Run("signup token", func(t *testing.T) {
		svc, _ := oauthService(t, RegistrationOpen)

		// Minted directly rather than by driving a sign-in. The token is signed rather than stored, so its
		// expiry is a claim and the service clock is what ages it — but that same clock sets the state
		// row's expires_at, which the database compares against its own now(). Aging the clock around a
		// whole sign-in would therefore expire the state first and never reach the token at all.
		svc.now = func() time.Time { return time.Now().Add(-2 * OAuthSignupTTL) }
		expired, err := svc.issueOAuthSignupToken(OAuthIdentity{
			Provider: ProviderGoogle, UserID: "google-99",
			Email: "newcomer@example.com", EmailVerified: true,
		})
		require.NoError(t, err)
		svc.now = time.Now

		_, err = svc.CompleteOAuthSignup(t.Context(), expired, "newcomer")
		assert.ErrorIs(t, err, ErrOAuthSignupToken)
	})
}

// ---------- concurrency ----------

// Two callbacks racing one state — a double-clicked link, or a browser that prefetches — must produce
// exactly one exchange. The guard is ConsumeOAuthState's WHERE clause, so this holds without the service
// coordinating anything.
func TestConcurrentCallbacksConsumeTheStateOnce(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	authURL, err := svc.StartOAuth(t.Context(), "google")
	require.NoError(t, err)
	state := stateFromURL(t, authURL)

	const racers = 4
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = svc.CompleteOAuth(context.Background(), "google", state, "code")
		}()
	}
	close(start)
	wg.Wait()

	var succeeded int
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			assert.ErrorIs(t, err, ErrOAuthState)
		}
	}
	assert.Equal(t, 1, succeeded, "exactly one callback may spend the state")
}

// Two signups racing one continuation token must create exactly one account. Nothing coordinates this
// either: oauth_identities' unique constraint is what refuses the second, which is the whole reason the
// token can be signed rather than stored.
func TestConcurrentSignupsCreateOneAccount(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	stub.asGoogle("google-99", "newcomer@example.com", true)

	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)

	const racers = 4
	var wg sync.WaitGroup
	errs := make([]error, racers)
	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = svc.CompleteOAuthSignup(context.Background(), outcome.SignupToken, "newcomer")
		}()
	}
	close(start)
	wg.Wait()

	var succeeded int
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	assert.Equal(t, 1, succeeded, "exactly one signup may create the account")

	var users int
	require.NoError(t, svc.pool.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&users))
	assert.Equal(t, 1, users, "and the database must agree")
}

// ---------- more than one provider ----------

// An account may link both providers. The UNIQUE (user_id, provider) constraint allows one row each, and
// signing in with either must reach the same account.
func TestAnAccountCanLinkBothProviders(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	stub.asGoogle("google-1", "ada@example.com", true)
	viaGoogle, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)
	require.True(t, viaGoogle.SignedIn())

	stub.githubUser = map[string]any{"id": 4242, "login": "ada"}
	stub.githubEmails = []map[string]any{{"email": "ada@example.com", "primary": true, "verified": true}}
	viaGitHub, err := signIn(t, svc, stub, "github")
	require.NoError(t, err)
	require.True(t, viaGitHub.SignedIn())

	// Both codes resolve to the same account.
	for _, code := range []string{viaGoogle.ExchangeCode, viaGitHub.ExchangeCode} {
		pair, err := svc.ExchangeOAuthCode(t.Context(), code, LoginInput{DeviceID: "laptop"})
		require.NoError(t, err)
		actor, err := svc.AuthenticateAccessToken(t.Context(), pair.AccessToken)
		require.NoError(t, err)
		assert.Equal(t, user.ID, int64(actor.UserID))
	}

	var links int
	require.NoError(t, svc.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM oauth_identities WHERE user_id = $1", user.ID).Scan(&links))
	assert.Equal(t, 2, links)
}

// A GitHub sign-in end to end through the service, not just the provider layer — the two providers differ
// enough in how they report an address that only exercising one of them proves less than it looks.
func TestGitHubSignsInThroughTheService(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	stub.githubUser = map[string]any{"id": 4242, "login": "ada", "name": "Ada Lovelace"}
	stub.githubEmails = []map[string]any{
		{"email": "private@example.com", "primary": true, "verified": false},
		{"email": "ada@example.com", "primary": false, "verified": true},
	}

	outcome, err := signIn(t, svc, stub, "github")
	require.NoError(t, err)
	require.NotEmpty(t, outcome.SignupToken, "a new GitHub identity starts a signup")
	assert.Equal(t, "ada@example.com", outcome.Email,
		"the verified address is what the account is created from, not the unverified primary")

	code, err := svc.CompleteOAuthSignup(t.Context(), outcome.SignupToken, "ada")
	require.NoError(t, err)

	pair, err := svc.ExchangeOAuthCode(t.Context(), code, LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)
	actor, err := svc.AuthenticateAccessToken(t.Context(), pair.AccessToken)
	require.NoError(t, err)

	created, err := svc.GetUser(t.Context(), actor.UserID)
	require.NoError(t, err)
	assert.Equal(t, "ada@example.com", created.Email)
}

// ---------- cleanup ----------

// The sweeps M11 will schedule. Untested code that deletes rows is worth exercising before something
// schedules it: a WHERE clause that is one character wrong here empties a table.
func TestExpiredRowsAreSweptAndLiveOnesAreNot(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	// One live flow, and one of each kind aged past its expiry.
	outcome, err := signIn(t, svc, stub, "google")
	require.NoError(t, err)
	require.NotEmpty(t, outcome.ExchangeCode)

	_, err = svc.StartOAuth(t.Context(), "google") // a second, still-live state
	require.NoError(t, err)
	_, err = svc.pool.Exec(t.Context(),
		"UPDATE oauth_states SET expires_at = now() - interval '1 hour' WHERE consumed_at IS NOT NULL")
	require.NoError(t, err)

	swept, err := svc.queries.DeleteExpiredOAuthStates(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), swept, "only the expired state may be removed")

	var remaining int
	require.NoError(t, svc.pool.QueryRow(t.Context(), "SELECT count(*) FROM oauth_states").Scan(&remaining))
	assert.Equal(t, 1, remaining, "the live flow must survive the sweep")

	// Exchange codes, same shape.
	_, err = svc.pool.Exec(t.Context(),
		"UPDATE oauth_exchange_codes SET expires_at = now() - interval '1 hour'")
	require.NoError(t, err)
	sweptCodes, err := svc.queries.DeleteExpiredOAuthExchangeCodes(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), sweptCodes)
}

package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

const testSigningKey = "test-signing-key-of-at-least-32-bytes"

// ---------- passwords ----------

func TestHashAndVerifyPassword(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(t.Context(), password)
	require.NoError(t, err)

	assert.NotContains(t, hash, password, "the hash must not contain the password")
	assert.True(t, strings.HasPrefix(hash, "$argon2id$"), "must be an argon2id encoded hash, got %q", hash)

	assert.NoError(t, VerifyPassword(t.Context(), hash, password))
	assert.ErrorIs(t, VerifyPassword(t.Context(), hash, "wrong password entirely"), ErrInvalidCredentials)
}

func TestHashingTheSamePasswordTwiceGivesDifferentHashes(t *testing.T) {
	const password = "correct horse battery staple"

	first, err := HashPassword(t.Context(), password)
	require.NoError(t, err)
	second, err := HashPassword(t.Context(), password)
	require.NoError(t, err)

	// Distinct salts. Identical hashes would let anyone with the table see which accounts share a password.
	assert.NotEqual(t, first, second)
	assert.NoError(t, VerifyPassword(t.Context(), first, password))
	assert.NoError(t, VerifyPassword(t.Context(), second, password))
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"too short", strings.Repeat("a", MinPasswordLength-1), ErrPasswordTooShort},
		{"at the floor", strings.Repeat("a", MinPasswordLength), nil},
		{"at the ceiling", strings.Repeat("a", MaxPasswordLength), nil},
		{"too long", strings.Repeat("a", MaxPasswordLength+1), ErrPasswordTooLong},
		// Counted in runes, not bytes: a 12-character passphrase in a non-Latin script is 12 characters,
		// and rejecting it while accepting 12 ASCII letters would be both wrong and quietly discriminatory.
		{"non-Latin at the floor", strings.Repeat("パ", MinPasswordLength), nil},
		{"non-Latin below the floor", strings.Repeat("パ", MinPasswordLength-1), ErrPasswordTooShort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// An account with no password must not be loginable, and must not be *distinguishable* from a wrong
// password either — "this address signs in with Google" turns a login form into an account-discovery tool.
func TestVerifyPasswordOnAnAccountWithNoPassword(t *testing.T) {
	assert.ErrorIs(t, VerifyPassword(t.Context(), "", "anything at all"), ErrPasswordNotSet)
}

func TestVerifyPasswordForMissingUserAlwaysFails(t *testing.T) {
	assert.ErrorIs(t, VerifyPasswordForMissingUser(t.Context(), "anything"), ErrInvalidCredentials)
}

// The timing-equalizer only works if the missing-user path costs about the same as a real verification.
// Without it, the difference between an immediate return and ~50ms of argon2id enumerates the user base.
func TestMissingUserVerificationCostsTheSameAsARealOne(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement is too noisy to be worth running in -short mode")
	}

	hash, err := HashPassword(t.Context(), "correct horse battery staple")
	require.NoError(t, err)

	measure := func(f func()) time.Duration {
		start := time.Now()
		for range 3 {
			f()
		}
		return time.Since(start) / 3
	}

	real := measure(func() { _ = VerifyPassword(t.Context(), hash, "wrong password") })
	missing := measure(func() { _ = VerifyPasswordForMissingUser(t.Context(), "wrong password") })

	t.Logf("real verification %s, missing-user path %s", real, missing)

	// A generous bound: this asserts the missing-user path does real work, not that the two match to the
	// microsecond. A regression that skipped hashing entirely would come in orders of magnitude faster.
	require.Positive(t, real)
	ratio := float64(missing) / float64(real)
	assert.Greater(t, ratio, 0.25, "the missing-user path is suspiciously fast — is it still hashing?")
}

// ---------- opaque tokens ----------

func TestGeneratedTokensAreDistinctAndWellFormed(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for range 200 {
		raw, hash, err := GenerateRefreshToken()
		require.NoError(t, err)

		assert.True(t, strings.HasPrefix(raw, "nrt_"), "refresh tokens carry an identifiable prefix: %q", raw)
		assert.Len(t, hash, 32, "the stored form is a SHA-256")
		assert.NotContains(t, string(hash), raw, "the stored hash must not contain the raw token")

		_, duplicate := seen[raw]
		require.False(t, duplicate, "generated a duplicate token")
		seen[raw] = struct{}{}
	}
}

func TestTokenPrefixesDistinguishTheTwoKinds(t *testing.T) {
	refresh, _, err := GenerateRefreshToken()
	require.NoError(t, err)
	api, _, err := GenerateAPIToken()
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(api, "nat_"))
	assert.NotEqual(t, refresh[:4], api[:4], "the two kinds must be distinguishable on sight")

	// The middleware routes a credential to the right verifier by prefix rather than trying both.
	assert.True(t, LooksLikeOpaqueToken(refresh))
	assert.True(t, LooksLikeOpaqueToken(api))
	assert.False(t, LooksLikeOpaqueToken("eyJhbGciOiJIUzI1NiJ9.e30.signature"), "a JWT is not an opaque token")
}

func TestParseTokenRejectsTheWrongKind(t *testing.T) {
	refresh, _, err := GenerateRefreshToken()
	require.NoError(t, err)

	// A refresh token must not authenticate as an API token, even though both are well-formed opaque
	// tokens of the same length — the prefix is what keeps the two credential types from being confused.
	_, err = ParseAPIToken(refresh)
	assert.ErrorIs(t, err, ErrMalformedToken)

	_, err = ParseRefreshToken(refresh)
	assert.NoError(t, err)
}

func TestParseTokenRejectsJunk(t *testing.T) {
	for _, bad := range []string{
		"", "nrt_", "nrt_short", "nonsense",
		"nrt_" + strings.Repeat("A", 100), // right prefix, wrong length
		"nrt_!!!not base64!!!",
	} {
		_, err := ParseRefreshToken(bad)
		assert.ErrorIs(t, err, ErrMalformedToken, "input %q must be rejected", bad)
	}
}

func TestHashTokenIsStableAndMatches(t *testing.T) {
	raw, hash, err := GenerateRefreshToken()
	require.NoError(t, err)

	assert.True(t, HashToken(raw).Equal(hash))
	assert.False(t, HashToken(raw+"x").Equal(hash))
}

func TestRedactTokenKeepsNoEntropy(t *testing.T) {
	raw, _, err := GenerateAPIToken()
	require.NoError(t, err)

	redacted := RedactToken(raw)
	assert.Equal(t, "nat_…", redacted)
	assert.NotContains(t, raw, redacted[4:], "the redaction must not leak any of the token body")
}

// ---------- access tokens ----------

func newIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	issuer, err := NewTokenIssuer([]byte(testSigningKey))
	require.NoError(t, err)
	return issuer
}

func TestTokenIssuerRejectsAShortKey(t *testing.T) {
	_, err := NewTokenIssuer([]byte("too short"))
	assert.ErrorIs(t, err, ErrSigningKeyTooShort)

	_, err = NewTokenIssuer(make([]byte, MinSigningKeyLength))
	assert.NoError(t, err)
}

func TestIssueAndVerify(t *testing.T) {
	issuer := newIssuer(t)
	userID, sessionID := snowflake.ID(1234567890), snowflake.ID(9876543210)

	token, expiresAt, err := issuer.Issue(userID, sessionID)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(AccessTokenTTL), expiresAt, time.Minute)

	claims, err := issuer.Verify(token)
	require.NoError(t, err)

	gotUser, err := claims.UserID()
	require.NoError(t, err)
	assert.Equal(t, userID, gotUser)

	gotSession, err := claims.Session()
	require.NoError(t, err)
	assert.Equal(t, sessionID, gotSession)
}

// The single most-exploited JWT misconfiguration: a token whose header claims no signature, or a different
// algorithm family, must never verify.
func TestVerifyRejectsAlgorithmConfusion(t *testing.T) {
	issuer := newIssuer(t)

	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   "1234567890",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		TokenType: tokenTypeName,
	})
	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = issuer.Verify(raw)
	assert.ErrorIs(t, err, ErrInvalidToken, `a token with "alg":"none" must never verify`)
}

func TestVerifyRejectsAForeignSignature(t *testing.T) {
	mine := newIssuer(t)

	theirs, err := NewTokenIssuer([]byte("a completely different signing key!!"))
	require.NoError(t, err)

	token, _, err := theirs.Issue(snowflake.ID(1), snowflake.ID(2))
	require.NoError(t, err)

	_, err = mine.Verify(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	issuer := newIssuer(t)

	// Issue in the past, then verify at real "now".
	issuer.now = func() time.Time { return time.Now().Add(-2 * AccessTokenTTL) }
	token, _, err := issuer.Issue(snowflake.ID(1), snowflake.ID(2))
	require.NoError(t, err)

	issuer.now = time.Now
	_, err = issuer.Verify(token)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsATokenOfAnotherType(t *testing.T) {
	issuer := newIssuer(t)

	// A genuinely-signed token minted for some other purpose must not authenticate a request — the
	// "confused deputy" shape the typ claim exists to prevent.
	other := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   "1234567890",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		TokenType: "password_reset",
	})
	raw, err := other.SignedString([]byte(testSigningKey))
	require.NoError(t, err)

	_, err = issuer.Verify(raw)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsJunk(t *testing.T) {
	issuer := newIssuer(t)
	for _, bad := range []string{"", "not.a.jwt", "eyJhbGciOiJIUzI1NiJ9", strings.Repeat("a", 500)} {
		_, err := issuer.Verify(bad)
		assert.ErrorIs(t, err, ErrInvalidToken, "input %q must be rejected", bad)
	}
}

// ---------- scopes and actors ----------

func TestScopeValidation(t *testing.T) {
	assert.True(t, ValidScope(ScopeIdentify))
	assert.False(t, ValidScope("wildcard"), "an unknown scope must never validate")
	// Token management is user-only and has no scope at all, so these must never validate either.
	assert.False(t, ValidScope("tokens:read"))
	assert.False(t, ValidScope("tokens:write"))
	assert.False(t, ValidScope(""), "the empty scope is not a scope")
}

func TestActorScopeSemantics(t *testing.T) {
	user := Actor{Kind: ActorUser, UserID: 1}
	// A person is not restricted by scopes — those exist to bound *delegated* credentials.
	assert.True(t, user.HasScope(ScopeIdentify))
	assert.True(t, user.HasScope("a scope that does not exist yet"))

	token := Actor{Kind: ActorAPIToken, UserID: 1, Scopes: []Scope{ScopeIdentify}}
	assert.True(t, token.HasScope(ScopeIdentify))
	assert.False(t, token.HasScope("some:other"), "a token must not hold a scope it was not granted")

	none := Actor{Kind: ActorAPIToken, UserID: 1}
	assert.False(t, none.HasScope(ScopeIdentify), "a token with no scopes can do nothing")
}

// An actor may only reach a handler by passing through the middleware. The context key is unexported
// precisely so nothing else can inject one.
func TestActorRoundTripsThroughContext(t *testing.T) {
	ctx := t.Context()

	_, ok := ActorFrom(ctx)
	assert.False(t, ok, "a bare context carries no actor")

	want := Actor{Kind: ActorUser, UserID: snowflake.ID(42), SessionID: snowflake.ID(7)}
	got, ok := ActorFrom(WithActor(ctx, want))
	require.True(t, ok)
	assert.Equal(t, want, got)

	// The zero actor is not an authenticated one, so a struct that got zeroed cannot pass as a caller.
	_, ok = ActorFrom(WithActor(ctx, Actor{}))
	assert.False(t, ok)
}

// argon2id allocates 64 MiB for the whole duration of every hash, which is the point of a memory-hard KDF
// and also a denial-of-service surface: 16 concurrent logins measured at 1 GiB of heap, and the per-IP rate
// limiter does nothing about a distributed flood. The gate turns the worst case into arithmetic.
func TestConcurrentHashingIsBounded(t *testing.T) {
	require.GreaterOrEqual(t, maxConcurrentHashes, 2, "a single-core instance must still be able to serve logins")

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)

	var wg sync.WaitGroup
	for range maxConcurrentHashes * 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = withHashSlot(t.Context(), func() error {
				mu.Lock()
				inFlight++
				if inFlight > peak {
					peak = inFlight
				}
				mu.Unlock()

				time.Sleep(5 * time.Millisecond)

				mu.Lock()
				inFlight--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	assert.LessOrEqual(t, peak, maxConcurrentHashes,
		"more hashes ran at once than the gate allows — the memory ceiling is not a ceiling")
	assert.Positive(t, peak)
}

// A caller who gives up while queued must not go on to do the work anyway: that would let a flood of
// abandoned requests hold 64 MiB each long after their clients disconnected.
func TestHashingGivesUpWhenTheCallerDoes(t *testing.T) {
	// Fill every slot and hold them.
	release := make(chan struct{})
	var held sync.WaitGroup
	for range maxConcurrentHashes {
		held.Add(1)
		go func() {
			defer held.Done()
			_ = withHashSlot(t.Context(), func() error { <-release; return nil })
		}()
	}
	// Give the fillers a moment to actually acquire.
	require.Eventually(t, func() bool { return len(hashSlots) == maxConcurrentHashes }, time.Second, time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ran := false
	err := withHashSlot(ctx, func() error { ran = true; return nil })

	assert.Error(t, err, "a canceled caller must not be granted a slot")
	assert.False(t, ran, "the work must not run for a caller that already gave up")

	close(release)
	held.Wait()
}

func TestPasswordHashingRespectsACanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Not a hard guarantee that it never runs — an idle gate admits immediately — but it must never hang,
	// and a full gate must reject rather than queue a caller who is already gone.
	_, err := HashPassword(ctx, "correct horse battery staple")
	_ = err // an idle gate legitimately succeeds; the bounded-wait behavior is covered above.
}

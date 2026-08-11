package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/database"
	"github.com/Alexnex31/Norite/backend/internal/platform/dbtest"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/migrations"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

const testPassword = "correct horse battery staple"

// newService builds a service over a freshly-migrated database.
func newService(t *testing.T, mode RegistrationMode) (*Service, *pgxpool.Pool) {
	t.Helper()

	dsn := dbtest.FreshDatabase(t)
	ctx := t.Context()

	require.NoError(t, database.Migrate(ctx, database.MigrateOptions{
		DatabaseURL: dsn,
		Source:      migrations.FS,
		SourceDir:   ".",
		LockTimeout: 30 * time.Second,
	}))

	pool, err := database.New(ctx, database.PoolOptions{
		DatabaseURL:    dsn,
		MaxConns:       4,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	ids, err := snowflake.NewGenerator(0)
	require.NoError(t, err)
	issuer, err := NewTokenIssuer([]byte(testSigningKey))
	require.NoError(t, err)

	svc, err := NewService(ServiceOptions{Pool: pool, IDs: ids, Issuer: issuer, RegistrationMode: mode})
	require.NoError(t, err)
	return svc, pool
}

// registerAndLogin is the common setup: one account, logged in on one device.
func registerAndLogin(t *testing.T, svc *Service, email, deviceID string) (db.User, TokenPair) {
	t.Helper()
	ctx := t.Context()

	user, err := svc.Register(ctx, RegisterInput{
		Username: email[:strings.IndexByte(email, '@')],
		Email:    email,
		Password: testPassword,
	})
	require.NoError(t, err)

	pair, err := svc.Login(ctx, LoginInput{Email: email, Password: testPassword, DeviceID: deviceID})
	require.NoError(t, err)
	return user, pair
}

// ---------- M4 done-when #1: a valid password yields an access + refresh pair ----------

func TestRegisterThenLoginIssuesATokenPair(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	user, pair := registerAndLogin(t, svc, "ada@example.com", "laptop")

	assert.NotEmpty(t, pair.AccessToken)
	assert.NotEmpty(t, pair.RefreshToken)
	assert.Equal(t, "Bearer", pair.TokenType)
	assert.WithinDuration(t, time.Now().Add(AccessTokenTTL), pair.ExpiresAt, time.Minute)

	// The access token must actually authenticate, and as the right account.
	actor, err := svc.AuthenticateAccessToken(t.Context(), pair.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, ActorUser, actor.Kind)
	assert.Equal(t, snowflake.ID(user.ID), actor.UserID)
}

func TestRegisterStoresNoRecoverablePassword(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)

	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	var stored *string
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT password_hash FROM users WHERE id = $1", user.ID).Scan(&stored))
	require.NotNil(t, stored)

	assert.NotContains(t, *stored, testPassword, "the password must not be recoverable from the row")
	assert.Contains(t, *stored, "$argon2id$")
}

func TestLoginRejectsAWrongPassword(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := svc.Login(t.Context(), LoginInput{
		Email: "ada@example.com", Password: "not the password", DeviceID: "laptop",
	})
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}

// An unknown address and a wrong password must be indistinguishable, or the login endpoint becomes a way to
// discover which addresses have accounts.
func TestLoginOnAnUnknownAccountFailsIdentically(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	_, err := svc.Login(t.Context(), LoginInput{
		Email: "nobody@example.com", Password: testPassword, DeviceID: "laptop",
	})
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestRegisterRejectsADuplicateEmail(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := svc.Register(t.Context(), RegisterInput{
		Username: "ada2", Email: "ada@example.com", Password: testPassword,
	})
	assert.ErrorIs(t, err, ErrEmailTaken)
}

// citext is what makes this true in the database rather than in whichever query remembered to lower().
func TestEmailAndUsernameUniquenessAreCaseInsensitive(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := svc.Register(t.Context(), RegisterInput{
		Username: "ADA", Email: "different@example.com", Password: testPassword,
	})
	assert.ErrorIs(t, err, ErrUsernameTaken)

	_, err = svc.Register(t.Context(), RegisterInput{
		Username: "someoneelse", Email: "ADA@Example.COM", Password: testPassword,
	})
	assert.ErrorIs(t, err, ErrEmailTaken)
}

// The decision recorded for M4: an instance configured for invite-only registration refuses outright rather
// than silently behaving as if it were open. M10 adds the redemption path that makes the mode usable.
func TestRegistrationIsRefusedWhenTheInstanceIsInviteOnly(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)

	_, err := svc.Register(t.Context(), RegisterInput{
		Username: "ada", Email: "ada@example.com", Password: testPassword,
	})
	assert.ErrorIs(t, err, ErrRegistrationClosed)
}

// ---------- refresh rotation ----------

func TestRefreshRotatesAndInvalidatesTheOldToken(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	_, first := registerAndLogin(t, svc, "ada@example.com", "laptop")

	second, err := svc.Refresh(t.Context(), first.RefreshToken)
	require.NoError(t, err)
	assert.NotEqual(t, first.RefreshToken, second.RefreshToken, "rotation must issue a new token")

	// The new one works.
	third, err := svc.Refresh(t.Context(), second.RefreshToken)
	require.NoError(t, err)
	assert.NotEqual(t, second.RefreshToken, third.RefreshToken)
}

// Presenting an already-rotated token is replay: either a retry after a lost response, or theft. The two
// are indistinguishable from here, so the pessimistic reading wins and the family is revoked.
func TestReplayingARotatedTokenRevokesThatDevicesFamily(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	_, first := registerAndLogin(t, svc, "ada@example.com", "laptop")

	second, err := svc.Refresh(t.Context(), first.RefreshToken)
	require.NoError(t, err)

	// Replay the consumed token.
	_, err = svc.Refresh(t.Context(), first.RefreshToken)
	assert.ErrorIs(t, err, ErrSessionReuse)

	// And the successor is now dead too — the whole family went, not just the replayed link.
	_, err = svc.Refresh(t.Context(), second.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken)
}

func TestRefreshRejectsGarbageAndUnknownTokens(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	unknown, _, err := GenerateRefreshToken()
	require.NoError(t, err)

	for name, token := range map[string]string{
		"malformed":    "not-a-token",
		"wrong prefix": "nat_" + unknown[4:],
		"never issued": unknown,
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Refresh(t.Context(), token)
			assert.ErrorIs(t, err, ErrInvalidRefreshToken)
		})
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	_, pair := registerAndLogin(t, svc, "ada@example.com", "laptop")

	require.NoError(t, svc.Logout(t.Context(), pair.RefreshToken))

	_, err := svc.Refresh(t.Context(), pair.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken)

	// Idempotent: a client retrying after a dropped response must not be told its logout failed.
	assert.NoError(t, svc.Logout(t.Context(), pair.RefreshToken))
	assert.NoError(t, svc.Logout(t.Context(), "nrt_never-issued"))
}

// ---------- M4 done-when #3: devices are independent ----------

// The invariant the whole device_id design exists for (ADR 0011). A user runs daemons on two machines;
// activity on one must never log out the other.
func TestRotatingOneDeviceLeavesAnotherDeviceValid(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, laptop := registerAndLogin(t, svc, "ada@example.com", "laptop")
	desktop, err := svc.Login(ctx, LoginInput{Email: "ada@example.com", Password: testPassword, DeviceID: "desktop"})
	require.NoError(t, err)

	// Rotate the laptop repeatedly.
	for range 3 {
		laptop, err = svc.Refresh(ctx, laptop.RefreshToken)
		require.NoError(t, err)
	}

	// The desktop's original token is still redeemable.
	rotatedDesktop, err := svc.Refresh(ctx, desktop.RefreshToken)
	require.NoError(t, err, "rotating one device must never invalidate another's family")
	assert.NotEmpty(t, rotatedDesktop.RefreshToken)
}

// And the harsher case: a *compromise* on one device must not log the other out either.
func TestReuseOnOneDeviceLeavesAnotherDeviceValid(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, laptop := registerAndLogin(t, svc, "ada@example.com", "laptop")
	desktop, err := svc.Login(ctx, LoginInput{Email: "ada@example.com", Password: testPassword, DeviceID: "desktop"})
	require.NoError(t, err)

	rotatedLaptop, err := svc.Refresh(ctx, laptop.RefreshToken)
	require.NoError(t, err)

	// Trigger reuse detection on the laptop.
	_, err = svc.Refresh(ctx, laptop.RefreshToken)
	require.ErrorIs(t, err, ErrSessionReuse)

	// The laptop's family is gone...
	_, err = svc.Refresh(ctx, rotatedLaptop.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken)

	// ...and the desktop is untouched. This is the bug the schema was designed to prevent.
	_, err = svc.Refresh(ctx, desktop.RefreshToken)
	assert.NoError(t, err, "reuse on one device must not revoke another device's family")
}

// Logging in again on the same device supersedes that device's previous family, so an old token stolen
// months ago does not stay redeemable forever.
func TestLoggingInAgainOnTheSameDeviceSupersedesTheOldSession(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, first := registerAndLogin(t, svc, "ada@example.com", "laptop")
	second, err := svc.Login(ctx, LoginInput{Email: "ada@example.com", Password: testPassword, DeviceID: "laptop"})
	require.NoError(t, err)

	_, err = svc.Refresh(ctx, first.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken, "the superseded token must not still work")

	_, err = svc.Refresh(ctx, second.RefreshToken)
	assert.NoError(t, err)
}

// ---------- M4 done-when #2: API tokens are restricted to their scopes ----------

func TestAPITokenAuthenticatesWithinItsScopesOnly(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	minted, err := svc.MintAPIToken(ctx, snowflake.ID(user.ID), MintAPITokenInput{
		Name:   "read-only bot",
		Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)
	require.NotEmpty(t, minted.Raw)

	actor, err := svc.AuthenticateAPIToken(ctx, minted.Raw)
	require.NoError(t, err)

	assert.Equal(t, ActorAPIToken, actor.Kind)
	assert.Equal(t, snowflake.ID(user.ID), actor.UserID)
	assert.True(t, actor.HasScope(ScopeIdentify), "the granted scope must be held")
	assert.False(t, actor.HasScope("some:other"), "an ungranted scope must not be held")
}

func TestMintedTokenIsStoredOnlyAsAHash(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	minted, err := svc.MintAPIToken(ctx, snowflake.ID(user.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)

	var stored []byte
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT token_hash FROM api_tokens WHERE id = $1", int64(minted.Token.ID)).Scan(&stored))

	assert.Len(t, stored, 32, "stored as a SHA-256")
	assert.NotContains(t, string(stored), minted.Raw, "the raw token must not be recoverable")
	assert.True(t, HashToken(minted.Raw).Equal(stored))
}

func TestMintRejectsAnUnknownScope(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := svc.MintAPIToken(t.Context(), snowflake.ID(user.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify, "admin:everything"},
	})
	assert.ErrorIs(t, err, ErrUnknownScope, "an unknown scope must be refused, never silently dropped")
}

func TestRevokedAPITokenStopsAuthenticating(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	minted, err := svc.MintAPIToken(ctx, snowflake.ID(user.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)

	_, err = svc.AuthenticateAPIToken(ctx, minted.Raw)
	require.NoError(t, err)

	require.NoError(t, svc.RevokeAPIToken(ctx, snowflake.ID(user.ID), snowflake.ID(minted.Token.ID)))

	_, err = svc.AuthenticateAPIToken(ctx, minted.Raw)
	assert.ErrorIs(t, err, ErrInvalidToken, "a revoked token must stop working immediately")
}

// Ownership is enforced in the statement's WHERE clause, so there is no version of this that forgets it.
func TestRevokingSomeoneElsesTokenIsNotFound(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	owner, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	minted, err := svc.MintAPIToken(ctx, snowflake.ID(owner.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)

	attacker, err := svc.Register(ctx, RegisterInput{
		Username: "mallory", Email: "mallory@example.com", Password: testPassword,
	})
	require.NoError(t, err)

	err = svc.RevokeAPIToken(ctx, snowflake.ID(attacker.ID), snowflake.ID(minted.Token.ID))
	assert.ErrorIs(t, err, ErrNotFound, "a token owned by someone else must be indistinguishable from a missing one")

	// And it still works for its actual owner.
	_, err = svc.AuthenticateAPIToken(ctx, minted.Raw)
	assert.NoError(t, err)
}

func TestListAPITokensReturnsNoCredentialMaterial(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	minted, err := svc.MintAPIToken(ctx, snowflake.ID(user.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)

	tokens, err := svc.ListAPITokens(ctx, snowflake.ID(user.ID))
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	assert.Equal(t, "bot", tokens[0].Name)

	// The listing carries the hash, which is not a usable credential, and never the raw value.
	assert.NotEqual(t, minted.Raw, string(tokens[0].TokenHash))
}

// An API token belonging to a soft-deleted account must stop working. Revoking a deleted user's tokens is
// M11's job, so without this check they would keep authenticating in the meantime.
func TestAPITokenOfASoftDeletedAccountStopsWorking(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	ctx := t.Context()

	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")
	minted, err := svc.MintAPIToken(ctx, snowflake.ID(user.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "UPDATE users SET deleted_at = now() WHERE id = $1", user.ID)
	require.NoError(t, err)

	_, err = svc.AuthenticateAPIToken(ctx, minted.Raw)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

// Regression: logout and "superseded by a fresh login" both revoke a session without rotating it, and the
// first version of Refresh treated any revoked session as replay. That meant this sequence — log out, log
// back in, then retry the stale token once (a dropped response, a queued request, an over-eager client) —
// triggered reuse detection and revoked the *new* session along with the old one, logging the user out of
// a device they had just signed into.
//
// replaced_by_id is what separates the two: only rotation sets it.
func TestAStaleTokenAfterLogoutDoesNotKillTheNewSession(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, first := registerAndLogin(t, svc, "ada@example.com", "laptop")
	require.NoError(t, svc.Logout(ctx, first.RefreshToken))

	second, err := svc.Login(ctx, LoginInput{Email: "ada@example.com", Password: testPassword, DeviceID: "laptop"})
	require.NoError(t, err)

	// The stale token from before the logout, presented again.
	_, err = svc.Refresh(ctx, first.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken, "a logged-out token is invalid, not evidence of theft")

	// The session created after the logout must be untouched.
	rotated, err := svc.Refresh(ctx, second.RefreshToken)
	require.NoError(t, err, "a stale pre-logout token must not revoke the session created after it")
	assert.NotEmpty(t, rotated.RefreshToken)
}

// The same shape, but for a session superseded by a second login rather than by an explicit logout.
func TestAStaleTokenAfterReloginDoesNotKillTheNewSession(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, first := registerAndLogin(t, svc, "ada@example.com", "laptop")
	second, err := svc.Login(ctx, LoginInput{Email: "ada@example.com", Password: testPassword, DeviceID: "laptop"})
	require.NoError(t, err)

	_, err = svc.Refresh(ctx, first.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken)

	_, err = svc.Refresh(ctx, second.RefreshToken)
	assert.NoError(t, err, "the superseding session must survive a retry of the superseded token")
}

// Reuse detection must still fire for the case it exists for: a token that was genuinely rotated away from.
func TestReuseDetectionStillFiresForAGenuinelyRotatedToken(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, first := registerAndLogin(t, svc, "ada@example.com", "laptop")
	_, err := svc.Refresh(ctx, first.RefreshToken)
	require.NoError(t, err)

	_, err = svc.Refresh(ctx, first.RefreshToken)
	assert.ErrorIs(t, err, ErrSessionReuse, "narrowing reuse detection must not switch it off")
}

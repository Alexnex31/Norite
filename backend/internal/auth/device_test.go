package auth

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
)

// The alphabet is the entropy figure, and the entropy figure is what the verification page's rate limit is
// sized against. A change to either that nobody notices would quietly move that number, so it is asserted
// rather than left to the constant's comment.
func TestTheUserCodeAlphabetIsWhatItClaims(t *testing.T) {
	assert.Len(t, userCodeAlphabet, 20)
	assert.Equal(t, 8, userCodeLength)

	for _, r := range userCodeAlphabet {
		assert.NotContains(t, "AEIOU", string(r), "a vowel lets a random draw spell something")
		assert.NotContains(t, "01OIL", string(r), "%q is misread off a terminal", string(r))
		assert.True(t, r >= 'A' && r <= 'Z', "%q is not an uppercase letter", string(r))
	}
	// The rejection sampling in GenerateUserCode drops every byte at or above this, so a bug that let one
	// through would bias the first (256 mod len) letters upward — invisible except as a smaller keyspace
	// than the comment claims.
	assert.Equal(t, 240, 256-(256%len(userCodeAlphabet)), "the rejection limit must be a whole multiple")
	assert.Len(t, uniqueRunes(userCodeAlphabet), 20, "a repeated letter would cost entropy silently")
}

// Every generated code is one this instance will accept back, which is the property that fails first if
// the generator and the parser drift.
func TestEveryGeneratedUserCodeParses(t *testing.T) {
	seen := make(map[string]bool, 200)
	for range 200 {
		code, err := GenerateUserCode()
		require.NoError(t, err)
		require.Len(t, code, userCodeLength)

		parsed, err := ParseUserCode(FormatUserCode(code))
		require.NoError(t, err, "%q was generated and then refused", code)
		assert.Equal(t, code, parsed)

		seen[code] = true
	}
	// Not a randomness test — a stuck generator returning one value is the failure this catches, and it
	// would otherwise show up as a collision-retry loop in production and nowhere else.
	assert.Greater(t, len(seen), 190, "the generator is repeating itself")
}

// What a person actually types. Case, spaces and dashes are things a phone keyboard adds on its own or a
// person gets wrong, none of them mean anything, so they are fixed rather than refused.
func TestUserCodeNormalization(t *testing.T) {
	for _, tc := range []struct{ why, in string }{
		{"the display form", "BCDF-GHJK"},
		{"no dash at all", "BCDFGHJK"},
		{"lowercase, as a phone keyboard offers first", "bcdf-ghjk"},
		{"mixed case", "Bcdf-GhJk"},
		{"spaces from a copy-paste", " BCDF GHJK "},
		{"a dash in the wrong place", "BC-DFGHJK"},
	} {
		got, err := ParseUserCode(tc.in)
		require.NoError(t, err, "%s: %q must be accepted", tc.why, tc.in)
		assert.Equal(t, "BCDFGHJK", got, tc.why)
	}
}

// One refusal each, because what this function is worth is entirely in what it will not look up.
func TestRefusedUserCodes(t *testing.T) {
	for _, tc := range []struct{ why, in string }{
		{"empty", ""},
		{"too short", "BCDFGHJ"},
		{"too long", "BCDFGHJKL"},
		{"far too long, which must not be scanned in full before refusing", strings.Repeat("B", 10_000)},
		{"a letter outside the alphabet", "BCDFGHJA"},
		{"a digit", "BCDFGHJ1"},
		{"punctuation that is not the grouping dash", "BCDF_GHJK"},
		{"a control character", "BCDFGHJ\x00"},
		{"an escape sequence, which must never reach a template", "BCDF\x1b[31mGHJK"},
	} {
		got, err := ParseUserCode(tc.in)
		assert.ErrorIs(t, err, ErrDeviceUserCode, "%s: %q must be refused", tc.why, tc.in)
		assert.Empty(t, got, "%s: a refusal must return nothing lookupable", tc.why)
	}
}

// The issued pair, and the shapes a client is told to expect.
func TestStartDeviceAuthIssuesBothCodes(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	auth, err := svc.StartDeviceAuth(t.Context(), StartDeviceAuthInput{
		DeviceID: "device-a", DeviceName: "archlinux",
	})
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(auth.DeviceCode, "nod_"), "device code: %q", auth.DeviceCode)
	_, err = ParseDeviceCode(auth.DeviceCode)
	assert.NoError(t, err)

	_, err = ParseUserCode(auth.UserCode)
	assert.NoError(t, err, "the issued user code must be one this instance accepts back")
	assert.Contains(t, auth.UserCode, "-", "the displayed form is grouped")

	assert.Equal(t, DevicePollInterval, auth.Interval)
	assert.WithinDuration(t, time.Now().Add(DeviceCodeTTL), auth.ExpiresAt, time.Minute)
}

// The device code is the only thing that redeems, and it is never derivable from the half a person sees.
func TestTheUserCodeCannotBePolledWith(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	auth, err := svc.StartDeviceAuth(t.Context(), StartDeviceAuthInput{
		DeviceID: "device-a", DeviceName: "archlinux",
	})
	require.NoError(t, err)

	_, err = svc.RedeemDeviceCode(t.Context(), auth.UserCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceCodeExpired)
}

// The ordinary answer to almost every poll, and the one a client keeps waiting on.
func TestPollingBeforeApprovalIsPending(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	auth := startDeviceAuth(t, svc, pool)

	_, err := svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceAuthorizationPending)
}

// A second poll inside the interval is refused, and — the part worth pinning — it is refused even once the
// authorization has landed. Rewarding a client that ignores the interval is what would make the interval
// advisory in name only (RFC 8628 §3.5).
func TestPollingTooFastIsSlowedDownEvenAfterApproval(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "device@example.com", "other-device")
	auth := startDeviceAuth(t, svc, pool)

	_, err := svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	require.ErrorIs(t, err, ErrDeviceAuthorizationPending)

	approve(t, svc, auth, user.ID)

	_, err = svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceSlowDown)

	// And the code is still there to collect once the client behaves — a slow_down must not spend it.
	letThePollIntervalPass(t, pool)
	pair, err := svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	require.NoError(t, err)
	assert.NotEmpty(t, pair.RefreshToken)
}

// The done-when, at the service level: an approved authorization becomes a session scoped to the device
// that asked for the code, not to anything the approving browser said.
func TestAnApprovedDeviceCodeBecomesASessionForTheWaitingDevice(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "device@example.com", "other-device")
	auth := startDeviceAuth(t, svc, pool)
	approve(t, svc, auth, user.ID)

	letThePollIntervalPass(t, pool)
	pair, err := svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.MustParseAddr("192.0.2.10"))
	require.NoError(t, err)
	require.NotEmpty(t, pair.AccessToken)

	var deviceID string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT device_id FROM sessions WHERE refresh_token_hash = $1`,
		HashToken(pair.RefreshToken)).Scan(&deviceID))
	assert.Equal(t, "waiting-device", deviceID,
		"the session must be scoped to the device that asked for the code")

	// And the other device's session is untouched, as for any other login.
	var live int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND device_id = 'other-device'
		   AND revoked_at IS NULL`, user.ID).Scan(&live))
	assert.Equal(t, 1, live)
}

// One authorization, one session. Two processes holding the same device code both reach the redemption,
// and exactly one of them may win — which is a property of ConsumeDeviceCode's WHERE clause, not of
// anything remembering to check.
func TestADeviceCodeIsSingleUse(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "device@example.com", "other-device")
	auth := startDeviceAuth(t, svc, pool)
	approve(t, svc, auth, user.ID)

	letThePollIntervalPass(t, pool)
	_, err := svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	require.NoError(t, err)

	letThePollIntervalPass(t, pool)
	_, err = svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceCodeExpired)
}

// Denial is its own answer and not an expiry, which is the whole reason the column exists: a client told
// "denied" stops now, where one told "pending" keeps asking for another twenty minutes.
func TestADeniedAuthorizationSaysSo(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	auth := startDeviceAuth(t, svc, pool)

	denied, err := svc.queries.DenyDeviceCode(t.Context(), auth.rowID)
	require.NoError(t, err)
	require.True(t, denied.DeniedAt.Valid)

	_, err = svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceAccessDenied)
}

// An expired authorization is refused, and refused as the same thing an unknown one is.
func TestAnExpiredDeviceCodeCannotBeRedeemed(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "device@example.com", "other-device")
	auth := startDeviceAuth(t, svc, pool)
	approve(t, svc, auth, user.ID)

	// Expired in the table rather than by moving the service's clock: every guard here reads the
	// database's now(), which is the point — expiry is not something a caller can be talked out of.
	_, err := pool.Exec(t.Context(),
		`UPDATE device_codes SET expires_at = now() - interval '1 minute' WHERE id = $1`, auth.rowID)
	require.NoError(t, err)

	_, err = svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceCodeExpired)
}

// An approval only counts once, so a stale approval page reloaded — or replayed — cannot re-arm an
// authorization that has already been spent or denied.
func TestAnApprovalOnlyCountsOnce(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "device@example.com", "other-device")
	other, _ := registerAndLogin(t, svc, "second@example.com", "third-device")
	auth := startDeviceAuth(t, svc, pool)

	approve(t, svc, auth, user.ID)

	_, err := svc.queries.ApproveDeviceCode(t.Context(),
		db.ApproveDeviceCodeParams{ID: auth.rowID, UserID: &other.ID})
	assert.Error(t, err, "a second approval must not re-point an authorization at another account")
}

// Rule 17: an approved-but-uncollected device code is an outstanding claim on the account, so the
// revoke-everything path has to reach it. Without this a password reset performed to lock an intruder out
// leaves them something that still trades for a fresh token pair.
func TestRevokingSessionsAlsoRevokesAnApprovedDeviceCode(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "device@example.com", "other-device")
	auth := startDeviceAuth(t, svc, pool)
	approve(t, svc, auth, user.ID)

	_, err := svc.queries.RevokeDeviceCodesForUser(t.Context(), &user.ID)
	require.NoError(t, err)

	letThePollIntervalPass(t, pool)
	_, err = svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	assert.ErrorIs(t, err, ErrDeviceCodeExpired)
}

// A device name is displayed on the approval page beside a warning, so it is bounded before it is stored.
func TestALongDeviceNameIsBoundedBeforeItIsStored(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)

	auth, err := svc.StartDeviceAuth(t.Context(), StartDeviceAuthInput{
		DeviceID: "device-a", DeviceName: strings.Repeat("é", 500),
	})
	require.NoError(t, err)

	var name string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT device_name FROM device_codes WHERE device_code_hash = $1`,
		HashToken(auth.DeviceCode)).Scan(&name))
	assert.Len(t, []rune(name), maxDeviceNameLength, "a multi-byte name must be cut by runes, not bytes")
}

// issuedDeviceAuth is a started authorization plus the row id the page-side queries take.
//
// The id is read straight out of the table rather than through GetDeviceCodeByUserCodeHash, which
// deliberately hides rows that are expired or already decided — exactly the rows several tests below need
// to reach.
type issuedDeviceAuth struct {
	DeviceAuth
	rowID int64
}

func startDeviceAuth(t *testing.T, svc *Service, pool *pgxpool.Pool) issuedDeviceAuth {
	t.Helper()
	auth, err := svc.StartDeviceAuth(t.Context(), StartDeviceAuthInput{
		DeviceID: "waiting-device", DeviceName: "archlinux",
	})
	require.NoError(t, err)

	var id int64
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT id FROM device_codes WHERE device_code_hash = $1`,
		HashToken(auth.DeviceCode)).Scan(&id))
	return issuedDeviceAuth{DeviceAuth: auth, rowID: id}
}

// letThePollIntervalPass backdates the last poll so the next one is not refused as too soon.
//
// The service's own clock is no longer the one that decides this — the comparison moved into SQL, because
// measuring a database timestamp against the application's clock measures the skew between two machines as
// well as the gap between two polls. So a test that wants to poll again has to move the value the query
// actually reads, which is also the only version of this that would catch the check being deleted.
func letThePollIntervalPass(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(t.Context(),
		`UPDATE device_codes SET last_polled_at = last_polled_at - make_interval(secs => $1)`,
		2*DevicePollInterval.Seconds())
	require.NoError(t, err)
}

func approve(t *testing.T, svc *Service, auth issuedDeviceAuth, userID int64) {
	t.Helper()
	_, err := svc.queries.ApproveDeviceCode(t.Context(),
		db.ApproveDeviceCodeParams{ID: auth.rowID, UserID: &userID})
	require.NoError(t, err)
}

func uniqueRunes(s string) map[rune]bool {
	out := make(map[rune]bool, len(s))
	for _, r := range s {
		out[r] = true
	}
	return out
}

// An approval token names one account, and the revocation Deny reaches has to be scoped to it. Tested here
// rather than over HTTP because a token for one account naming another's authorization is not something a
// browser can produce — it needs the signing key, which is exactly what makes the scoping a property of
// the query rather than of the token.
func TestADenialCannotRevokeAnotherAccountsAuthorization(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	ada, _ := registerAndLogin(t, svc, "ada@example.com", "ada-device")
	grace, _ := registerAndLogin(t, svc, "grace@example.com", "grace-device")

	auth := startDeviceAuth(t, svc, pool)
	approve(t, svc, auth, ada.ID)

	outcome, err := svc.DenyDeviceAuthorization(t.Context(), auth.rowID, grace.ID)
	require.NoError(t, err)
	assert.Equal(t, DeviceDenyTooLate, outcome,
		"another account's denial must reach nothing, and must not claim to have stopped anything")

	// Ada's authorization is untouched and still collectable.
	letThePollIntervalPass(t, pool)
	pair, err := svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	require.NoError(t, err)
	assert.NotEmpty(t, pair.RefreshToken)
}

// The three outcomes, each reached the way somebody actually reaches it.
func TestDenyReportsWhatItManagedToDo(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "other-device")

	pending := startDeviceAuth(t, svc, pool)
	outcome, err := svc.DenyDeviceAuthorization(t.Context(), pending.rowID, user.ID)
	require.NoError(t, err)
	assert.Equal(t, DeviceDenyStopped, outcome, "nothing had been authorized")

	approved := startDeviceAuth(t, svc, pool)
	approve(t, svc, approved, user.ID)
	outcome, err = svc.DenyDeviceAuthorization(t.Context(), approved.rowID, user.ID)
	require.NoError(t, err)
	assert.Equal(t, DeviceDenyRevoked, outcome, "an approval not yet collected can still be taken back")

	collected := startDeviceAuth(t, svc, pool)
	approve(t, svc, collected, user.ID)
	letThePollIntervalPass(t, pool)
	_, err = svc.RedeemDeviceCode(t.Context(), collected.DeviceCode, netip.Addr{})
	require.NoError(t, err)
	outcome, err = svc.DenyDeviceAuthorization(t.Context(), collected.rowID, user.ID)
	require.NoError(t, err)
	assert.Equal(t, DeviceDenyTooLate, outcome, "a redeemed code is past reach from the page")
}

// Spending the code and creating the session are one thing or neither. Without that, a transient failure
// between them leaves a spent authorization and no session — and every later poll answering expired_token
// for a pool timeout nobody saw, which costs a walk back to the other device rather than a retry.
//
// Forced with a constraint the session insert cannot satisfy, since a transient database failure is not
// something a test can ask for politely.
func TestAFailedSessionDoesNotBurnTheDeviceCode(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "other-device")
	auth := startDeviceAuth(t, svc, pool)
	approve(t, svc, auth, user.ID)
	letThePollIntervalPass(t, pool)

	// A trigger standing in for anything that can fail while the session is being written.
	_, err := pool.Exec(t.Context(), `
		CREATE FUNCTION fail_session() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'transient'; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER fail_session BEFORE INSERT ON sessions
		  FOR EACH ROW EXECUTE FUNCTION fail_session();`)
	require.NoError(t, err)

	_, err = svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrDeviceCodeExpired, "a failure here is not the code being spent")

	_, err = pool.Exec(t.Context(), `DROP TRIGGER fail_session ON sessions`)
	require.NoError(t, err)

	// And the authorization is still there to collect, which is the whole property.
	letThePollIntervalPass(t, pool)
	pair, err := svc.RedeemDeviceCode(t.Context(), auth.DeviceCode, netip.Addr{})
	require.NoError(t, err)
	assert.NotEmpty(t, pair.RefreshToken)
}

// The account line goes through the same sanitizer the device name does, because a page with one hole
// plugged and the other open is the same page.
//
// Registration constrains a username and an address, so this is a backstop rather than a reachable case
// today — written because "constrained somewhere else" is exactly the reasoning that quietly stops being
// true when a later milestone adds an import, an admin rename, or a second registration path. Driven
// against a row written directly, since the constrained path cannot produce one.
func TestAHostileAccountNameCannotReorderTheApprovalPage(t *testing.T) {
	svc, pool := newService(t, RegistrationOpen)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := pool.Exec(t.Context(),
		`UPDATE users SET username = $2 WHERE id = $1`, user.ID, "ada\u202elaptop")
	require.NoError(t, err)

	described := svc.describeAccount(t.Context(), user.ID)
	assert.NotContains(t, described, "\u202e", "nothing that reorders rendered text may reach the page")
	assert.Contains(t, described, "�", "with the removal marked rather than silent")
	assert.Contains(t, described, "ada@example.com", "and the address still identifies the account")
}

// A lookup failure degrades the line rather than the page. The approval screen's job is to let somebody
// notice a mismatch, and refusing to render it because a name could not be read would remove the check
// entirely — the device name, the code and the buttons are all still worth showing.
func TestAnUnreadableAccountStillRendersTheApprovalPage(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	assert.Equal(t, "your account", svc.describeAccount(t.Context(), 0))
}

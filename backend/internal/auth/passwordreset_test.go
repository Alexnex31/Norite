package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/mail"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// fakeMailer records what would have been sent, and can pretend to be disabled or full.
type fakeMailer struct {
	mu       sync.Mutex
	sent     []mail.Message
	disabled bool
	err      error
}

func (m *fakeMailer) Enabled() bool { return !m.disabled }

func (m *fakeMailer) Enqueue(msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

// only returns the single message of the given kind, and fails if there is not exactly one.
//
// Filtered by kind from M10 on, because registration now queues a verification mail of its own: a test
// about password reset would otherwise be counting a message it never asked for, and the fix of loosening
// the count to "at least one" would stop it noticing a duplicate reset link. Naming the kind also makes
// each assertion say which mail it means.
func (m *fakeMailer) only(t *testing.T, kind mail.Kind) mail.Message {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()

	var found []mail.Message
	for _, msg := range m.sent {
		if msg.Kind == kind {
			found = append(found, msg)
		}
	}
	require.Len(t, found, 1, "exactly one %s message should have been queued (all: %v)", kind, m.kinds())
	return found[0]
}

// count reports how many messages of one kind were queued.
func (m *fakeMailer) count(kind mail.Kind) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for _, msg := range m.sent {
		if msg.Kind == kind {
			n++
		}
	}
	return n
}

// allOfKind returns every message of one kind, in the order they were queued.
func (m *fakeMailer) allOfKind(kind mail.Kind) []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	var found []mail.Message
	for _, msg := range m.sent {
		if msg.Kind == kind {
			found = append(found, msg)
		}
	}
	return found
}

// kinds lists what was queued, for a failure message. Caller holds the lock.
func (m *fakeMailer) kinds() []mail.Kind {
	out := make([]mail.Kind, 0, len(m.sent))
	for _, msg := range m.sent {
		out = append(out, msg.Kind)
	}
	return out
}

// resetService builds a service with a mailer attached.
func resetService(t *testing.T) (*Service, *fakeMailer) {
	t.Helper()
	svc, _ := newService(t, RegistrationOpen)
	mailer := &fakeMailer{}
	svc.mailer = mailer
	svc.publicBaseURL = "https://chat.example.com"
	return svc, mailer
}

// tokenFromLink pulls the raw token back out of the emailed URL, which is the only place it exists.
func tokenFromLink(t *testing.T, msg mail.Message) string {
	t.Helper()
	_, after, found := strings.Cut(msg.Body, "token=")
	require.True(t, found, "the email must carry a reset link:\n%s", msg.Body)
	token, _, _ := strings.Cut(strings.TrimSpace(after), "\n")
	require.NotEmpty(t, token)
	return token
}

// ---------- M5 done-when #2: an unknown address is answered identically ----------

// The endpoint answers the same either way, and the service is what makes that true: no error, no
// distinguishable state change. Unlike registration — which necessarily discloses whether a name is taken
// — a reset request has no reason to say whether an address is registered.
func TestRequestingAResetForAnUnknownAddressLooksIdentical(t *testing.T) {
	svc, mailer := resetService(t)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	require.NoError(t, svc.RequestPasswordReset(t.Context(), "nobody@example.com"),
		"an unknown address must not be reported as an error")

	assert.Equal(t, 1, mailer.count(mail.KindPasswordReset), "only the real account may receive an email")
}

// An account that signs in with Google has no password to reset. Silent for the same reason: "that account
// uses OAuth" is exactly the detail an enumeration attempt is looking for.
func TestRequestingAResetForAnOAuthOnlyAccountIsSilent(t *testing.T) {
	svc, mailer := resetService(t)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	_, err := svc.pool.Exec(t.Context(), "UPDATE users SET password_hash = NULL WHERE id = $1", user.ID)
	require.NoError(t, err)

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	assert.Zero(t, mailer.count(mail.KindPasswordReset), "there is no password to reset, and nothing to disclose")
}

// ---------- M5 done-when #1: a reset completes ----------

func TestPasswordResetCompletesEndToEnd(t *testing.T) {
	svc, mailer := resetService(t)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))

	msg := mailer.only(t, mail.KindPasswordReset)
	assert.Equal(t, "ada@example.com", msg.To)
	assert.Contains(t, msg.Body, "https://chat.example.com/reset?token=")
	assert.Contains(t, msg.Body, "revokes any API tokens",
		"the email must say what resetting costs, since it cannot be undone")

	const newPassword = "an entirely different passphrase"
	require.NoError(t, svc.ConfirmPasswordReset(t.Context(), tokenFromLink(t, msg), newPassword))

	// The new password works.
	_, err := svc.Login(t.Context(), LoginInput{
		Email: "ada@example.com", Password: newPassword, DeviceID: "laptop",
	})
	require.NoError(t, err)

	// The old one does not.
	_, err = svc.Login(t.Context(), LoginInput{
		Email: "ada@example.com", Password: testPassword, DeviceID: "laptop",
	})
	assert.ErrorIs(t, err, ErrInvalidCredentials)

	_ = user
}

// A reset is how someone recovers a compromised account, so everything the old password could reach has to
// stop working — including the API tokens an intruder could have minted while they had access.
func TestResettingRevokesSessionsAndAPITokens(t *testing.T) {
	svc, mailer := resetService(t)
	user, pair := registerAndLogin(t, svc, "ada@example.com", "laptop")

	minted, err := svc.MintAPIToken(t.Context(), snowflake.ID(user.ID), MintAPITokenInput{
		Name: "bot", Scopes: []Scope{ScopeIdentify},
	})
	require.NoError(t, err)

	// Both credentials work before the reset.
	_, err = svc.AuthenticateAPIToken(t.Context(), minted.Raw)
	require.NoError(t, err)

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	require.NoError(t, svc.ConfirmPasswordReset(t.Context(), tokenFromLink(t, mailer.only(t, mail.KindPasswordReset)), "a new passphrase entirely"))

	_, err = svc.Refresh(t.Context(), pair.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken, "the session must not survive a password reset")

	_, err = svc.AuthenticateAPIToken(t.Context(), minted.Raw)
	assert.ErrorIs(t, err, ErrInvalidToken,
		"an API token an intruder could have minted must not survive the reset that evicts them")
}

// ---------- token lifecycle ----------

func TestAResetTokenIsSingleUse(t *testing.T) {
	svc, mailer := resetService(t)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	token := tokenFromLink(t, mailer.only(t, mail.KindPasswordReset))

	require.NoError(t, svc.ConfirmPasswordReset(t.Context(), token, "first new passphrase"))

	err := svc.ConfirmPasswordReset(t.Context(), token, "second new passphrase")
	assert.ErrorIs(t, err, ErrInvalidResetToken, "a spent token must not work twice")

	// And the second attempt changed nothing.
	_, err = svc.Login(t.Context(), LoginInput{
		Email: "ada@example.com", Password: "first new passphrase", DeviceID: "laptop",
	})
	assert.NoError(t, err)
}

// Requesting again spends the earlier token, so the newest link is the only one that works. Otherwise
// every request an anxious user makes leaves another live token behind.
func TestRequestingAgainInvalidatesTheEarlierToken(t *testing.T) {
	svc, mailer := resetService(t)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))

	// Filtered to reset mail: registering queued a verification link of its own (M10), and this test is
	// about the two reset links superseding each other.
	sent := mailer.allOfKind(mail.KindPasswordReset)
	require.Len(t, sent, 2)
	first, second := sent[0], sent[1]

	err := svc.ConfirmPasswordReset(t.Context(), tokenFromLink(t, first), "new passphrase here")
	assert.ErrorIs(t, err, ErrInvalidResetToken, "the superseded link must stop working")

	require.NoError(t, svc.ConfirmPasswordReset(t.Context(), tokenFromLink(t, second), "new passphrase here"))
}

func TestAnExpiredResetTokenIsRefused(t *testing.T) {
	svc, mailer := resetService(t)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	// Issue the token as though it were created well over the TTL ago.
	svc.now = func() time.Time { return time.Now().Add(-2 * PasswordResetTTL) }
	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	svc.now = time.Now

	err := svc.ConfirmPasswordReset(t.Context(), tokenFromLink(t, mailer.only(t, mail.KindPasswordReset)), "new passphrase here")
	assert.ErrorIs(t, err, ErrInvalidResetToken)
}

func TestGarbageResetTokensAreRefused(t *testing.T) {
	svc, _ := resetService(t)

	for name, token := range map[string]string{
		"empty":             "",
		"wrong prefix":      "nat_" + strings.Repeat("A", 43),
		"not base64":        "nrp_!!!!",
		"never issued":      "nrp_" + strings.Repeat("A", 43),
		"a refresh token":   "nrt_" + strings.Repeat("A", 43),
		"an arbitrary word": "hunter2",
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, svc.ConfirmPasswordReset(t.Context(), token, "new passphrase here"),
				ErrInvalidResetToken)
		})
	}
}

// The token was mailed to one address. If the account's address changed since, whoever controls the old
// mailbox is not necessarily the account holder any more.
func TestATokenIsRefusedAfterTheAccountsEmailChanges(t *testing.T) {
	svc, mailer := resetService(t)
	user, _ := registerAndLogin(t, svc, "ada@example.com", "laptop")

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	token := tokenFromLink(t, mailer.only(t, mail.KindPasswordReset))

	_, err := svc.pool.Exec(t.Context(),
		"UPDATE users SET email = 'moved@example.com' WHERE id = $1", user.ID)
	require.NoError(t, err)

	assert.ErrorIs(t, svc.ConfirmPasswordReset(t.Context(), token, "new passphrase here"),
		ErrInvalidResetToken)
}

// ---------- the relay ----------

// An instance with no relay must say so rather than accept a request it cannot fulfill. This does not
// depend on the address, so it discloses nothing about who has an account.
func TestResetIsUnavailableWithoutARelay(t *testing.T) {
	svc, mailer := resetService(t)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	mailer.disabled = true

	err := svc.RequestPasswordReset(t.Context(), "ada@example.com")
	assert.ErrorIs(t, err, ErrResetUnavailable)

	// And the same answer for an address with no account, so the error itself is not an oracle.
	assert.ErrorIs(t, svc.RequestPasswordReset(t.Context(), "nobody@example.com"), ErrResetUnavailable)
}

// The token is committed before the mail is queued, so a queue failure cannot roll it back — and must not
// fail the request either, since the caller already has its 202.
func TestAFailedEnqueueDoesNotFailTheRequest(t *testing.T) {
	svc, mailer := resetService(t)
	registerAndLogin(t, svc, "ada@example.com", "laptop")
	mailer.err = mail.ErrQueueFull

	assert.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"),
		"a saturated mail queue must not surface as a failed reset request")
}

// Two confirms racing on one token: exactly one may win. The guard is in the UPDATE's WHERE clause, so
// this holds without the service coordinating anything.
func TestConcurrentConfirmsSpendTheTokenOnce(t *testing.T) {
	svc, mailer := resetService(t)
	registerAndLogin(t, svc, "ada@example.com", "laptop")

	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	token := tokenFromLink(t, mailer.only(t, mail.KindPasswordReset))

	const racers = 4
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = svc.ConfirmPasswordReset(context.Background(), token, "new passphrase here")
		}()
	}
	close(start)
	wg.Wait()

	var succeeded int
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			assert.ErrorIs(t, err, ErrInvalidResetToken)
		}
	}
	assert.Equal(t, 1, succeeded, "exactly one confirm may spend the token")
}

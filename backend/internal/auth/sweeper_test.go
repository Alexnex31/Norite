package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/mail"
)

// The sweep exists because nothing else was ever going to run it: four comments in this package pointed at
// "M11's cleanup job", and M11 is the session-revocation primitive. Two of these three tables are written by
// unauthenticated endpoints, so before this they grew for the life of the instance.
func TestSweepRemovesExpiredRowsAndKeepsLiveOnes(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	mailer := &fakeMailer{}
	svc.mailer = mailer

	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	// One of each kind, live.
	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	outcome, _, err := signInBound(t, svc, stub, "google")
	require.NoError(t, err)
	require.NotEmpty(t, outcome.ExchangeCode)

	swept, err := svc.SweepExpired(t.Context())
	require.NoError(t, err)
	assert.Zero(t, swept.Total(), "nothing has expired yet, so nothing may be removed")

	// ...and one of each kind, expired. Aged in the database rather than on the service clock, because the
	// WHERE clauses compare against the database's own now().
	for _, table := range []string{"password_reset_tokens", "oauth_states", "oauth_exchange_codes"} {
		_, err := svc.pool.Exec(t.Context(),
			"UPDATE "+table+" SET expires_at = now() - interval '1 hour'")
		require.NoError(t, err)
	}

	swept, err = svc.SweepExpired(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), swept.ResetTokens)
	assert.Equal(t, int64(1), swept.OAuthStates)
	assert.Equal(t, int64(1), swept.ExchangeCodes)

	for _, table := range []string{"password_reset_tokens", "oauth_states", "oauth_exchange_codes"} {
		var remaining int
		require.NoError(t, svc.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&remaining))
		assert.Zero(t, remaining, "%s should be empty", table)
	}
}

// A spent row is swept exactly like an unspent one. This is the case the partial indexes got wrong — they
// were predicated on the row being unconsumed, while the sweep never looks at that — so it is worth
// asserting the behavior and not only the plan.
func TestSweepRemovesSpentRowsToo(t *testing.T) {
	svc, stub := oauthService(t, RegistrationOpen)
	mailer := &fakeMailer{}
	svc.mailer = mailer

	registerAndLogin(t, svc, "ada@example.com", "laptop")
	stub.asGoogle("google-1", "ada@example.com", true)

	// A completed sign-in: its state is consumed, and so is the exchange code once redeemed.
	outcome, verifier, err := signInBound(t, svc, stub, "google")
	require.NoError(t, err)
	_, err = svc.ExchangeOAuthCode(t.Context(), outcome.ExchangeCode, verifier,
		LoginInput{DeviceID: "laptop"})
	require.NoError(t, err)

	// A spent reset token.
	require.NoError(t, svc.RequestPasswordReset(t.Context(), "ada@example.com"))
	require.NoError(t, svc.ConfirmPasswordReset(t.Context(),
		tokenFromLink(t, mailer.only(t, mail.KindPasswordReset)), "a new passphrase entirely"))

	for _, table := range []string{"password_reset_tokens", "oauth_states", "oauth_exchange_codes"} {
		_, err := svc.pool.Exec(t.Context(),
			"UPDATE "+table+" SET expires_at = now() - interval '1 hour'")
		require.NoError(t, err)
	}

	swept, err := svc.SweepExpired(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3), swept.Total(), "consumed rows are dead weight and must be swept as well")
}

// Canceling the context stops the loop rather than leaving a goroutine behind for the life of the process.
func TestTheSweeperStopsWhenItsContextIsCancelled(t *testing.T) {
	svc, _ := oauthService(t, RegistrationOpen)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunSweeper(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweeper did not return after its context was canceled")
	}
}

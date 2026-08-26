package auth

import (
	"context"
	"strings"
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

// ---------- sessions (M11) ----------

// The sweep this table went seven milestones without.
//
// Worth exercising at the query level rather than only through RunSweeper: a WHERE clause one character
// wrong here empties the table people are signed in with.
func TestExpiredSessionsAreSweptAndLiveOnesAreNot(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, live := registerAndLogin(t, svc, "ada@example.com", "laptop")

	// A second device, aged past its expiry the way a year of rotations ages.
	_, err := svc.Login(ctx, LoginInput{
		Email: "ada@example.com", Password: testPassword, DeviceID: "old-laptop",
	})
	require.NoError(t, err)
	_, err = svc.pool.Exec(ctx,
		"UPDATE sessions SET expires_at = now() - interval '1 day' WHERE device_id = 'old-laptop'")
	require.NoError(t, err)

	swept, err := svc.queries.DeleteExpiredSessions(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, swept, "only the expired session may be removed")

	// The live one still works, which is the assertion that matters.
	_, err = svc.Refresh(ctx, live.RefreshToken)
	assert.NoError(t, err, "a live session must survive the sweep")
}

// A revoked session that has not expired must stay, because it is still evidence.
//
// replaced_by_id is what lets a presented token be recognized as *replay* rather than as merely unknown,
// and replay is what reuse detection revokes a whole device family on. Sweeping revoked rows early would
// turn a stolen token into an unrecognized one and quietly disable that detection — the table would look
// tidier and the security property would be gone. Past expires_at nothing can be presented, so the
// evidence has nothing left to prove; that is the whole reason the WHERE clause names expiry and not
// revocation.
func TestSweepingKeepsRevokedButUnexpiredSessions(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	_, first := registerAndLogin(t, svc, "ada@example.com", "laptop")

	// Rotate, which revokes the first session and leaves it pointing at its successor.
	second, err := svc.Refresh(ctx, first.RefreshToken)
	require.NoError(t, err)

	swept, err := svc.queries.DeleteExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Zero(t, swept, "a revoked row is not an expired one")

	// And the detection it exists for still fires: presenting the rotated-away token is replay, which
	// revokes the family rather than merely failing.
	_, err = svc.Refresh(ctx, first.RefreshToken)
	require.ErrorIs(t, err, ErrSessionReuse, "the evidence the sweep spared is what makes this replay")

	_, err = svc.Refresh(ctx, second.RefreshToken)
	assert.ErrorIs(t, err, ErrInvalidRefreshToken, "and replay took the whole family with it")
}

// The sweeper's own accounting: a table added to SweepExpired has to reach SweepResult.Total, or the
// "swept N rows" line under-reports and the debug log stays silent on a pass that did work.
func TestTheSweepCountsSessions(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)
	ctx := t.Context()

	registerAndLogin(t, svc, "ada@example.com", "laptop")
	_, err := svc.pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 day'")
	require.NoError(t, err)

	result, err := svc.SweepExpired(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.Sessions)
	assert.EqualValues(t, 1, result.Total(), "Total must include every table SweepExpired touches")
}

// Every index a sweep depends on must be non-partial, and this is the only kind of test that can say so.
//
// The behavior tests above pass either way: a partial index does not produce wrong rows, it produces a
// sequential scan. That is exactly why the mistake 000005 corrected survived three migrations unnoticed —
// nothing was broken, something was merely scanning a table it had an index for. So this asserts the
// *shape*, which is the property, rather than the results, which are not affected.
//
// Table-wide rather than per-migration on purpose: the next table with a TTL will add its own expires_at
// index, and the value of this test is that it is already watching when that happens.
func TestNoSweepIndexIsPartial(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	rows, err := svc.pool.Query(t.Context(), `
		SELECT c.relname, i.indexrelid::regclass::text, pg_get_indexdef(i.indexrelid)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		WHERE c.relnamespace = 'public'::regnamespace
		  AND pg_get_indexdef(i.indexrelid) LIKE '%expires_at%'
		  AND i.indpred IS NOT NULL
		ORDER BY c.relname`)
	require.NoError(t, err)
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var table, index, def string
		require.NoError(t, rows.Scan(&table, &index, &def))
		offenders = append(offenders, index+" on "+table+": "+def)
	}
	require.NoError(t, rows.Err())

	assert.Empty(t, offenders,
		"a sweep deletes by expiry and never looks at whether a row was consumed or revoked, so a partial\n"+
			"index cannot serve it — see migration 000005, and 000012 for the same mistake on sessions:\n  %s",
		strings.Join(offenders, "\n  "))
}

// And the foreign key the sweep cascades through has an index.
//
// sessions.replaced_by_id is ON DELETE SET NULL, so every deleted row runs a referential-integrity trigger
// that looks for children. Unindexed, that trigger scanned the table: 3,757 ms against 9.4 ms of actual
// deleting, for two thousand rows out of fifty thousand (000012). Nothing about the FK declaration hints
// at it, and the cost only appears once something finally deletes — which, for this table, was M11.
func TestTheSessionChainForeignKeyIsIndexed(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	var indexed bool
	require.NoError(t, svc.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_index i
			WHERE i.indrelid = 'sessions'::regclass
			  AND pg_get_indexdef(i.indexrelid) LIKE '%replaced_by_id%'
		)`).Scan(&indexed))

	assert.True(t, indexed,
		"without this index the sweep spends four hundred times as long in the FK trigger as in the delete")
}

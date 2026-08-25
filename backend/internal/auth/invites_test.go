package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newInvite mints one through the service, so these test what an administrator would actually get.
func newInvite(t *testing.T, svc *Service, in CreateInviteInput) string {
	t.Helper()
	invite, err := svc.CreateInstanceInvite(context.Background(), in)
	require.NoError(t, err)
	return invite.Code
}

// The property RedeemInstanceInvite's single-statement shape exists for, and the one a check-then-update
// in Go would fail. Two people redeeming a one-use code at the same moment both read `uses = 0`; as one
// statement, Postgres serializes them on the row and the second re-evaluates its WHERE against the first's
// committed value.
//
// Confirmed by rewriting the query as a SELECT followed by an UPDATE, which lets four of four through.
func TestAOneUseInviteSurvivesConcurrentRedemption(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{MaxUses: 1})

	const attempts = 4
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  []string
		refused  int
		start    = make(chan struct{})
		register = func(i int) (string, error) {
			user, err := svc.Register(context.Background(), RegisterInput{
				Username:   fmt.Sprintf("racer%d", i),
				Email:      fmt.Sprintf("racer%d@example.com", i),
				Password:   "a-sufficiently-long-test-password",
				InviteCode: code,
			})
			return user.Username, err
		}
	)

	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			name, err := register(i)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created = append(created, name)
			case assert.ErrorIs(t, err, ErrInviteInvalid):
				refused++
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Len(t, created, 1, "a one-use invite may create exactly one account")
	assert.Equal(t, attempts-1, refused)
}

// An invite with several uses hands out exactly that many accounts and no more. The n-use case is not the
// one-use case with a bigger number: `uses < max_uses` has to be re-read on every attempt, which is what
// makes the ceiling hold under concurrency rather than approximately.
func TestAnInviteStopsAtItsCeiling(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{MaxUses: 3})

	var succeeded int
	for i := range 5 {
		_, err := svc.Register(context.Background(), RegisterInput{
			Username:   fmt.Sprintf("member%d", i),
			Email:      fmt.Sprintf("member%d@example.com", i),
			Password:   "a-sufficiently-long-test-password",
			InviteCode: code,
		})
		if err == nil {
			succeeded++
			continue
		}
		assert.ErrorIs(t, err, ErrInviteInvalid)
	}
	assert.Equal(t, 3, succeeded)
}

// NULL max_uses means unlimited, and it is written as an explicit IS NULL branch rather than left to
// three-valued logic — `uses < NULL` is NULL rather than true, so the most permissive setting would
// otherwise be the most restrictive one and match nothing at all.
func TestAnUnlimitedInviteKeepsWorking(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{})

	for i := range 3 {
		_, err := svc.Register(context.Background(), RegisterInput{
			Username:   fmt.Sprintf("open%d", i),
			Email:      fmt.Sprintf("open%d@example.com", i),
			Password:   "a-sufficiently-long-test-password",
			InviteCode: code,
		})
		require.NoError(t, err, "an invite with no use limit must not run out")
	}
}

// The other NULL branch, and the same reasoning: an invite with no expiry must not be treated as one that
// expired at the zero time.
func TestAnInviteWithNoExpiryDoesNotExpire(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{})

	// Far enough forward that any accidental comparison against a zero or default time would have fired.
	svc.now = func() time.Time { return time.Now().Add(10 * 365 * 24 * time.Hour) }

	_, err := svc.Register(context.Background(), RegisterInput{
		Username: "later", Email: "later@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: code,
	})
	assert.NoError(t, err)
}

// An expired invite is refused, and refused with the same error an unknown one gets.
func TestAnExpiredInviteIsRefused(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{ExpiresIn: time.Minute})

	// The expiry is evaluated by Postgres' now(), not the service's, so the wait has to be real — hence a
	// negative TTL applied directly rather than moving the service clock. Created through the service so
	// the row is shaped exactly as a real one.
	svc.now = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	stale := newInvite(t, svc, CreateInviteInput{ExpiresIn: time.Minute})
	svc.now = time.Now

	_, err := svc.Register(context.Background(), RegisterInput{
		Username: "late", Email: "late@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: stale,
	})
	assert.ErrorIs(t, err, ErrInviteInvalid)

	// The live one still works, so the refusal above is about expiry rather than about invites being
	// broken generally.
	_, err = svc.Register(context.Background(), RegisterInput{
		Username: "ontime", Email: "ontime@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: code,
	})
	assert.NoError(t, err)
}

// A gated instance with no code supplied. Distinct from an invalid code, because the two are different
// problems for the caller — one is "you need one of these", the other is "yours does not work" — and
// neither says anything about which codes exist.
func TestRegistrationWithoutAnInviteIsRefusedWhenGated(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)

	_, err := svc.Register(context.Background(), RegisterInput{
		Username: "nobody", Email: "nobody@example.com",
		Password: "a-sufficiently-long-test-password",
	})
	assert.ErrorIs(t, err, ErrInviteRequired)
}

// An open instance ignores a code rather than rejecting it. A client that always sends one is not doing
// anything wrong, and refusing would make the two modes differ in a way no caller can predict.
func TestAnOpenInstanceIgnoresAnInviteCode(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	_, err := svc.Register(context.Background(), RegisterInput{
		Username: "anyone", Email: "anyone@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: "NOT-A-REAL-CODE",
	})
	assert.NoError(t, err)
}

// A registration refused by the pre-checks leaves the invite unspent.
//
// Note what this does *not* prove. It was written claiming to test the shared transaction, and moving
// redemption outside that transaction leaves it passing — because Register's username and email checks run
// before the transaction opens, so this case never reaches the insert at all. What holds here is the
// ordering: the cheap refusals happen first. The transaction property is tested directly below.
func TestARefusedRegistrationDoesNotSpendTheInvite(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{MaxUses: 1})

	// Taken username: it passes the invite gate and fails afterwards, which is the ordering that matters.
	_, err := svc.Register(context.Background(), RegisterInput{
		Username: "taken", Email: "first@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: code,
	})
	require.NoError(t, err)

	second := newInvite(t, svc, CreateInviteInput{MaxUses: 1})
	_, err = svc.Register(context.Background(), RegisterInput{
		Username: "taken", Email: "second@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: second,
	})
	require.ErrorIs(t, err, ErrUsernameTaken)

	// The invite survived, so somebody else can still use it.
	_, err = svc.Register(context.Background(), RegisterInput{
		Username: "free", Email: "second@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: second,
	})
	assert.NoError(t, err, "a registration that failed must leave the invite unspent")
}

// Presentation is forgiven and content is not, the same treatment ParseUserCode gives a device code: case,
// spaces and dashes are things a person gets wrong or a chat client adds, and none of them carry meaning.
func TestAnInviteCodeIsForgivingAboutPresentation(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{MaxUses: 5})

	for _, typed := range []string{
		strings.ToLower(code),
		code[:4] + "-" + code[4:],
		" " + code + " ",
	} {
		parsed, err := ParseInviteCode(typed)
		require.NoError(t, err, "%q must normalize", typed)
		assert.Equal(t, code, parsed)
	}

	for _, bad := range []string{"", "TOO-SHORT", code + "X", "AEIOU" + code} {
		_, err := ParseInviteCode(bad)
		assert.ErrorIs(t, err, ErrInviteInvalid, "%q must be refused", bad)
	}
}

// Revocation, and that it reports whether there was anything to revoke — the difference between "done"
// and "check what you typed".
func TestRevokingAnInviteStopsItWorking(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{})

	require.NoError(t, svc.DeleteInstanceInvite(context.Background(), code))
	assert.ErrorIs(t, svc.DeleteInstanceInvite(context.Background(), code), ErrNotFound)

	_, err := svc.Register(context.Background(), RegisterInput{
		Username: "toolate", Email: "toolate@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: code,
	})
	assert.ErrorIs(t, err, ErrInviteInvalid)
}

// The sweep removes expired invites and leaves permanent ones alone. This is the only sweep that deletes
// something a person made on purpose, so the NULL case is the one worth pinning.
func TestTheSweepSparesAnInviteWithNoExpiry(t *testing.T) {
	svc, _ := newService(t, RegistrationInvite)

	permanent := newInvite(t, svc, CreateInviteInput{})
	svc.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	expired := newInvite(t, svc, CreateInviteInput{ExpiresIn: time.Hour})
	svc.now = time.Now

	result, err := svc.SweepExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Invites)

	_, err = svc.Register(context.Background(), RegisterInput{
		Username: "still", Email: "still@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: permanent,
	})
	assert.NoError(t, err, "an invite with no expiry must survive the sweep")
	_ = expired
}

// The transaction property itself, tested where it actually lives rather than through Register.
//
// redeemInvite takes a queries handle rather than opening its own transaction, so redemption and the
// account insert commit or roll back together. What that buys is the case Register's pre-checks cannot
// catch: two registrations race, both pass the username check, and the loser fails on the unique
// constraint *inside* the insert. Without the shared transaction that loser would keep the use it spent.
//
// Driven directly because the race is not reproducible on demand, and a test that only sometimes exercises
// its own subject is not evidence.
func TestRedemptionRollsBackWithItsTransaction(t *testing.T) {
	svc, pool := newService(t, RegistrationInvite)
	code := newInvite(t, svc, CreateInviteInput{MaxUses: 1})

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	require.NoError(t, redeemInvite(ctx, svc.queries.WithTx(tx), code))
	require.NoError(t, tx.Rollback(ctx))

	// Unspent, so the next caller still gets it. Reading it back through a real registration rather than
	// through the row, because "still redeemable" is the property, not "the counter says zero".
	_, err = svc.Register(ctx, RegisterInput{
		Username: "afterrollback", Email: "afterrollback@example.com",
		Password: "a-sufficiently-long-test-password", InviteCode: code,
	})
	assert.NoError(t, err, "a rolled-back registration must leave the invite redeemable")
}

package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The property the advisory lock exists for, and the reason it is not enough to check the count.
//
// Bootstrap reads "how many admins are there" and then acts on the answer. Under READ COMMITTED — the
// default, and this pool's — that is a read-modify-write with a gap in it: two concurrent calls both see
// zero, both insert a *different* user_id, and the primary key stops neither. The instance ends up with
// two administrators, one of whom nobody intended and no audit record explains.
//
// Nothing else in the schema can catch it. There is no row to lock, because the guard is the absence of
// rows, so SELECT ... FOR UPDATE would lock nothing at all.
//
// Confirmed by removing the LockInstanceBootstrap call: two of five runs then finish with two winners
// instead of one. Note what that means about this test — it detects the bug it was written for only some
// of the time, because the window is genuinely narrow (argon2id runs *before* the transaction opens, so
// the racing transactions are short). It is still worth having, since a re-introduced race shows up within
// a handful of CI runs rather than never, but a green run is weaker evidence here than elsewhere in this
// package and should not be read as proof the lock is unnecessary.
func TestOnlyOneConcurrentBootstrapWins(t *testing.T) {
	svc, _ := newService(t, RegistrationOpen)

	const attempts = 4

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created []string
		refused int
	)
	start := make(chan struct{})

	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Released together, so the four calls reach the lock at the same time rather than in the
			// order they were spawned.
			<-start

			user, err := svc.Bootstrap(context.Background(), BootstrapInput{
				Username: fmt.Sprintf("admin%d", i),
				Email:    fmt.Sprintf("admin%d@example.com", i),
				Password: "a-sufficiently-long-test-password",
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created = append(created, user.Username)
			case assert.ErrorIs(t, err, ErrAlreadyBootstrapped):
				refused++
			}
		}()
	}

	close(start)
	wg.Wait()

	assert.Len(t, created, 1, "exactly one bootstrap may create an administrator")
	assert.Equal(t, attempts-1, refused, "every other attempt must be refused, not failed some other way")

	// And the table agrees with what the callers were told, which is the assertion that would catch a lock
	// that serialized the inserts without the count ever being rechecked.
	admins, err := svc.queries.CountInstanceAdmins(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), admins)
}

package credentials

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Serializing access to the credential store across processes.
//
// # Why this is needed now and was not before
//
// Until M7 exactly one process wrote the state directory. Now two do: the CLI writes at `norite login`, and
// the daemon writes back the rotated refresh token at every start (M4 rotates on every refresh). Those can
// overlap — starting the daemon and logging in are things people do in either order, seconds apart — and
// the store is two files that have to agree.
//
// Atomic renames already rule out a torn file. What they do not rule out is a lost update: a login writing
// its record while the daemon writes a token, leaving a record from one session beside a token from
// another. Both halves are individually valid, which is what makes it worth preventing rather than
// detecting — nothing downstream would notice.
//
// architecture.md §3 requires exactly this of every writer of shared client state: atomic writes *plus*
// `gofrs/flock` around each read-modify-write cycle. This is that lock for the credential pair.

// ErrStoreUnavailable reports that nothing could be determined about what is stored: the lock was not
// taken, or the record or the secret could not be read. Nothing was written either.
//
// Its own sentinel because "I could not look" and "I looked and the write failed" call for opposite
// responses, and conflating them destroys credentials: the daemon treats a refused write as proof that the
// store still holds a token the instance has already rotated, and clears it. Not being able to look is not
// that proof — the likeliest holder of the lock is a `norite login` writing a *fresh* credential, which is
// exactly the one that must not be cleared.
//
// It covers every failure *before* the write for the same reason. Once the compare-and-swap has confirmed
// the stored secret is the one being replaced, a failure is genuinely "the write was refused"; anything
// earlier is not, whatever caused it — a held lock, a lock file that could not be opened at all, a token
// file whose mode changed under us.
var ErrStoreUnavailable = errors.New("the credential store could not be read")

// lockFileName is a lock and never holds content. Separate from the files it guards, because a lock taken
// on a file that is then replaced by rename protects nothing — the new file is a different inode, and the
// next process locks that one instead.
const lockFileName = "credentials.lock"

// lockTimeout bounds the wait. A credential operation is two small file writes, so anything approaching
// this means another process is wedged rather than busy — and blocking a login forever on a stuck daemon
// is worse than failing with something a person can act on.
const lockTimeout = 5 * time.Second

// lockRetryInterval is how often the wait re-tries. Short enough to be invisible on the ordinary
// uncontended path, long enough not to spin.
const lockRetryInterval = 20 * time.Millisecond

// withLock runs fn while holding the store's lock.
//
// exclusive for anything that writes, shared for reads: two daemons on one machine cannot both start
// anyway (they contend on daemon.lock first), but a read while a write is in flight is exactly the
// interleaving this exists to stop.
func (s *Store) withLock(exclusive bool, fn func() error) error {
	lock := flock.New(filepath.Join(s.dir, lockFileName))

	wait := lockTimeout
	if s.lockWait > 0 {
		wait = s.lockWait
	}

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	var (
		got bool
		err error
	)
	if exclusive {
		got, err = lock.TryLockContext(ctx, lockRetryInterval)
	} else {
		got, err = lock.TryRLockContext(ctx, lockRetryInterval)
	}
	if err != nil {
		return fmt.Errorf("%w: waiting for the credential lock: %w", ErrStoreUnavailable, err)
	}
	if !got {
		return fmt.Errorf(
			"%w: another process holds the lock (waited %s); is a `norite login` already running?",
			ErrStoreUnavailable, wait)
	}
	defer func() { _ = lock.Unlock() }()

	return fn()
}

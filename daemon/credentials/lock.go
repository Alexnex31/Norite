package credentials

import (
	"context"
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

	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
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
		return fmt.Errorf("waiting for the credential lock: %w", err)
	}
	if !got {
		return fmt.Errorf(
			"another process is using the credential store (waited %s); is a `norite login` already running?",
			lockTimeout)
	}
	defer func() { _ = lock.Unlock() }()

	return fn()
}

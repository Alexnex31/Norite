package daemonproc

import (
	"errors"
	"fmt"

	"github.com/gofrs/flock"
)

// ErrAlreadyRunning reports that another daemon already holds this user's single-instance lock.
//
// A distinct error rather than a generic startup failure because the caller treats it differently: it is
// the one failure mode that is routine rather than broken. `norite daemon start` on an already-started
// daemon, or a service manager racing its own restart, both land here, and neither deserves a stack trace
// or a non-zero exit that alarms an operator.
var ErrAlreadyRunning = errors.New("another norite daemon is already running for this user")

// instanceLock is an advisory whole-file lock proving this process is the only daemon for this OS user.
//
// One process per OS user account is the invariant the whole client architecture rests on (ADR 0010): the
// daemon is the sole holder of the account's tokens and, later, of the E2E keystore. Two daemons would mean
// two gateway connections racing for presence and two writers on one SQLite keystore. The check therefore
// has to be a real mutual exclusion, not a PID file — a stale PID file after a crash or an OOM kill would
// either block a legitimate start forever or, if the PID got recycled, be silently wrong.
//
// flock gives that for free: the lock is owned by the open file description, so the kernel releases it when
// the process dies, however it dies. There is nothing to clean up and nothing to go stale.
type instanceLock struct {
	fl *flock.Flock
}

// acquireInstanceLock takes the lock without blocking, or reports ErrAlreadyRunning.
//
// Non-blocking on purpose. Waiting would turn "the daemon is already up" into a process that hangs
// indefinitely looking healthy to its service manager, which is a worse failure than an immediate,
// legible refusal.
func acquireInstanceLock(path string) (*instanceLock, error) {
	fl := flock.New(path)

	held, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !held {
		return nil, ErrAlreadyRunning
	}
	return &instanceLock{fl: fl}, nil
}

// release drops the lock.
//
// The kernel would do this at exit anyway; doing it explicitly means a test can start a second daemon in
// the same process after the first has stopped, and means the lock is gone at the moment shutdown finishes
// rather than whenever the process image is torn down.
func (l *instanceLock) release() error {
	if err := l.fl.Unlock(); err != nil {
		return fmt.Errorf("unlocking %s: %w", l.fl.Path(), err)
	}
	return nil
}

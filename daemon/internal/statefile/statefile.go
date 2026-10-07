// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package statefile writes state.json. It is internal so that only the daemon can: see daemon/statefile,
// which reads it and is where the format is described.
package statefile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/Alexnex31/Norite/daemon/atomicfile"
	"github.com/Alexnex31/Norite/daemon/statefile"
)

// fileMode is the state file's: it will hold plugin grants, and the directory it is in is 0700 for the
// same reason.
const fileMode = 0o600

const (
	lockName = "state.lock"
	lockWait = 5 * time.Second
	lockPoll = 20 * time.Millisecond
)

// ErrLocked reports that something else held the state file's lock for the whole wait.
var ErrLocked = errors.New("the state file is locked by another process")

// Update changes the state file under its lock: fn is given the state as it is and edits it, and whatever
// else the file held is written back with it. fn may do the work the change stands for, so that the state
// and the thing it describes change together or not at all; an error from fn leaves the file as it was.
//
// then, when not nil, runs after the file has been written and before the lock is let go: for what must
// follow the state rather than lead it, and must still not be interleaved with the next update. It does
// not run when nothing was written because fn failed.
//
// One daemon runs per user, so the lock is not against a second daemon. It is against a second request to
// this one: two clients asking for a toggle at once are two goroutines.
func Update(ctx context.Context, stateDir string, fn func(s *statefile.State) error, then func()) error {
	lock := flock.New(filepath.Join(stateDir, lockName))
	lctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	locked, err := lock.TryLockContext(lctx, lockPoll)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("locking the state file: %w", err)
	}
	if !locked {
		return ErrLocked
	}
	defer func() { _ = lock.Unlock() }()

	state, raw, err := statefile.Load(stateDir)
	if err != nil {
		return err
	}
	before := state
	if err := fn(&state); err != nil {
		return err
	}
	state.Version = statefile.Version
	if state == before {
		// Nothing to record. No file is the defaults, and stays no file.
		if then != nil {
			then()
		}
		return nil
	}

	// The known fields over whatever else was there, so a daemon older than the one that last wrote the
	// file keeps the fields it does not know.
	known, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(known, &raw); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	err = atomicfile.Write(statefile.PathIn(stateDir), append(data, '\n'), atomicfile.Options{Mode: fileMode})
	if errors.Is(err, atomicfile.ErrNotDurable) {
		// Written and renamed. The toggle it records has been carried out, and reporting failure would
		// tell a caller the opposite of what is on disk.
		err = nil
	}
	if err == nil && then != nil {
		then()
	}
	return err
}

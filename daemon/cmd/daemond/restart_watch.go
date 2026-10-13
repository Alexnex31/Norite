// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"time"
)

// startAndWatch starts the installed version as a process of its own and stays until it has stood for
// watch, starting it again when it fails sooner. It is for a platform where the daemon cannot become its
// successor and nothing else is certain to restart it.
//
// Without it a new version that crashed at its start stayed down: the daemon that started it had exited
// 0, so to a service manager nothing had failed, and the start that undoes a bad version never came
// (M24 /code-review).
//
// start starts the program and returns a function that waits for it to end, answering nil when it ended
// by itself for a reason that is not a failure.
func startAndWatch(start func() (wait func() error, err error), watch time.Duration, restarts int) error {
	for try := 0; ; try++ {
		wait, err := start()
		if err != nil {
			return err
		}
		ended := make(chan error, 1)
		go func() { ended <- wait() }()
		timer := time.NewTimer(watch)
		select {
		case <-timer.C:
			return nil
		case err := <-ended:
			timer.Stop()
			if err == nil {
				// It handed over to another version, or was told to stop.
				return nil
			}
			if try == restarts {
				return fmt.Errorf("it was started %d times and did not stay up: %w", try+1, err)
			}
		}
	}
}

// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package daemonctl

import (
	"testing"
	"time"
)

// A process that has exited and that nobody has waited for still answers a signal. A daemon started by a
// parent that never waits stays that way, and the command waited its full twenty seconds for it and then
// said it had not exited.
func TestAnExitedProcessNobodyReapedIsGone(t *testing.T) {
	cmd := sleeper(t)
	pid := cmd.Process.Pid
	if isZombie(pid) {
		t.Fatal("a running process was called exited")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Not waited for: this test is the parent that never reaps. The cleanup does, afterwards.
	if !waitForExit(t.Context(), pid, 10*time.Second) {
		t.Error("an exited process that was not reaped was waited on to the end")
	}
}

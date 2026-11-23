// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package hotkey holds push-to-talk's key for the daemon (M35, ADR 0034 §2–§5): an OS-wide key, registered
// in pure Go on each OS, which tells the daemon when it goes down and up whatever program is in front.
//
//   - Windows: RegisterHotKey, on a locked thread with its own message loop.
//   - Linux: the GlobalShortcuts portal wherever it answers, otherwise a grab on an X server's root window,
//     and never X through XWayland, which loses and reorders presses (M25).
//   - macOS: Carbon's RegisterEventHotKey, on the application run loop the daemon's main thread runs.
//
// A key is held only while a call wants it (ADR 0034 §3): on Windows and X11 a registered key is taken from
// every other program. Registering takes a moment and may ask the person (the portal's dialog), so Bind
// never waits: what became of the key is reported as it happens, and a key that could not be registered is
// call state, never a failed call.
package hotkey

import "github.com/Alexnex31/Norite/daemon/ipc"

// Status is what became of a key: one of ipc's VoiceHotkey* states, why when it is unavailable, and the key
// as the desktop says it is bound, where the desktop chooses it.
type Status struct {
	State   string
	Reason  string
	Trigger string
}

// Events is what a binding reports, each from a goroutine of the binding's and never from inside Bind or
// after its stop has returned. Status is told each change of what became of the key; Held each time it goes
// down or up, down first.
type Events struct {
	Status func(Status)
	Held   func(down bool)
}

// A Binder registers keys. Bind starts registering key, in ipc.CanonicalKey's spelling, and returns at once
// with what releases it: stop returns once the key is released and ev will not be told anything more.
type Binder interface {
	Bind(key string, ev Events) (stop func())
}

// Unavailable is a Binder for a system with no key to offer: every key is unavailable, for why.
type Unavailable string

// Bind reports the key unavailable, from a goroutine of its own as every binding reports.
func (u Unavailable) Bind(_ string, ev Events) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: string(u)})
	}()
	return func() { <-done }
}

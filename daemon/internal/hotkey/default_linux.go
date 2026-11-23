// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"context"
	"errors"

	"github.com/godbus/dbus/v5"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Default is this OS's keys: the GlobalShortcuts portal wherever it answers, otherwise a grab on the
// session's X server when the session is X's, and never X through XWayland (ADR 0034 §2). Which is asked at
// each binding, since the session may have appeared since the daemon started (§4).
func Default() Binder { return linux{} }

type linux struct{}

func (linux) Bind(key string, ev Events) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bindLinux(ctx, key, ev, dbus.ConnectSessionBus)
	}()
	return func() {
		cancel()
		<-done
	}
}

// bindLinux binds name through the session bus connect makes, a connection of its own, which a test points
// at a bus of its own.
func bindLinux(ctx context.Context, name string, ev Events, connect func(...dbus.ConnOption) (*dbus.Conn, error)) {
	key, err := ParseKey(name)
	if err != nil {
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: err.Error()})
		return
	}
	why := "there is no session bus to reach a desktop portal on"
	bus, err := connect()
	if err == nil {
		defer func() { _ = bus.Close() }()
	} else {
		bus = nil
	}
	env := sessionEnvironment(bus)
	if bus != nil {
		var none *noPortal
		if err := bindPortal(ctx, bus, key, ev); !errors.As(err, &none) {
			return
		}
		why = none.why
	}
	switch {
	case wayland(env):
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: why + "; X through XWayland loses presses, " +
			"so Norite does not use it"})
	case env["DISPLAY"] == "":
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: why + ", and the session has no X server: " +
			"is somebody logged in at the machine?"})
	default:
		bindX11(ctx, env, key, ev, why)
	}
}

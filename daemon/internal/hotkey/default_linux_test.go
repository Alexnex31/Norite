// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// linuxBinding binds key as Default does, on the bus at addr ("" for none) and in a session whose
// environment the test sets, and returns its last report once it has made one.
func linuxBinding(t *testing.T, addr, key string) Status {
	t.Helper()
	connect := func(...dbus.ConnOption) (*dbus.Conn, error) {
		if addr == "" {
			return nil, errors.New("no session bus")
		}
		return dbus.Connect(addr)
	}
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bindLinux(ctx, key, rec.events(), connect)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool { return rec.last().State != "" }, 10*time.Second, 5*time.Millisecond)
	return rec.last()
}

// session sets the environment the binding reads where no service manager answers: the test's private bus
// has none.
func session(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, k := range sessionVariables {
		t.Setenv(k, vars[k])
	}
}

// With no portal to use, a Wayland session's key is refused, X through XWayland being no path (M25), and a
// session with neither is told so; an X session's is grabbed on its X server.
func TestWithNoPortalOnlyAnXSessionIsGrabbed(t *testing.T) {
	bus := privateBus(t)

	// A display nothing is on, never the developer's: a binding that tried X anyway would dial it.
	session(t, map[string]string{"WAYLAND_DISPLAY": "wayland-1", "DISPLAY": ":9999", "XDG_SESSION_TYPE": "wayland"})
	st := linuxBinding(t, bus, "ctrl+shift+space")
	require.Equal(t, ipc.VoiceHotkeyUnavailable, st.State)
	require.Contains(t, st.Reason, "no desktop portal answers")
	require.Contains(t, st.Reason, "XWayland")

	session(t, map[string]string{})
	st = linuxBinding(t, "", "ctrl+shift+space")
	require.Equal(t, ipc.VoiceHotkeyUnavailable, st.State)
	require.Contains(t, st.Reason, "no session bus")
	require.Contains(t, st.Reason, "no X server")

	display, auth := xvfb(t)
	session(t, map[string]string{"DISPLAY": display, "XAUTHORITY": auth, "XDG_SESSION_TYPE": "x11"})
	st = linuxBinding(t, bus, "ctrl+shift+space")
	require.Equal(t, ipc.VoiceHotkeyRegistered, st.State, "%+v", st)
}

// A portal that answers is used, an X server or not.
func TestAPortalIsUsedBeforeX(t *testing.T) {
	bus := privateBus(t)
	newFakePortal(t, bus, true)
	display, auth := xvfb(t)
	session(t, map[string]string{"DISPLAY": display, "XAUTHORITY": auth, "XDG_SESSION_TYPE": "x11"})
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bindLinux(ctx, "ctrl+shift+space", rec.events(),
			func(...dbus.ConnOption) (*dbus.Conn, error) { return dbus.Connect(bus) })
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyRegistered }, 10*time.Second,
		5*time.Millisecond)
	// The desktop's words for the key, which X has none of.
	require.Equal(t, "Ctrl+Shift+Space", rec.last().Trigger, "the portal was not used first")
}

// managerProperties is a user manager's Environment, as systemd's org.freedesktop.systemd1.Manager has it.
type managerProperties struct{ env []string }

func (m managerProperties) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface == "org.freedesktop.systemd1.Manager" && prop == "Environment" {
		return dbus.MakeVariant(m.env), nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(errors.New("no such property"))
}

// The session is where the service manager says it is now, over what the daemon was started with, which a
// daemon started before the login never had (M25); and nothing else the manager holds is taken.
func TestTheSessionIsTheServiceManagers(t *testing.T) {
	addr := privateBus(t)
	manager, err := dbus.Connect(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Close() })
	require.NoError(t, manager.Export(managerProperties{env: []string{"WAYLAND_DISPLAY=wayland-0", "DISPLAY=:7",
		"NORITE_PASSWORD=hunter2", "XDG_SESSION_TYPE=wayland"}}, "/org/freedesktop/systemd1",
		"org.freedesktop.DBus.Properties"))
	reply, err := manager.RequestName("org.freedesktop.systemd1", dbus.NameFlagDoNotQueue)
	require.NoError(t, err)
	require.Equal(t, dbus.RequestNameReplyPrimaryOwner, reply)

	session(t, map[string]string{"DISPLAY": ":1", "HOME": "/home/someone"})
	bus, err := dbus.Connect(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	env := sessionEnvironment(bus)
	require.Equal(t, "wayland-0", env["WAYLAND_DISPLAY"])
	require.Equal(t, ":7", env["DISPLAY"])
	require.Equal(t, "/home/someone", env["HOME"])
	require.NotContains(t, env, "NORITE_PASSWORD")
	require.True(t, wayland(env))
	// With no manager on the bus, the daemon's own.
	require.Equal(t, ":1", sessionEnvironment(nil)["DISPLAY"])
}

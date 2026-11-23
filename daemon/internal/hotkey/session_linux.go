// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// sessionVariables are what says where the graphical session is, and how to reach its X server.
var sessionVariables = []string{"WAYLAND_DISPLAY", "DISPLAY", "XAUTHORITY", "XDG_SESSION_TYPE", "HOME"}

// askWithin bounds a question to the session bus: the service manager's environment, the portal's version.
const askWithin = 2 * time.Second

// sessionEnvironment is the graphical session's environment as the account's service manager holds it
// now, over the daemon's own. A desktop puts WAYLAND_DISPLAY and DISPLAY in the user manager at login, after a
// daemon running as a service has its environment, and a daemon started at boot under --linger never had
// them (M25): its own environment says where the session was when it started, if anywhere.
func sessionEnvironment(bus *dbus.Conn) map[string]string {
	out := map[string]string{}
	for _, k := range sessionVariables {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	if bus == nil {
		return out
	}
	call := bus.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").Call(
		"org.freedesktop.DBus.Properties.Get", dbus.FlagNoAutoStart, "org.freedesktop.systemd1.Manager",
		"Environment")
	var v dbus.Variant
	if call.Err != nil || call.Store(&v) != nil {
		return out
	}
	list, _ := v.Value().([]string)
	for _, kv := range list {
		k, val, ok := strings.Cut(kv, "=")
		if ok && isSessionVariable(k) {
			out[k] = val
		}
	}
	return out
}

func isSessionVariable(k string) bool {
	for _, v := range sessionVariables {
		if v == k {
			return true
		}
	}
	return false
}

// wayland is whether the session is a Wayland one, where X is XWayland's and never a path (M25: under KWin
// it lost a held key outright and reordered presses).
func wayland(env map[string]string) bool {
	return env["WAYLAND_DISPLAY"] != "" || strings.EqualFold(env["XDG_SESSION_TYPE"], "wayland")
}

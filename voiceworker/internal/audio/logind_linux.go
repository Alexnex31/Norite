// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// logindWithin bounds the whole question to logind.
const logindWithin = time.Second

// loginSessions is the account's sessions as logind has them, over the system bus (D5): the manager's
// GetUser, the user's Sessions, and each session's Seat, Active and Class. Asked only once a stream is on a
// stand-in, never on an ordinary call. An account logind has no record of has no session: that is its
// NoSuchUser, which an account with no login and no linger gets.
func loginSessions() ([]loginSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), logindWithin)
	defer cancel()
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	const dest = "org.freedesktop.login1"
	var user dbus.ObjectPath
	err = conn.Object(dest, "/org/freedesktop/login1").CallWithContext(ctx,
		"org.freedesktop.login1.Manager.GetUser", 0, uint32(os.Getuid())).Store(&user) //nolint:gosec // a uid
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.freedesktop.login1.NoSuchUser" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []struct {
		ID   string
		Path dbus.ObjectPath
	}
	if err := property(ctx, conn.Object(dest, user), "org.freedesktop.login1.User", "Sessions", &refs); err != nil {
		return nil, err
	}
	sessions := make([]loginSession, 0, len(refs))
	for _, r := range refs {
		o := conn.Object(dest, r.Path)
		var s loginSession
		var seat struct {
			ID   string
			Path dbus.ObjectPath
		}
		const iface = "org.freedesktop.login1.Session"
		if err := errors.Join(property(ctx, o, iface, "Seat", &seat), property(ctx, o, iface, "Active", &s.Active),
			property(ctx, o, iface, "Class", &s.Class)); err != nil {
			// A session that ended between the listing and the question is no session.
			continue
		}
		s.Seat = seat.ID
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// property reads one property of o into v.
func property(ctx context.Context, o dbus.BusObject, iface, name string, v any) error {
	var variant dbus.Variant
	if err := o.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&variant); err != nil {
		return err
	}
	return dbus.Store([]any{variant.Value()}, v)
}

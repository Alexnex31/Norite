// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// logindWithin bounds connecting to the system bus, and then the questions to logind. loginFresh is how
// long the answer is kept: both streams ask once a second for as long as a call outlives its session, and
// the answer only chooses between two words for a device that is gone either way.
const (
	logindWithin = time.Second
	loginFresh   = 5 * time.Second
)

// login is logind's last answer, shared by both streams, and the system-bus connection it came over, kept
// for the worker's life and made again after a question on it fails: a connection made for each question
// is a handshake each time, and announced to every listener on the bus as it comes and goes. The worker
// exits with its call, which closes it.
var login struct {
	mu       sync.Mutex
	at       time.Time
	sessions []loginSession
	err      error
	conn     *dbus.Conn
	drop     context.CancelFunc
}

// loginSessions is loginSessionsNow's answer, asked again once it is older than loginFresh.
func loginSessions() ([]loginSession, error) {
	login.mu.Lock()
	defer login.mu.Unlock()
	if login.at.IsZero() || time.Since(login.at) > loginFresh {
		login.sessions, login.err = loginSessionsNow()
		login.at = time.Now()
	}
	return login.sessions, login.err
}

// loginSessionsNow is the account's sessions as logind has them, over the system bus (D5): the manager's
// GetUser, the user's Sessions, and each session's Seat, Active and Class. Asked only once a stream is on a
// stand-in, never on an ordinary call. An account logind has no record of has no session: that is its
// NoSuchUser, which an account with no login and no linger gets.
//
// Called under login.mu, which orders it with itself.
func loginSessionsNow() ([]loginSession, error) {
	if login.conn == nil {
		// A connection lives as long as its context, so the bound on connecting cancels it only if the
		// handshake outruns it: past that it is the kept connection's, and each question takes ctx.
		live, drop := context.WithCancel(context.Background())
		late := time.AfterFunc(logindWithin, drop)
		conn, err := dbus.ConnectSystemBus(dbus.WithContext(live))
		if !late.Stop() && err == nil {
			err = errors.New("the system bus did not answer in time")
		}
		if err != nil {
			drop()
			if conn != nil {
				_ = conn.Close()
			}
			return nil, err
		}
		login.conn, login.drop = conn, drop
	}
	// The questions' bound starts once connected, so a slow handshake leaves them their second.
	ctx, cancel := context.WithTimeout(context.Background(), logindWithin)
	defer cancel()
	sessions, err := askLogind(ctx, login.conn)
	if err != nil && !remote(err) {
		_ = login.conn.Close()
		login.drop()
		login.conn, login.drop = nil, nil
	}
	return sessions, err
}

// remote is whether err is an answer over the bus, which leaves the connection as it was: logind not
// running is ServiceUnknown, a session gone UnknownObject. Anything else is the connection's or the
// bound's.
func remote(err error) bool {
	var de dbus.Error
	return errors.As(err, &de)
}

// askLogind is loginSessionsNow's questions, on conn.
func askLogind(ctx context.Context, conn *dbus.Conn) ([]loginSession, error) {
	const dest = "org.freedesktop.login1"
	var user dbus.ObjectPath
	err := conn.Object(dest, "/org/freedesktop/login1").CallWithContext(ctx,
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
		var err error
		for _, p := range []struct {
			name string
			v    any
		}{{"Seat", &seat}, {"Active", &s.Active}, {"Class", &s.Class}} {
			if err = property(ctx, o, iface, p.name, p.v); err != nil {
				break
			}
		}
		if err != nil {
			// A session that ended between the listing and the question is no session, which logind says;
			// the bound running out or the connection failing says nothing of the session, and is the answer.
			if remote(err) {
				continue
			}
			return nil, err
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

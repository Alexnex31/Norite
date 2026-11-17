// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"context"
	"errors"
	"fmt"
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
// running is ServiceUnknown, a session gone UnknownObject, and an answer of a shape this does not read is
// logind's too. Anything else is the connection's or the bound's.
func remote(err error) bool {
	var de dbus.Error
	var mis misanswered
	return errors.As(err, &de) || errors.As(err, &mis)
}

// misanswered is an answer that came over the bus and could not be read as what was asked.
type misanswered struct{ error }

func (m misanswered) Unwrap() error { return m.error }

// call asks o's method and stores its answer in v: the call's own error as it came, the answer's not
// reading as v as misanswered.
func call(ctx context.Context, o dbus.BusObject, method string, v any, args ...any) error {
	c := o.CallWithContext(ctx, method, 0, args...)
	if c.Err != nil {
		return c.Err
	}
	if err := c.Store(v); err != nil {
		return misanswered{err}
	}
	return nil
}

// askLogind is loginSessionsNow's questions, on conn.
func askLogind(ctx context.Context, conn *dbus.Conn) ([]loginSession, error) {
	const dest = "org.freedesktop.login1"
	var user dbus.ObjectPath
	err := call(ctx, conn.Object(dest, "/org/freedesktop/login1"), "org.freedesktop.login1.Manager.GetUser",
		&user, uint32(os.Getuid())) //nolint:gosec // a uid
	if userGone(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []struct {
		ID   string
		Path dbus.ObjectPath
	}
	err = property(ctx, conn.Object(dest, user), "org.freedesktop.login1.User", "Sessions", &refs)
	if userGone(err) {
		return nil, nil // the account's last session ended, and logind let go of it, between the two
	}
	if err != nil {
		return nil, err
	}
	sessions := make([]loginSession, 0, len(refs))
	for _, r := range refs {
		// One GetAll a session rather than a Get a property: 2+n round trips under the bound, not 2+3n.
		var props map[string]dbus.Variant
		err := call(ctx, conn.Object(dest, r.Path), "org.freedesktop.DBus.Properties.GetAll", &props,
			"org.freedesktop.login1.Session")
		if gone(err) {
			continue // ended between the listing and the question, which logind says: no session
		}
		if err != nil {
			// The bound running out, the connection failing, logind going away or refusing: none says the
			// session ended, so none is an answer.
			return nil, err
		}
		s, err := sessionOf(props)
		if err != nil {
			return nil, misanswered{err}
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// gone is whether err is logind saying the session is no more: the object gone, or login1's own word.
func gone(err error) bool {
	var de dbus.Error
	return errors.As(err, &de) && (de.Name == "org.freedesktop.DBus.Error.UnknownObject" ||
		de.Name == "org.freedesktop.login1.NoSuchSession")
}

// userGone is whether err is logind saying it has no record of the account: an account with no login and
// no linger, or one whose last session ended a moment ago. Either has no session.
func userGone(err error) bool {
	var de dbus.Error
	return errors.As(err, &de) && (de.Name == "org.freedesktop.DBus.Error.UnknownObject" ||
		de.Name == "org.freedesktop.login1.NoSuchUser")
}

// sessionOf is a session from its properties, as GetAll gives them.
func sessionOf(props map[string]dbus.Variant) (loginSession, error) {
	var s loginSession
	var seat struct {
		ID   string
		Path dbus.ObjectPath
	}
	for _, p := range []struct {
		name string
		v    any
	}{{"Seat", &seat}, {"Active", &s.Active}, {"Class", &s.Class}} {
		v, ok := props[p.name]
		if !ok {
			return loginSession{}, fmt.Errorf("logind gave a session without its %s", p.name)
		}
		if err := dbus.Store([]any{v.Value()}, p.v); err != nil {
			return loginSession{}, fmt.Errorf("logind gave a session's %s as %s: %w", p.name, v.Signature(), err)
		}
	}
	s.Seat = seat.ID
	return s, nil
}

// property reads one property of o into v.
func property(ctx context.Context, o dbus.BusObject, iface, name string, v any) error {
	var variant dbus.Variant
	if err := call(ctx, o, "org.freedesktop.DBus.Properties.Get", &variant, iface, name); err != nil {
		return err
	}
	if err := dbus.Store([]any{variant.Value()}, v); err != nil {
		return misanswered{err}
	}
	return nil
}

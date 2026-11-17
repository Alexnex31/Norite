// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// A system bus that accepts the connection and never answers costs the question its bound and no more,
// and leaves no connection kept.
func TestABusThatNeverAnswersIsGivenUpOn(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bus")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	held := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			held <- c // accepted, and never answered
		}
	}()
	t.Cleanup(func() {
		close(held)
		for c := range held {
			_ = c.Close()
		}
	})
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path="+sock)
	login.mu.Lock()
	defer login.mu.Unlock()
	// Whatever an earlier test kept is set aside, so the question goes to this bus.
	kept, drop := login.conn, login.drop
	login.conn, login.drop = nil, nil
	defer func() { login.conn, login.drop = kept, drop }()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := loginSessionsNow()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a bus that never answered gave an answer")
		}
	case <-time.After(5 * logindWithin):
		t.Fatal("the question waited on the bus past its bound")
	}
	if took := time.Since(start); took > 2*logindWithin {
		t.Errorf("the question took %s, past twice its bound", took)
	}
	if login.conn != nil {
		t.Error("a connection that never finished its handshake is kept")
	}
}

// An answer over the bus, logind not running, a session gone or an answer of a shape not read, leaves the
// connection kept; anything else is the connection's, and it is made again.
func TestOnlyTheBusesOwnAnswersKeepTheConnection(t *testing.T) {
	if !remote(dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}) {
		t.Error("logind not running drops the connection")
	}
	if _, err := sessionOf(nil); !remote(misanswered{err}) {
		t.Error("a session logind gave without its properties drops the connection")
	}
	for _, err := range []error{errors.New("connection reset"), errors.New("context deadline exceeded")} {
		if remote(err) {
			t.Errorf("%v keeps the connection", err)
		}
	}
}

// Only logind saying so makes a session gone; logind going away or refusing is no answer about it.
func TestASessionIsGoneOnlyWhenLogindSaysSo(t *testing.T) {
	for name, want := range map[string]bool{
		"org.freedesktop.DBus.Error.UnknownObject":  true,
		"org.freedesktop.login1.NoSuchSession":      true,
		"org.freedesktop.DBus.Error.ServiceUnknown": false,
		"org.freedesktop.DBus.Error.NoReply":        false,
		"org.freedesktop.DBus.Error.AccessDenied":   false,
	} {
		if got := gone(dbus.Error{Name: name}); got != want {
			t.Errorf("%s: gone %v, want %v", name, got, want)
		}
	}
	if gone(errors.New("context deadline exceeded")) {
		t.Error("the bound running out is a session gone")
	}
}

// The account is not logged in when the manager has no record of it, or its own object is gone as its
// sessions are read. The manager's object gone is no answer about the account, nor is logind going away.
func TestAnAccountIsGoneOnlyWhenLogindSaysSo(t *testing.T) {
	for _, c := range []struct {
		ask        func(error) bool
		name       string
		want       bool
		whoAnswers string
	}{
		{noUser, "org.freedesktop.login1.NoSuchUser", true, "the manager"},
		{noUser, "org.freedesktop.DBus.Error.UnknownObject", false, "the manager"},
		{noUser, "org.freedesktop.DBus.Error.ServiceUnknown", false, "the manager"},
		{userGone, "org.freedesktop.DBus.Error.UnknownObject", true, "the account"},
		{userGone, "org.freedesktop.DBus.Error.NoReply", false, "the account"},
	} {
		if got := c.ask(dbus.Error{Name: c.name}); got != c.want {
			t.Errorf("%s from %s: gone %v, want %v", c.name, c.whoAnswers, got, c.want)
		}
	}
	for _, ask := range []func(error) bool{noUser, userGone} {
		if ask(nil) || ask(errors.New("context deadline exceeded")) {
			t.Error("no error, or the bound running out, is an account gone")
		}
	}
}

// A session's properties, as GetAll gives them, and one missing a property is an error, not a session.
func TestASessionIsReadFromItsProperties(t *testing.T) {
	props := map[string]dbus.Variant{
		"Seat":   dbus.MakeVariant([]any{"seat0", dbus.ObjectPath("/org/freedesktop/login1/seat/seat0")}),
		"Active": dbus.MakeVariant(true),
		"Class":  dbus.MakeVariant("user"),
		"Name":   dbus.MakeVariant("pass"),
	}
	s, err := sessionOf(props)
	if err != nil || s != (loginSession{Seat: "seat0", Active: true, Class: "user"}) {
		t.Errorf("%+v, %v", s, err)
	}
	delete(props, "Class")
	if _, err := sessionOf(props); err == nil {
		t.Error("a session without its class was read")
	}
}

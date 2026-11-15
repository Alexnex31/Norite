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

// An answer over the bus, logind not running or a session gone, leaves the connection kept; anything else
// is the connection's, and it is made again.
func TestOnlyTheBusesOwnAnswersKeepTheConnection(t *testing.T) {
	if !remote(dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}) {
		t.Error("logind not running drops the connection")
	}
	for _, err := range []error{errors.New("connection reset"), errors.New("context deadline exceeded")} {
		if remote(err) {
			t.Errorf("%v keeps the connection", err)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/turn/v5"
)

func (f fixture) liveStreams() int {
	f.s.streams.mu.Lock()
	defer f.s.streams.mu.Unlock()
	return len(f.s.streams.live)
}

// tcpClient is a TURN client over one TCP connection to the fixture.
func (f fixture) tcpClient(t *testing.T, cred Credential) (*turn.Client, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp4", f.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	return f.clientOn(t, turn.NewSTUNConn(conn), f.tcp.String(), cred), conn
}

// TestAStreamWhoseAllocationEndsIsClosed: a connection is asked again, every AuthenticateWithin, whether
// it holds an allocation. Asked once, one that ended its allocation with a Refresh of lifetime zero could
// stay open on Binding requests and hold a stream slot for nothing.
func TestAStreamWhoseAllocationEndsIsClosed(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Limits = DefaultLimits()
		o.Limits.AuthenticateWithin = 300 * time.Millisecond
	})
	c, _ := f.tcpClient(t, cred(1))
	relay, err := c.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	// Three checks, and one holding an allocation stays.
	time.Sleep(time.Second)
	if n := f.liveStreams(); n != 1 {
		t.Fatalf("a connection holding an allocation was closed: %d left", n)
	}
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, "the connection whose allocation ended closes", func() bool { return f.liveStreams() == 0 })
}

// TestAUDPAllocationDoesNotCountForAStream: a stream from the address and port a UDP allocation came from holds
// none, and is closed for it.
func TestAUDPAllocationDoesNotCountForAStream(t *testing.T) {
	udp := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 40000}
	tcp := &net.TCPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 40000}
	server := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3478}
	var s sources
	s.perSource, s.perAddr = map[string]int{}, map[string]int{}
	s.add(udp, server)
	if s.allocated(tcp, &net.TCPAddr{IP: server.IP, Port: server.Port}) {
		t.Fatal("a UDP allocation counted for a TCP connection from the same address and port")
	}
	if !s.allocated(udp, server) {
		t.Fatal("the UDP allocation itself is not counted")
	}
}

// TestStreamsAreBoundedOverTCPAndTLSTogether: the bound is on the server's connections, not on each
// listener's, or one address holds twice what it may.
func TestStreamsAreBoundedOverTCPAndTLSTogether(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(o *Options) {
		o.TLS, o.TLSConfig = ln, testTLSConfig(t)
		o.Limits = DefaultLimits()
		o.Limits.StreamsPerSource = 1
	})
	first, err := net.Dial("tcp4", f.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	eventually(t, time.Second, "the TCP connection is counted", func() bool { return f.liveStreams() == 1 })
	// Refused before any handshake, so long before HandshakeWithin would end it.
	second, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	_ = second.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := second.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("a TLS connection past the address's bound was kept: %v", err)
	}
}

// TestAWriteToAClientThatStopsReadingIsBounded: pion writes the SFU's packets to a stream from its
// relay's goroutine, which a client that never reads would otherwise hold for good.
func TestAWriteToAClientThatStopsReadingIsBounded(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Limits = DefaultLimits()
		o.Limits.WriteWithin = 100 * time.Millisecond
	})
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	c := &streamConn{Conn: server, s: f.s, key: "a client"}
	if !f.s.streams.admit(c) {
		t.Fatal("not admitted")
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Write(make([]byte, 1500))
		done <- err
	}()
	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("the write ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a write to a client that never reads did not end")
	}
	if n := f.liveStreams(); n != 0 {
		t.Fatalf("the connection was kept after its write timed out: %d", n)
	}
}

// TestClosingTheServerEndsEveryAllocation: pion's Close stops its listeners and nothing else. It ends the
// allocations once each listener's loop stops, and every stream it had accepted stayed open.
func TestClosingTheServerEndsEveryAllocation(t *testing.T) {
	f := newFixture(t, nil)
	udpRelay, err := f.client(t, cred(1)).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	c, conn := f.tcpClient(t, cred(2))
	if _, err := c.Allocate(); err != nil {
		t.Fatal(err)
	}
	if n := f.s.AllocationCount(); n != 2 {
		t.Fatalf("%d allocations", n)
	}
	relayAddr := udpRelay.LocalAddr().String()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, "the relay socket is free", func() bool {
		pc, err := net.ListenPacket("udp4", relayAddr)
		if err != nil {
			return false
		}
		_ = pc.Close()
		return true
	})
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("the stream outlived the server: %v", err)
	}
	if n := f.liveStreams(); n != 0 {
		t.Fatalf("%d streams after Close", n)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestAPanicInTheStreamListenerCostsOneConnection: contained per connection, the listener goes on
// accepting; contained around its loop, it stopped for good.
func TestAPanicInTheStreamListenerCostsOneConnection(t *testing.T) {
	f := newFixture(t, nil)
	hook := func(at string) {
		if at == "the stream listener" {
			panic("planted")
		}
	}
	testPanic.Store(&hook)
	first, err := net.Dial("tcp4", f.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("the connection a panic was handling was kept: %v", err)
	}
	testPanic.Store(nil)
	c, _ := f.tcpClient(t, cred(1))
	if _, err := c.Allocate(); err != nil {
		t.Fatalf("after the panic: %v", err)
	}
	if !strings.Contains(f.log.String(), "the stream listener") {
		t.Fatalf("the panic was not logged: %s", f.log)
	}
}

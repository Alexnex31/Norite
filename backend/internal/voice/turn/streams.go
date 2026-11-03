// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"
)

// streamListener is what pion accepts TCP and TLS connections from. pion bounds none of it: it accepts
// whatever arrives, runs a goroutine per connection for as long as the peer keeps it open, and on TLS
// logs every failed handshake at error level. So this listener does all three itself before pion sees a
// connection: it refuses one past the totals, handshakes within a deadline and silently, and hands pion
// a connection that closes itself when idle or when it has made no allocation in time.
type streamListener struct {
	net.Listener
	s   *Server
	tls *tls.Config

	accepted chan net.Conn
	done     chan struct{}
	once     sync.Once

	mu        sync.Mutex
	total     int
	perSource map[string]int
}

func newStreamListener(s *Server, ln net.Listener, config *tls.Config) *streamListener {
	l := &streamListener{
		Listener:  ln,
		s:         s,
		tls:       config,
		accepted:  make(chan net.Conn),
		done:      make(chan struct{}),
		perSource: map[string]int{},
	}
	go l.serve()
	return l
}

// Accept is pion's: a connection that has passed every check, handshaken if TLS.
func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accepted:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *streamListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.Listener.Close()
	})
	return err
}

func (l *streamListener) serve() {
	defer l.s.contain("the stream listener", nil)
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			// A transient failure, too many files most likely: wait rather than spin.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		key := sourceKey(raw.RemoteAddr())
		if !l.admit(key) {
			_ = raw.Close()
			continue
		}
		c := &streamConn{Conn: raw, l: l, key: key}
		// Under the lock its callback takes: the timer can fire before an unguarded assignment is seen.
		c.mu.Lock()
		c.authenticated = time.AfterFunc(l.s.lim.AuthenticateWithin, c.checkAuthenticated)
		c.mu.Unlock()
		go l.handOver(c)
	}
}

// handOver handshakes a TLS connection, then gives it to pion.
func (l *streamListener) handOver(c *streamConn) {
	defer l.s.contain("a stream handshake", nil)
	var conn net.Conn = c
	if l.tls != nil {
		tc := tls.Server(c, l.tls)
		ctx, cancel := context.WithTimeout(context.Background(), l.s.lim.HandshakeWithin)
		err := tc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			_ = c.Close()
			return
		}
		conn = tc
	}
	select {
	case l.accepted <- conn:
	case <-l.done:
		_ = conn.Close()
	}
}

func (l *streamListener) admit(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.s.lim.Streams || l.perSource[key] >= l.s.lim.StreamsPerSource {
		return false
	}
	l.total++
	l.perSource[key]++
	return true
}

func (l *streamListener) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.perSource[key]--; l.perSource[key] <= 0 {
		delete(l.perSource, key)
	}
}

// streamConn is one TCP connection, under TLS's if there is one: a read waits Idle at most, and the
// connection is closed if it has made no allocation within AuthenticateWithin.
type streamConn struct {
	net.Conn
	l    *streamListener
	key  string
	once sync.Once

	mu            sync.Mutex
	authenticated *time.Timer
}

func (c *streamConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.l.s.lim.Idle))
	return c.Conn.Read(p)
}

func (c *streamConn) checkAuthenticated() {
	if !c.l.s.sources.allocated(c.RemoteAddr()) {
		_ = c.Close()
	}
}

func (c *streamConn) Close() error {
	var err error
	c.once.Do(func() {
		c.mu.Lock()
		c.authenticated.Stop()
		c.mu.Unlock()
		err = c.Conn.Close()
		c.l.release(c.key)
	})
	return err
}

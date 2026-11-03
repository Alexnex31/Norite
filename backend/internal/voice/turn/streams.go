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
// a connection that closes itself when idle or when it holds no allocation.
type streamListener struct {
	net.Listener
	s   *Server
	tls *tls.Config

	accepted chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func newStreamListener(s *Server, ln net.Listener, config *tls.Config) *streamListener {
	l := &streamListener{
		Listener: ln,
		s:        s,
		tls:      config,
		accepted: make(chan net.Conn),
		done:     make(chan struct{}),
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
	for l.acceptOne() {
	}
}

// acceptOne takes one connection, and is false once the listener is closed. A panic is contained here,
// per connection, so that it costs that connection and not the listener.
func (l *streamListener) acceptOne() (more bool) {
	more = true
	defer l.s.contain("the stream listener", nil)
	raw, err := l.Listener.Accept()
	if err != nil {
		select {
		case <-l.done:
			return false
		default:
		}
		// A transient failure, too many files most likely: wait rather than spin.
		time.Sleep(50 * time.Millisecond)
		return true
	}
	c := &streamConn{Conn: raw, s: l.s, key: sourceKey(raw.RemoteAddr())}
	handed := false
	// Runs before contain recovers: a panic here closes the connection it was handling, and releases
	// its count if it had one.
	defer func() {
		if !handed {
			_ = c.Close()
		}
	}()
	panicHook("the stream listener")
	if !l.s.streams.admit(c) {
		return true
	}
	// Under the lock its callback takes: the timer can fire before an unguarded assignment is seen.
	c.mu.Lock()
	c.authenticated = time.AfterFunc(l.s.lim.AuthenticateWithin, c.checkAuthenticated)
	c.mu.Unlock()
	go l.handOver(c)
	handed = true
	return true
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

// streams counts the TCP and TLS connections the server holds, both listeners' together, and keeps them
// so that closing the server closes them: pion's own Close stops its listeners and leaves every
// connection it accepted running.
type streams struct {
	lim Limits

	mu        sync.Mutex
	closed    bool
	perSource map[string]int
	live      map[*streamConn]struct{}
}

func (st *streams) admit(c *streamConn) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed || len(st.live) >= st.lim.Streams || st.perSource[c.key] >= st.lim.StreamsPerSource {
		return false
	}
	st.live[c] = struct{}{}
	st.perSource[c.key]++
	return true
}

func (st *streams) release(c *streamConn) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.live[c]; !ok {
		return
	}
	delete(st.live, c)
	if st.perSource[c.key]--; st.perSource[c.key] <= 0 {
		delete(st.perSource, c.key)
	}
}

// close closes every connection, and admits none after.
func (st *streams) close() {
	st.mu.Lock()
	st.closed = true
	all := make([]*streamConn, 0, len(st.live))
	for c := range st.live {
		all = append(all, c)
	}
	st.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

// streamConn is one TCP connection, under TLS's if there is one: a read waits Idle at most, a write
// WriteWithin, and the connection is closed whenever it is found holding no allocation, every
// AuthenticateWithin.
type streamConn struct {
	net.Conn
	s    *Server
	key  string
	once sync.Once

	mu            sync.Mutex
	closed        bool
	authenticated *time.Timer
}

func (c *streamConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.s.lim.Idle))
	return c.Conn.Read(p)
}

// Write is bounded, since pion writes from the goroutine that relays the SFU's packets: a client that
// stops reading would otherwise hold it, and this connection, for as long as it went on sending. A write
// past the bound closes the connection, which a TLS connection could not survive anyway.
func (c *streamConn) Write(p []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.s.lim.WriteWithin))
	n, err := c.Conn.Write(p)
	if err != nil {
		_ = c.Close()
	}
	return n, err
}

// checkAuthenticated closes the connection if it holds no allocation, and looks again later if it does:
// an allocation ends with a Refresh of lifetime zero or with its relay, and the connection it came over
// must not outlive it by more than this.
func (c *streamConn) checkAuthenticated() {
	if !c.s.sources.allocated(c.RemoteAddr(), c.LocalAddr()) {
		_ = c.Close()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.authenticated.Reset(c.s.lim.AuthenticateWithin)
	}
}

func (c *streamConn) Close() error {
	var err error
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		// Nil when the server closes it between admitting it and arming the timer.
		if c.authenticated != nil {
			c.authenticated.Stop()
		}
		c.mu.Unlock()
		err = c.Conn.Close()
		c.s.streams.release(c)
	})
	return err
}

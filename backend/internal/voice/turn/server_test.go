// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

var testSecret = []byte("a secret for tests, at least thirty-two bytes long")

// fixture is a server on loopback with a socket standing in for the SFU, so a test can see exactly what
// the relay delivers.
type fixture struct {
	s   *Server
	sfu net.PacketConn
	udp net.Addr
	tcp net.Addr
	log *syncBuffer
}

// syncBuffer is the server's log as a test reads it, while the server's goroutines write it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int { return len(b.String()) }

func newFixture(t *testing.T, tune func(*Options)) fixture {
	t.Helper()
	sfu, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sfu.Close() })
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var log syncBuffer
	opts := Options{
		UDP:    udp,
		TCP:    tcp,
		Secret: testSecret,
		Relay:  netip.MustParseAddr("127.0.0.1"),
		SFU:    sfu.LocalAddr().(*net.UDPAddr).AddrPort(),
		Limits: DefaultLimits(),
		Logger: zerolog.New(&log),
	}
	if tune != nil {
		tune(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return fixture{s: s, sfu: sfu, udp: udp.LocalAddr(), tcp: tcp.Addr(), log: &log}
}

// client is a TURN client over UDP from a socket of its own, with cred.
func (f fixture) client(t *testing.T, cred Credential) *turn.Client {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return f.clientOn(t, conn, f.udp.String(), cred)
}

func (f fixture) clientOn(t *testing.T, conn net.PacketConn, server string, cred Credential) *turn.Client {
	t.Helper()
	c, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: server,
		TURNServerAddr: server,
		Conn:           conn,
		Username:       cred.Username,
		Password:       cred.Password,
		Realm:          Realm,
		RTO:            100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		_ = conn.Close()
	})
	return c
}

func (f fixture) sfuAddr() net.Addr { return f.sfu.LocalAddr() }

func cred(user snowflake.ID) Credential { return Mint(testSecret, user, time.Now()) }

// received is everything a socket hears within d.
func received(t *testing.T, pc net.PacketConn, d time.Duration) [][]byte {
	t.Helper()
	var got [][]byte
	deadline := time.Now().Add(d)
	buf := make([]byte, 1500)
	for {
		_ = pc.SetReadDeadline(deadline)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return got
		}
		got = append(got, bytes.Clone(buf[:n]))
	}
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("not within %v: %s", d, what)
}

func TestTheRelayReachesTheSFU(t *testing.T) {
	f := newFixture(t, nil)
	relay, err := f.client(t, cred(1)).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.WriteTo([]byte("to the SFU"), f.sfuAddr()); err != nil {
		t.Fatal(err)
	}
	got := received(t, f.sfu, time.Second)
	if len(got) != 1 || string(got[0]) != "to the SFU" {
		t.Fatalf("the SFU heard %q", got)
	}
	// The address the relay reports is the one it is bound to, on the SFU's host.
	if a := relay.LocalAddr().(*net.UDPAddr); !a.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("the relay reports %v", a)
	}
}

// TestTheRelayReachesNothingButTheSFU sends to another port of the SFU's own host, which pion's
// permission admits since it asks of the IP alone, and to another host, which it does not.
func TestTheRelayReachesNothingButTheSFU(t *testing.T) {
	f := newFixture(t, nil)
	other, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	elsewhere, err := net.ListenPacket("udp4", "127.0.0.2:0")
	if err != nil {
		t.Skip("no 127.0.0.2 here:", err)
	}
	defer func() { _ = elsewhere.Close() }()

	relay, err := f.client(t, cred(1)).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = relay.WriteTo([]byte("another port"), other.LocalAddr())
	_, err = relay.WriteTo([]byte("another host"), elsewhere.LocalAddr())
	if err == nil {
		t.Error("a permission for another host was granted")
	}
	if got := received(t, other, 500*time.Millisecond); len(got) != 0 {
		t.Errorf("another port of the SFU's host heard %q", got)
	}
	if got := received(t, elsewhere, 100*time.Millisecond); len(got) != 0 {
		t.Errorf("another host heard %q", got)
	}

	// What reaches the relay from anybody but the SFU goes no further.
	if _, err := other.WriteTo([]byte("from another port"), relay.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = relay.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, from, err := relay.ReadFrom(make([]byte, 1500)); err == nil {
		t.Fatalf("the client was relayed %d bytes from %v", n, from)
	}
}

func TestATCPRelayIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	conn, err := net.Dial("tcp4", f.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	c := f.clientOn(t, turn.NewSTUNConn(conn), f.tcp.String(), cred(1))
	if _, err := c.AllocateTCP(); err == nil {
		t.Fatal("a TCP allocation, which would relay to any peer over TCP, was granted")
	}
	// The same connection relays over UDP, which is TURN over TCP as a client behind a firewall uses it.
	if _, err := c.Allocate(); err != nil {
		t.Fatalf("an ordinary allocation over TCP: %v", err)
	}
}

func TestCredentials(t *testing.T) {
	f := newFixture(t, nil)
	now := time.Now()
	for name, c := range map[string]Credential{
		"expired":   Mint(testSecret, 1, now.Add(-CredentialLifetime-time.Second)),
		"forged":    Mint([]byte("another instance's secret, also thirty-two bytes"), 1, now),
		"malformed": {Username: "\x1b[31mnot a username\x1b[0m", Password: "x"},
	} {
		if _, err := f.client(t, c).Allocate(); err == nil {
			t.Errorf("%s: allocated", name)
		}
	}
	if f.log.Len() != 0 {
		t.Fatalf("refused credentials wrote to the log: %s", f.log)
	}
}

// TestExpiryIsAskedOfAnAllocateAlone: a refresh, a permission or a channel on an allocation that exists
// passes with a credential past its expiry, so a call outlives the credential that started it.
func TestExpiryIsAskedOfAnAllocateAlone(t *testing.T) {
	f := newFixture(t, nil)
	expired := Mint(testSecret, 7, time.Now().Add(-CredentialLifetime-time.Second))
	ra := func(m stun.Method) *turn.RequestAttributes {
		return &turn.RequestAttributes{Username: expired.Username, Realm: Realm, Method: m}
	}
	if _, _, ok := f.s.authenticate(ra(stun.MethodAllocate)); ok {
		t.Fatal("an expired credential authenticated an Allocate")
	}
	for _, m := range []stun.Method{stun.MethodRefresh, stun.MethodCreatePermission, stun.MethodChannelBind} {
		user, key, ok := f.s.authenticate(ra(m))
		if !ok || user != "7" {
			t.Fatalf("%v with an expired credential: %q, %v", m, user, ok)
		}
		if want := turn.GenerateAuthKey(expired.Username, Realm, expired.Password); !bytes.Equal(key, want) {
			t.Fatalf("%v: the key is not the credential's", m)
		}
	}
	for _, bad := range []string{"", ":", "1:", ":1", "x:1", "1:x", "01:1", "1:01", "-1:1", "1:0",
		strings.Repeat("9", 40) + ":1", "1:1:1"} {
		if _, _, ok := f.s.authenticate(&turn.RequestAttributes{Username: bad, Realm: Realm,
			Method: stun.MethodRefresh}); ok {
			t.Errorf("%q authenticated", bad)
		}
	}
	if _, _, ok := f.s.authenticate(&turn.RequestAttributes{Username: expired.Username, Realm: "other",
		Method: stun.MethodRefresh}); ok {
		t.Error("another realm authenticated")
	}
}

func TestAnAllocationTheSFUDoesNotUseIsClosed(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Limits = DefaultLimits()
		o.Limits.Idle = 300 * time.Millisecond
	})
	relay, err := f.client(t, cred(1)).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	// The SFU answering keeps it.
	for range 8 {
		if _, err := relay.WriteTo([]byte("check"), f.sfuAddr()); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		_ = f.sfu.SetReadDeadline(time.Now().Add(time.Second))
		_, from, err := f.sfu.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.sfu.WriteTo([]byte("answer"), from); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := f.s.AllocationCount(); n != 1 {
		t.Fatalf("an allocation the SFU answers through was closed: %d left", n)
	}
	// Silence closes it.
	eventually(t, 2*time.Second, "the unused allocation closes", func() bool { return f.s.AllocationCount() == 0 })
}

func TestTheRelayIsBoundedPerAllocation(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Limits = DefaultLimits()
		o.Limits.RelayPacketsPerSecond = 20
	})
	relay, err := f.client(t, cred(1)).Allocate()
	if err != nil {
		t.Fatal(err)
	}
	// One second at 2.5x the bound: the bound arrives, the rest does not.
	for range 50 {
		_, _ = relay.WriteTo([]byte("x"), f.sfuAddr())
		time.Sleep(time.Second / 50)
	}
	got := len(received(t, f.sfu, 300*time.Millisecond))
	if got > 2*20 || got < 15 {
		t.Fatalf("%d packets through a bound of 20 a second", got)
	}
	// Seconds past twice it in a row close the allocation.
	for range 4 * 50 {
		_, _ = relay.WriteTo([]byte("x"), f.sfuAddr())
		time.Sleep(time.Second / 50)
	}
	eventually(t, 2*time.Second, "the allocation past twice its bound closes", func() bool {
		return f.s.AllocationCount() == 0
	})
}

func TestAllocationsAreBounded(t *testing.T) {
	t.Run("per account", func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.Limits = DefaultLimits(); o.Limits.AllocationsPerUser = 1 })
		if _, err := f.client(t, cred(1)).Allocate(); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client(t, cred(1)).Allocate(); err == nil {
			t.Fatal("a second allocation for one account")
		}
		if _, err := f.client(t, cred(2)).Allocate(); err != nil {
			t.Fatalf("another account: %v", err)
		}
	})
	t.Run("per source", func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.Limits = DefaultLimits(); o.Limits.AllocationsPerSource = 2 })
		for user := range snowflake.ID(2) {
			if _, err := f.client(t, cred(user+1)).Allocate(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.client(t, cred(9)).Allocate(); err == nil {
			t.Fatal("a third allocation from one source")
		}
	})
	t.Run("in all", func(t *testing.T) {
		f := newFixture(t, func(o *Options) { o.Limits = DefaultLimits(); o.Limits.Allocations = 2 })
		for user := range snowflake.ID(2) {
			if _, err := f.client(t, cred(user+1)).Allocate(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.client(t, cred(9)).Allocate(); err == nil {
			t.Fatal("an allocation past the total")
		}
	})
}

func TestStreamsAreBounded(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Limits = DefaultLimits()
		o.Limits.StreamsPerSource = 1
		o.Limits.AuthenticateWithin = time.Second
	})
	first, err := net.Dial("tcp4", f.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	closed := func(c net.Conn, within time.Duration) bool {
		_ = c.SetReadDeadline(time.Now().Add(within))
		_, err := c.Read(make([]byte, 1))
		var ne net.Error
		return err != nil && (!errors.As(err, &ne) || !ne.Timeout())
	}
	time.Sleep(50 * time.Millisecond)
	second, err := net.Dial("tcp4", f.tcp.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	// Closed at once, well before the deadline to authenticate would close it anyway.
	if !closed(second, 200*time.Millisecond) {
		t.Fatal("a second connection from one source was kept")
	}
	// The first made no allocation, and is closed for it.
	if !closed(first, 3*time.Second) {
		t.Fatal("a connection that never allocated was kept")
	}
}

func TestATLSHandshakeIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(o *Options) {
		o.TLS = ln
		o.TLSConfig = testTLSConfig(t)
		o.Limits = DefaultLimits()
		o.Limits.HandshakeWithin = 200 * time.Millisecond
	})
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	switch {
	case err == nil:
		t.Fatal("a silent TLS connection was answered")
	case errors.As(err, &ne) && ne.Timeout():
		t.Fatal("a silent TLS connection was held past the handshake bound")
	}
	if f.log.Len() != 0 {
		t.Fatalf("a failed handshake wrote to the log: %s", f.log)
	}
}

// TestUnauthenticatedRepliesAreBounded sends Binding requests faster than the bound from one address:
// each reply is larger than its request, so unbounded, a forged source would make TURN a reflector.
func TestUnauthenticatedRepliesAreBounded(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Limits = DefaultLimits(); o.Limits.RepliesPerSecond = 5 })
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	for range 40 {
		if _, err := conn.WriteTo(msg.Raw, f.udp); err != nil {
			t.Fatal(err)
		}
	}
	got := received(t, conn, 500*time.Millisecond)
	if len(got) == 0 || len(got) > 2*5 {
		t.Fatalf("%d replies to 40 requests, against a bound of 5 a second", len(got))
	}
	for _, r := range got {
		if len(r) <= len(msg.Raw) {
			t.Fatalf("a reply of %d bytes to a request of %d: the ledger's amplification figure is wrong", len(r), len(msg.Raw))
		}
	}
}

func TestUnauthenticatedReplyRecognisesWhatPionSends(t *testing.T) {
	plain := stun.MustBuild(stun.TransactionID, stun.BindingSuccess,
		&stun.XORMappedAddress{IP: net.IPv4(203, 0, 113, 7), Port: 9}, stun.Fingerprint)
	challenge := stun.MustBuild(stun.TransactionID, stun.NewType(stun.MethodAllocate, stun.ClassErrorResponse),
		&stun.ErrorCodeAttribute{Code: stun.CodeUnauthorized}, stun.NewNonce("n"), stun.NewRealm(Realm))
	signed := stun.MustBuild(stun.TransactionID, stun.NewType(stun.MethodAllocate, stun.ClassSuccessResponse),
		stun.NewShortTermIntegrity("key"), stun.Fingerprint)
	request := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	for name, c := range map[string]struct {
		raw  []byte
		want bool
	}{
		"a binding response":         {plain.Raw, true},
		"a challenge":                {challenge.Raw, true},
		"an authenticated response":  {signed.Raw, false},
		"a request":                  {request.Raw, false},
		"channel data":               {[]byte{0x40, 0x00, 0x00, 0x04, 1, 2, 3, 4}, false},
		"a truncated header":         {plain.Raw[:10], false},
		"a length past the datagram": {append(bytes.Clone(plain.Raw[:2]), append([]byte{0xff, 0xff}, plain.Raw[4:]...)...), true},
	} {
		if got := unauthenticatedReply(c.raw); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// TestAPanicRefusesTheRequestAndNothingElse panics where a stranger's request is handled: the request is
// refused, the panic is logged once, and the server goes on serving.
func TestAPanicRefusesTheRequestAndNothingElse(t *testing.T) {
	for _, where := range []string{"authentication", "the relay generator"} {
		t.Run(where, func(t *testing.T) {
			f := newFixture(t, nil)
			hook := func(at string) {
				if at == where {
					panic("planted")
				}
			}
			testPanic.Store(&hook)
			for range 3 {
				if _, err := f.client(t, cred(1)).Allocate(); err == nil {
					t.Fatal("allocated through a panic")
				}
			}
			testPanic.Store(nil)
			if n := strings.Count(f.log.String(), "TURN panicked"); n != 1 {
				t.Fatalf("the panic was logged %d times, want once", n)
			}
			if _, err := f.client(t, cred(2)).Allocate(); err != nil {
				t.Fatalf("after the panic: %v", err)
			}
		})
	}
}

// TestOneHostCannotSpendEverybodysReplies: the total is charged only for replies a destination's bound
// lets through, so one host sending far past its bound costs the total its bound and no more.
func TestOneHostCannotSpendEverybodysReplies(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Limits = DefaultLimits()
		o.Limits.RepliesPerSecond = 5
		o.Limits.RepliesTotalPerSecond = 10
	})
	flood, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = flood.Close() }()
	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	for range 40 {
		if _, err := flood.WriteTo(msg.Raw, f.udp); err != nil {
			t.Fatal(err)
		}
	}
	if got := received(t, flood, 300*time.Millisecond); len(got) == 0 || len(got) > 2*5 {
		t.Fatalf("the flooding host got %d replies", len(got))
	}
	// Another host, asked about as pion's reply to it would be.
	if !f.s.replies.allow(&net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 9}) {
		t.Fatal("another host gets no reply after one host sent 40 requests")
	}
}

// TestPionsLogIsBounded sends Binding requests carrying an unknown comprehension-required attribute,
// which pion logs at error level, quoting the source, before anything here sees them.
func TestPionsLogIsBounded(t *testing.T) {
	f := newFixture(t, nil)
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	unknown := stun.RawAttribute{Type: 0x7fff, Value: []byte{0, 0, 0, 0}}
	for range 100 {
		msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest, unknown)
		if _, err := conn.WriteTo(msg.Raw, f.udp); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, time.Second, "pion logs the first", func() bool { return f.log.Len() > 0 })
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(f.log.String(), "\n"); n > pionLogBurst {
		t.Fatalf("pion wrote %d lines for 100 packets, past its burst of %d", n, pionLogBurst)
	}
}

// TestEachPlaceAPanicHappensIsLogged: one panic logged does not silence a second bug somewhere else.
func TestEachPlaceAPanicHappensIsLogged(t *testing.T) {
	f := newFixture(t, nil)
	for _, where := range []string{"authentication", "the relay generator", "authentication"} {
		hook := func(at string) {
			if at == where {
				panic("planted")
			}
		}
		testPanic.Store(&hook)
		if _, err := f.client(t, cred(1)).Allocate(); err == nil {
			t.Fatal("allocated through a panic")
		}
	}
	testPanic.Store(nil)
	if n := strings.Count(f.log.String(), "TURN panicked"); n != 2 {
		t.Fatalf("%d lines for panics in two places, want two:\n%s", n, f.log)
	}
}

// TestAPanicInAnAllocationEventIsContained: pion calls the events on its own goroutine, where an
// uncontained panic ends norite-server.
func TestAPanicInAnAllocationEventIsContained(t *testing.T) {
	f := newFixture(t, nil)
	hook := func(at string) {
		if at == "the allocation events" {
			panic("planted")
		}
	}
	testPanic.Store(&hook)
	defer testPanic.Store(nil)
	_, _ = f.client(t, cred(1)).Allocate()
	eventually(t, time.Second, "the panic is logged", func() bool {
		return strings.Contains(f.log.String(), "the allocation events")
	})
}

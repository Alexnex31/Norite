// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/voice/turn"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
)

// TestMain trusts a certificate made for 127.0.0.1, which the TURNS listener serves. Pion verifies a
// TURNS server against the system's roots and offers no other way, so the roots are this certificate:
// Go reads SSL_CERT_FILE once, the first time anything verifies.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "voice-tls")
	if err != nil {
		panic(err)
	}
	cert, file, err := selfSigned(dir)
	if err != nil {
		panic(err)
	}
	testCertificate = cert
	_ = os.Setenv("SSL_CERT_FILE", file)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var testCertificate tls.Certificate

func selfSigned(dir string) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "voice test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	file := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, file, nil
}

// relayed is an SFU with TURN beside it, on loopback, as the stack runs them.
type relayed struct {
	sfu    *SFU
	turn   *turn.Server
	secret []byte
	// udp, tcp and tls are TURN's listeners' addresses.
	udp, tcp, tls string
}

func newRelayed(t *testing.T, tune func(*turn.Options)) relayed {
	t.Helper()
	loopback := netip.MustParseAddr("127.0.0.1")
	s := newSFUWith(t, func(o *Options) { o.Relay = loopback })
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := turn.DeriveSecret([]byte("a signing key for tests, long enough to be one"))
	if err != nil {
		t.Fatal(err)
	}
	opts := turn.Options{
		UDP:       udp,
		TCP:       tcp,
		TLS:       tlsLn,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{testCertificate}, MinVersion: tls.VersionTLS12},
		Secret:    secret,
		Relay:     loopback,
		SFU:       s.opts.Conn.LocalAddr().(*net.UDPAddr).AddrPort(),
		Logger:    zerolog.Nop(),
	}
	if tune != nil {
		tune(&opts)
	}
	srv, err := turn.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return relayed{sfu: s, turn: srv, secret: secret, udp: udp.LocalAddr().String(), tcp: tcp.Addr().String(),
		tls: tlsLn.Addr().String()}
}

// server is the ICE server a client is given for one transport, with a credential for user.
func (r relayed) server(transport string, user turn.Credential) webrtc.ICEServer {
	url := map[string]string{
		"udp": "turn:" + r.udp + "?transport=udp",
		"tcp": "turn:" + r.tcp + "?transport=tcp",
		"tls": "turns:" + r.tls + "?transport=tcp",
	}[transport]
	return webrtc.ICEServer{URLs: []string{url}, Username: user.Username, Credential: user.Password}
}

// TestAClientWithNoUDPJoinsThroughTheRelay is the milestone's done-when: a client offering nothing but a
// relayed candidate, over each of TURN's transports, exchanges real Opus with a client that reaches the
// SFU directly, every payload intact.
func TestAClientWithNoUDPJoinsThroughTheRelay(t *testing.T) {
	for _, transport := range []string{"udp", "tcp", "tls"} {
		t.Run(transport, func(t *testing.T) {
			r := newRelayed(t, nil)
			room, user := ids.MustNext(), ids.MustNext()
			cred := turn.Mint(r.secret, user, time.Now())
			a := joinWith(t, r.sfu, room, user, voicetest.Options{
				Slots:      1,
				ICEServers: []webrtc.ICEServer{r.server(transport, cred)},
				RelayOnly:  true,
			})
			b := join(t, r.sfu, room, ids.MustNext(), 1)
			if n := r.turn.AllocationCount(); n != 1 {
				t.Fatalf("%d allocations, want the relayed client's one", n)
			}
			if got := a.client.SelectedCandidateType(); got != webrtc.ICECandidateTypeRelay {
				t.Fatalf("the relayed client connected through a %s candidate", got)
			}

			tone := voicetest.TonePackets()
			send(t, map[*voicetest.Client][][]byte{a.client: tone, b.client: tone}, voicetest.SendOptions{Level: voicetest.Loud})
			assertTone(t, "the relayed client", a.client, tone)
			assertTone(t, "the direct client", b.client, tone)
		})
	}
}

// TestAnExpiredCredentialStartsNoAllocation offers a relay whose credential expired a second ago: the
// client gathers no relayed candidate, and with nothing else to offer, never connects.
func TestAnExpiredCredentialStartsNoAllocation(t *testing.T) {
	r := newRelayed(t, nil)
	for name, cred := range map[string]turn.Credential{
		"expired": turn.Mint(r.secret, ids.MustNext(), time.Now().Add(-turn.CredentialLifetime-time.Second)),
		"forged":  turn.Mint([]byte("not this instance's secret, though long enough"), ids.MustNext(), time.Now()),
	} {
		c := newClientWith(t, voicetest.Options{Slots: 1, ICEServers: []webrtc.ICEServer{r.server("udp", cred)},
			RelayOnly: true})
		offer := offerOf(t, c)
		if n := countCandidates(offer); n != 0 {
			t.Fatalf("%s: the offer carries %d candidates", name, n)
		}
		if n := r.turn.AllocationCount(); n != 0 {
			t.Fatalf("%s: %d allocations", name, n)
		}
	}
}

func countCandidates(sdp string) int { return strings.Count(sdp, "a=candidate:") }

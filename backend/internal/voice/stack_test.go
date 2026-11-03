// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/voice/turn"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
)

var testSigningKey = []byte("a signing key for tests, long enough to be one")

func stackOptions(t *testing.T) StackOptions {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "turn.pem"), filepath.Join(dir, "turn.key")
	der, err := x509.MarshalPKCS8PrivateKey(testCertificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: testCertificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	return StackOptions{
		MediaListen:   "127.0.0.1:0",
		TURNListen:    freePort(t),
		TURNTLSListen: "127.0.0.1:0",
		TLSCert:       cert,
		TLSKey:        key,
		Relay:         loopback,
		SFU:           netip.AddrPortFrom(loopback, 0),
		SigningKey:    testSigningKey,
		Loopback:      true,
		Version:       "dev",
		Logger:        zerolog.Nop(),
	}
}

// freePort is a loopback address whose port is free on UDP and TCP alike, as TURN's must be.
func freePort(t *testing.T) string {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		if pc, err := net.ListenPacket("udp4", addr); err == nil {
			_ = pc.Close()
			return addr
		}
	}
	t.Fatal("no port free on both")
	return ""
}

func TestTheStackRelaysToItsOwnSFU(t *testing.T) {
	st, err := StartStack(stackOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	user := ids.MustNext()
	cred := turn.Mint(st.Secret, user, time.Now())
	a := joinWith(t, st.SFU, 1, user, voicetest.Options{Slots: 1, RelayOnly: true, ICEServers: []webrtc.ICEServer{{
		URLs:     []string{"turn:" + st.TURN.TCPAddr().String() + "?transport=tcp"},
		Username: cred.Username, Credential: cred.Password,
	}}})
	b := join(t, st.SFU, 1, ids.MustNext(), 1)
	tone := voicetest.TonePackets()[:50]
	send(t, map[*voicetest.Client][][]byte{a.client: tone, b.client: tone}, voicetest.SendOptions{Level: voicetest.Loud})
	assertTone(t, "the relayed client", a.client, tone)
	assertTone(t, "the direct client", b.client, tone)
}

// TestAStackThatCannotStartLeavesNothingBound starts a stack whose TURN port is taken: it fails, and the
// media port it had bound is free again.
func TestAStackThatCannotStartLeavesNothingBound(t *testing.T) {
	opts := stackOptions(t)
	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	opts.MediaListen = freePort(t)
	opts.TURNListen = taken.Addr().String()
	if _, err := StartStack(opts); err == nil {
		t.Fatal("a stack started on a port in use")
	}
	pc, err := net.ListenPacket("udp4", opts.MediaListen)
	if err != nil {
		t.Fatalf("the media port is still held: %v", err)
	}
	_ = pc.Close()
}

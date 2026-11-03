// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func writePair(t *testing.T, dir string, at time.Time) []byte {
	t.Helper()
	cert := testTLSConfig(t).Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "turn.pem"), filepath.Join(dir, "turn.key")
	for name, block := range map[string]*pem.Block{
		certFile: {Type: "CERTIFICATE", Bytes: cert.Certificate[0]},
		keyFile:  {Type: "PRIVATE KEY", Bytes: key},
	} {
		if err := os.WriteFile(name, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return cert.Certificate[0]
}

func TestARenewedCertificateIsServedWithoutARestart(t *testing.T) {
	old := recheck
	recheck = 0
	t.Cleanup(func() { recheck = old })

	dir := t.TempDir()
	first := writePair(t, dir, time.Now().Add(-time.Hour))
	var log bytes.Buffer
	config, err := CertificateFiles(filepath.Join(dir, "turn.pem"), filepath.Join(dir, "turn.key"), zerolog.New(&log))
	if err != nil {
		t.Fatal(err)
	}
	serving := func() []byte {
		c, err := config.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Certificate[0]
	}
	if !bytes.Equal(serving(), first) {
		t.Fatal("not serving the certificate in the files")
	}

	second := writePair(t, dir, time.Now())
	if !bytes.Equal(serving(), second) {
		t.Fatal("a renewed certificate was not picked up")
	}

	// A renewal half written keeps the certificate that works, and says so once.
	if err := os.WriteFile(filepath.Join(dir, "turn.pem"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if !bytes.Equal(serving(), second) {
			t.Fatal("an unreadable renewal replaced a working certificate")
		}
	}
	if n := bytes.Count(log.Bytes(), []byte("could not be read")); n != 1 {
		t.Fatalf("the unreadable renewal was logged %d times", n)
	}

	if _, err := CertificateFiles(filepath.Join(dir, "missing.pem"), filepath.Join(dir, "turn.key"), zerolog.Nop()); err == nil {
		t.Fatal("a missing certificate started")
	}
}

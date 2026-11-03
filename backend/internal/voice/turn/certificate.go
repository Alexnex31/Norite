// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// recheck is how often the certificate's files are looked at again, at most: on a handshake, never on a
// timer, so an idle server reads nothing.
var recheck = 10 * time.Second

// CertificateFiles is a TLS configuration serving the certificate in two PEM files, read again when either
// changes on disk. The files are the ones a reverse proxy or certbot already renews for the instance's
// name, so a renewal reaches TURN without a restart. A renewal that cannot be read keeps the certificate
// that could, and says so once.
func CertificateFiles(certFile, keyFile string, log zerolog.Logger) (*tls.Config, error) {
	f := &certFiles{certFile: certFile, keyFile: keyFile, log: log}
	if err := f.load(); err != nil {
		return nil, err
	}
	return &tls.Config{GetCertificate: f.get, MinVersion: tls.VersionTLS12}, nil
}

type certFiles struct {
	certFile, keyFile string
	log               zerolog.Logger

	// cert is what every handshake serves, read without a lock.
	cert atomic.Pointer[tls.Certificate]

	// mu is the one handshake looking at the files, taken by TryLock: a slow disk delays that handshake
	// and no other, which go on serving the certificate they have.
	mu      sync.Mutex
	stamp   [2]time.Time
	checked time.Time
	warned  bool
}

func (f *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if f.mu.TryLock() {
		if time.Since(f.checked) >= recheck {
			f.checked = time.Now()
			if stamp, err := f.stamps(); err == nil && stamp != f.stamp {
				if err := f.loadLocked(); err != nil && !f.warned {
					f.warned = true
					f.log.Warn().Err(err).Msg("TURN's certificate changed and could not be read; keeping the last one")
				}
			}
		}
		f.mu.Unlock()
	}
	return f.cert.Load(), nil
}

func (f *certFiles) load() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = time.Now()
	return f.loadLocked()
}

func (f *certFiles) loadLocked() error {
	stamp, err := f.stamps()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(f.certFile, f.keyFile)
	if err != nil {
		return fmt.Errorf("turn: the certificate: %w", err)
	}
	f.cert.Store(&cert)
	f.stamp, f.warned = stamp, false
	return nil
}

func (f *certFiles) stamps() ([2]time.Time, error) {
	var stamp [2]time.Time
	for i, name := range []string{f.certFile, f.keyFile} {
		st, err := os.Stat(name)
		if err != nil {
			return stamp, fmt.Errorf("turn: the certificate: %w", err)
		}
		stamp[i] = st.ModTime()
	}
	return stamp, nil
}

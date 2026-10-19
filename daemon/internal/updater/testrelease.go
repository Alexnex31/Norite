// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build norite_testrelease

package updater

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"time"
)

// This file is in no release. It is compiled only with the norite_testrelease tag, which nothing in
// .goreleaser.yaml, the justfile's build recipes or the installers passes, and a test holds a build
// without the tag to not containing any of the names below (cmd/daemond).
//
// It exists because two clauses of M24's done-when cannot be walked with a real release: a release that
// crashes at start, signed by the real workflow, would be a public one, and so would a second and third
// version to move between. So a daemon built with this tag takes its releases from a host a test runs, and
// believes a signature made with a key the test holds, and can be told to crash. Everything else is the
// code a release runs: the listing, the choosing, the download, the checksum, the unpacking, the swap, the
// restart and the rollback.
//
// What it replaces is the one thing a release build must never let anything replace, which is why it is a
// build tag and not a flag, a file or an environment variable: there is no value a released daemon can be
// given that makes it believe another signer.
func init() {
	if host := os.Getenv("NORITE_TESTRELEASE_HOST"); host != "" {
		sourceOverride = func(version string) *Source {
			return &Source{ListURL: host + "/releases", DownloadBase: host + "/download",
				UserAgent: "norite-daemon/" + version, Client: newHTTPClient()}
		}
	}
	if encoded := os.Getenv("NORITE_TESTRELEASE_KEY"); encoded != "" {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != ed25519.PublicKeySize {
			panic("NORITE_TESTRELEASE_KEY is not an ed25519 public key")
		}
		// The "bundle" is a signature over the tag and the checksums, so that here too one version's
		// cannot stand for another's.
		verifyOverride = func(checksums, bundle []byte, tag string) error {
			sig, err := base64.StdEncoding.DecodeString(string(bundle))
			if err != nil || !ed25519.Verify(key, append([]byte(tag+"\n"), checksums...), sig) {
				return fmt.Errorf("%w: not signed with the test key", ErrNotVerified)
			}
			return nil
		}
	}
	if os.Getenv("NORITE_TESTRELEASE_FAST") != "" {
		firstCheck, checkJitter, checkEvery, retryStaged = 300*time.Millisecond, 0, time.Second, time.Second
		confirmAfter = 4 * time.Second
	}
	if version := os.Getenv("NORITE_TESTRELEASE_CRASH"); version != "" {
		startedHook = func(running string) {
			if running == version {
				// On a goroutine of its own, as a real crash is: a panic on the daemon's own would run its
				// deferred calls first, one of which stops directing the traceback to the crash file.
				go func() { panic("norite_testrelease: this version was told to crash at start") }()
				time.Sleep(time.Minute)
			}
		}
	}
}

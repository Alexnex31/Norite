// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command rootfetch writes Sigstore's current trusted root to a file, for `just trusted-root`.
//
// It is the one place this repository talks to Sigstore, and it runs on a developer's machine or in CI,
// never in the daemon: the daemon verifies against the copy this wrote and somebody committed (ADR 0020).
// The root is fetched through TUF, starting from the TUF root sigstore-go embeds, so what is written is
// what Sigstore's root keys signed and not merely what a server returned.
package main

import (
	"fmt"
	"os"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: rootfetch <output file>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "rootfetch:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	cache, err := os.MkdirTemp("", "norite-tuf-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(cache) }()

	// A cache of this run's own, so the answer never depends on what an earlier run left in a home.
	opts := tuf.DefaultOptions()
	opts.CachePath = cache
	client, err := tuf.New(opts)
	if err != nil {
		return fmt.Errorf("starting TUF: %w", err)
	}
	raw, err := client.GetTarget("trusted_root.json")
	if err != nil {
		return fmt.Errorf("fetching the trusted root: %w", err)
	}
	// Parsed before it is written: a file the verifier cannot read must not replace one it can.
	if _, err := root.NewTrustedRootFromJSON(raw); err != nil {
		return fmt.Errorf("the fetched root does not parse: %w", err)
	}
	return os.WriteFile(out, raw, 0o644) //nolint:gosec // a public file, committed
}

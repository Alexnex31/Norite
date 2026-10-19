// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"errors"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

// The published alpha tests the embedded root against a signature Sigstore issued, and can only be
// tampered with. These sign with an authority made here, to ask what the real one never lets a test ask:
// a signature that is valid in every way and names somebody else.
func TestOnlyThisRepositorysReleaseWorkflowIsBelieved(t *testing.T) {
	authority, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	v, err := newVerifierOn(authority, false)
	if err != nil {
		t.Fatal(err)
	}
	checksums := []byte("aa  norite_0.1.1_linux_amd64.tar.gz\n")
	const tag = "v0.1.1"

	cases := []struct {
		name             string
		identity, issuer string
		ok               bool
	}{
		{name: "this repository's release workflow, at this tag", identity: releaseWorkflow + tag, issuer: oidcIssuer, ok: true},
		{name: "a fork's release workflow", issuer: oidcIssuer,
			identity: "https://github.com/Somebody/Norite/.github/workflows/release.yml@refs/tags/" + tag},
		{name: "another workflow in this repository", issuer: oidcIssuer,
			identity: "https://github.com/Alexnex31/Norite/.github/workflows/ci.yml@refs/tags/" + tag},
		{name: "the release workflow run on a branch", issuer: oidcIssuer,
			identity: "https://github.com/Alexnex31/Norite/.github/workflows/release.yml@refs/heads/main"},
		{name: "the release workflow at another tag", identity: releaseWorkflow + "v0.1.0", issuer: oidcIssuer},
		{name: "a tag this one is the beginning of", identity: releaseWorkflow + tag + "0", issuer: oidcIssuer},
		{name: "the right name, vouched for by somebody other than GitHub", identity: releaseWorkflow + tag,
			issuer: "https://accounts.google.com"},
		{name: "a person's address", identity: "somebody@example.com", issuer: "https://github.com/login/oauth"},
	}
	for _, tc := range cases {
		signed, err := authority.Sign(tc.identity, tc.issuer, checksums)
		if err != nil {
			t.Fatalf("%s: signing: %v", tc.name, err)
		}
		err = v.verifyEntity(checksums, signed, tag)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case !tc.ok && !errors.Is(err, ErrNotVerified):
			t.Errorf("%s: verified (%v)", tc.name, err)
		}
	}

	// Signed properly, over another file.
	signed, err := authority.Sign(releaseWorkflow+tag, oidcIssuer, []byte("bb  norite_0.1.1_linux_amd64.tar.gz\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.verifyEntity(checksums, signed, tag); !errors.Is(err, ErrNotVerified) {
		t.Errorf("a signature over another checksums.txt verified: %v", err)
	}
}

// A signature that is right in every way, from an authority the binary does not carry.
func TestAnAuthorityTheBinaryDoesNotCarryIsNotBelieved(t *testing.T) {
	stranger, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	checksums := []byte("aa  norite_0.1.1_linux_amd64.tar.gz\n")
	signed, err := stranger.Sign(releaseWorkflow+"v0.1.1", oidcIssuer, checksums)
	if err != nil {
		t.Fatal(err)
	}

	// Believed by the authority that made it, so the refusals below are about whose it is.
	own, err := newVerifierOn(stranger, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := own.verifyEntity(checksums, signed, "v0.1.1"); err != nil {
		t.Fatalf("the authority's own signature did not verify: %v", err)
	}

	embedded, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if err := embedded.verifyEntity(checksums, signed, "v0.1.1"); !errors.Is(err, ErrNotVerified) {
		t.Errorf("the embedded root believed a stranger's authority: %v", err)
	}
	another, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	other, err := newVerifierOn(another, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.verifyEntity(checksums, signed, "v0.1.1"); !errors.Is(err, ErrNotVerified) {
		t.Errorf("one authority believed another's signature: %v", err)
	}
}

// A certificate with no proof that it was written to a certificate log is refused by every Verifier a
// release build makes. The authority a test makes issues exactly such certificates, so its own signature,
// right in every other way, shows the requirement is in force.
func TestACertificateNobodyLoggedIsRefused(t *testing.T) {
	authority, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	checksums := []byte("aa  norite_0.1.1_linux_amd64.tar.gz\n")
	signed, err := authority.Sign(releaseWorkflow+"v0.1.1", oidcIssuer, checksums)
	if err != nil {
		t.Fatal(err)
	}
	lenient, err := newVerifierOn(authority, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := lenient.verifyEntity(checksums, signed, "v0.1.1"); err != nil {
		t.Fatalf("without the requirement it verifies, or this test shows nothing: %v", err)
	}
	// And the one a release build makes is the strict one. The embedded root cannot be made to sign a
	// certificate without the proof, so this is asked of the Verifier itself.
	released, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if !released.logged {
		t.Error("the Verifier a release build makes does not require the log proof")
	}
	strict, err := newVerifierOn(authority, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := strict.verifyEntity(checksums, signed, "v0.1.1"); !errors.Is(err, ErrNotVerified) {
		t.Errorf("a certificate with no log proof verified: %v", err)
	}
}

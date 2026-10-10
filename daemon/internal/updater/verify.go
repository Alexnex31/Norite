// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package updater finds, verifies, installs and undoes a release of the client (M24, ADR 0020).
//
// Everything it reads from the network is somebody else's until a signature says otherwise: the listing is
// a hint, a download is bytes, and the only thing believed is checksums.txt once its bundle has verified
// against this repository's release workflow at the candidate's own tag.
package updater

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

const (
	// releaseWorkflow is the identity a release's certificate must name, less the tag: this repository's
	// release.yml, which is the only thing that publishes (ADR 0033). It is .goreleaser.yaml's footer and
	// scripts/install.sh's, and changes with them.
	releaseWorkflow = "https://github.com/Alexnex31/Norite/.github/workflows/release.yml@refs/tags/"
	// oidcIssuer is who vouched for that identity: GitHub Actions' token service.
	oidcIssuer = "https://token.actions.githubusercontent.com"
)

// embeddedRoot is Sigstore's public-good trusted root: the certificate authority, the transparency and
// certificate logs' keys and the timestamp authority a bundle is checked against. Embedded so that
// verifying asks nobody anything (ADR 0020). `just trusted-root` regenerates it.
//
//go:embed trusted_root.json
var embeddedRoot []byte

// ErrNotVerified is a release whose signature did not verify. Whatever it wraps, nothing of that release
// is used.
var ErrNotVerified = errors.New("the release's signature did not verify")

// Verifier checks that a checksums.txt is the one this repository's release workflow signed for a tag.
type Verifier struct {
	verifier *verify.Verifier
	// logged is whether a certificate must prove it was written to a certificate log. See newVerifierOn.
	logged bool
}

// NewVerifier builds a Verifier on the trusted root.
func NewVerifier() (*Verifier, error) {
	trusted, err := root.NewTrustedRootFromJSON(embeddedRoot)
	if err != nil {
		return nil, fmt.Errorf("reading the trusted root: %w", err)
	}
	return newVerifierOn(trusted, true)
}

// newVerifierOn builds a Verifier that trusts what material says to trust. NewVerifier passes the embedded
// root and nothing else in a release build does; the tests pass an authority they made, to sign things the
// real one never would.
//
// logged is whether the certificate must carry proof that it was written to a certificate log when it
// was issued. It is true for every Verifier that is not a test's: the authority a test makes issues
// certificates with no such proof, which is exactly what a real one must be refused for.
func newVerifierOn(trusted root.TrustedMaterial, logged bool) (*Verifier, error) {
	// All three, as cosign's own verify-blob asks by default: the certificate was logged when it was
	// issued, the signature was logged when it was made, and something other than this machine's clock
	// says when that was. The last is what lets a ten-minute certificate be checked years later, and what
	// keeps a wrong clock here from deciding anything.
	options := []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	if logged {
		options = append(options, verify.WithSignedCertificateTimestamps(1))
	}
	v, err := verify.NewVerifier(trusted, options...)
	if err != nil {
		return nil, fmt.Errorf("building the verifier: %w", err)
	}
	return &Verifier{verifier: v, logged: logged}, nil
}

// Verify reports whether bundleJSON is the release workflow's signature over checksums, made for tag.
//
// The tag is part of the identity, so a bundle made for one version does not verify for another, and the
// caller's tag is the version it is about to install: it must be a version, exactly as a release is
// tagged, before it is put into an identity at all. That check cannot be proved by removing it, since the
// identity is compared whole and no malformed tag equals one a certificate holds; it is here so that
// nothing else has to be true for that to stay so.
func (v *Verifier) Verify(checksums, bundleJSON []byte, tag string) error {
	if len(tag) < 2 || tag[0] != 'v' {
		return fmt.Errorf("%w: %q is not a release tag", ErrNotVerified, tag)
	}
	if _, err := gatewayproto.ParseVersion(tag); err != nil {
		return fmt.Errorf("%w: %q is not a release tag", ErrNotVerified, tag)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("%w: the bundle cannot be read: %w", ErrNotVerified, err)
	}
	return v.verifyEntity(checksums, &b, tag)
}

// verifyEntity is Verify once the bundle has been read.
func (v *Verifier) verifyEntity(checksums []byte, signed verify.SignedEntity, tag string) error {
	identity, err := verify.NewShortCertificateIdentity(oidcIssuer, "", releaseWorkflow+tag, "")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotVerified, err)
	}
	policy := verify.NewPolicy(verify.WithArtifact(bytes.NewReader(checksums)), verify.WithCertificateIdentity(identity))
	if _, err := v.verifier.Verify(signed, policy); err != nil {
		return fmt.Errorf("%w: %w", ErrNotVerified, err)
	}
	return nil
}

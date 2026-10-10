// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// The fixtures are the real thing: v0.1.0-alpha's checksums.txt and the bundle the release workflow signed
// it with, as published. So this is the embedded root verifying a signature Sigstore actually issued, with
// no network, which is the first half of the milestone's done-when.
func alphaRelease(t *testing.T) (checksums, bundle []byte) {
	t.Helper()
	checksums, err := os.ReadFile("testdata/v0.1.0-alpha.checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err = os.ReadFile("testdata/v0.1.0-alpha.checksums.txt.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	return checksums, bundle
}

func TestTheRealAlphaReleaseVerifiesOffline(t *testing.T) {
	v, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	checksums, bundle := alphaRelease(t)
	if err := v.Verify(checksums, bundle, "v0.1.0-alpha"); err != nil {
		t.Fatalf("the published alpha did not verify: %v", err)
	}
}

func TestABundleVerifiesOnlyForItsOwnTag(t *testing.T) {
	v, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	checksums, bundle := alphaRelease(t)
	// A real signature, presented as a later version's: what a host serving an old release under a new
	// name would send.
	for _, tag := range []string{"v0.1.0", "v0.1.0-alpha.1", "v0.2.0", "v9.9.9"} {
		if err := v.Verify(checksums, bundle, tag); !errors.Is(err, ErrNotVerified) {
			t.Errorf("the alpha's bundle verified as %s: %v", tag, err)
		}
	}
}

func TestOnlyAReleaseTagIsPutIntoAnIdentity(t *testing.T) {
	v, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	checksums, bundle := alphaRelease(t)
	for _, tag := range []string{"", "v", "0.1.0-alpha", "v0.1", "vnext", "v0.1.0-alpha/../v0.1.0-alpha", "v0.1.0-alpha\n", ".*"} {
		if err := v.Verify(checksums, bundle, tag); !errors.Is(err, ErrNotVerified) {
			t.Errorf("tag %q: %v", tag, err)
		}
	}
}

func TestAChangedFileDoesNotVerify(t *testing.T) {
	v, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	checksums, bundle := alphaRelease(t)

	// One hash changed: an archive swapped for another.
	swapped := bytes.Replace(checksums, []byte("ef6a6abf"), []byte("ef6a6abe"), 1)
	if bytes.Equal(swapped, checksums) {
		t.Fatal("the fixture does not hold the hash this test changes")
	}
	if err := v.Verify(swapped, bundle, "v0.1.0-alpha"); !errors.Is(err, ErrNotVerified) {
		t.Errorf("a changed checksums.txt verified: %v", err)
	}
	// A line added: an archive the workflow never built.
	added := append(append([]byte{}, checksums...), []byte("00  norite_0.1.0-alpha_plan9_amd64.tar.gz\n")...)
	if err := v.Verify(added, bundle, "v0.1.0-alpha"); !errors.Is(err, ErrNotVerified) {
		t.Errorf("a lengthened checksums.txt verified: %v", err)
	}
}

func TestABundleThatIsNotOneDoesNotVerify(t *testing.T) {
	v, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	checksums, bundle := alphaRelease(t)

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(bundle, &parsed); err != nil {
		t.Fatal(err)
	}
	without := func(key string) []byte {
		t.Helper()
		var material map[string]json.RawMessage
		if err := json.Unmarshal(parsed["verificationMaterial"], &material); err != nil {
			t.Fatal(err)
		}
		if _, ok := material[key]; !ok {
			t.Fatalf("the fixture's bundle has no %s", key)
		}
		delete(material, key)
		copyOf := map[string]json.RawMessage{}
		for k, v := range parsed {
			copyOf[k] = v
		}
		copyOf["verificationMaterial"], _ = json.Marshal(material)
		out, _ := json.Marshal(copyOf)
		return out
	}
	cases := map[string][]byte{
		"empty":           nil,
		"not JSON":        []byte("checksums"),
		"an empty object": []byte("{}"),
		"no log entry":    without("tlogEntries"),
		"no certificate":  without("certificate"),
		"the file itself": checksums,
	}
	for name, b := range cases {
		if err := v.Verify(checksums, b, "v0.1.0-alpha"); !errors.Is(err, ErrNotVerified) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

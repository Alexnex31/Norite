// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The daemon built as a release builds it believes one signer and takes its releases from one host, and
// nothing it can be given changes either. The norite_testrelease tag is the one way to a daemon that can
// be told otherwise, and this holds a build without the tag to not containing that code at all: none of
// the names it reads are in the binary.
//
// It is proved able to see them by building with the tag as well, where they must be found.
func TestAReleaseBuildCannotBeToldToBelieveAnotherSigner(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the daemon twice")
	}
	dir := t.TempDir()
	build := func(name string, args ...string) []byte {
		t.Helper()
		binary := filepath.Join(dir, name)
		cmd := exec.Command("go", append(append([]string{"build"}, args...), "-o", binary, ".")...) //nolint:gosec // fixed arguments
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", name, err, out)
		}
		body, err := os.ReadFile(binary) //nolint:gosec // the binary just built
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	released := build("released")
	tagged := build("tagged", "-tags", "norite_testrelease")
	for _, name := range []string{"NORITE_TESTRELEASE_HOST", "NORITE_TESTRELEASE_KEY", "NORITE_TESTRELEASE_FAST",
		"NORITE_TESTRELEASE_CRASH", "NORITE_TESTRELEASE"} {
		if !bytes.Contains(tagged, []byte(name)) {
			t.Errorf("the tagged build does not contain %s, so this test cannot see what it looks for", name)
		}
		if bytes.Contains(released, []byte(name)) {
			t.Errorf("a build without the tag contains %s", name)
		}
	}
}

// And no build recipe or release configuration passes the tag.
func TestNothingThatBuildsAReleasePassesTheTag(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{".goreleaser.yaml", "justfile", filepath.Join("scripts", "install.sh"),
		filepath.Join("scripts", "install.ps1"), filepath.Join("scripts", "install-server.sh"),
		filepath.Join(".github", "workflows", "release.yml"), filepath.Join(".github", "workflows", "ci.yml")} {
		body, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // this repository's own files
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(body, []byte("norite_testrelease")) {
			t.Errorf("%s names the norite_testrelease tag", name)
		}
	}
}

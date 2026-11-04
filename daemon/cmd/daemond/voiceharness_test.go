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

// Until M33 the daemon signals voice calls through M26's harness only when built with the
// norite_voiceharness tag (M28). A build without it holds none of that: not the variable naming the
// harness, and not the harness's requests, which the linker drops once nothing reaches them. It is proved
// able to see both by building with the tag, where they must be found.
func TestAReleaseBuildCannotBeToldToSignalThroughAHarness(t *testing.T) {
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
	tagged := build("tagged", "-tags", "norite_voiceharness")
	for _, s := range []string{"NORITE_VOICE_HARNESS", "/provision", "the voice server answered with no answer"} {
		if !bytes.Contains(tagged, []byte(s)) {
			t.Errorf("the tagged build does not contain %s, so this test cannot see what it looks for", s)
		}
		if bytes.Contains(released, []byte(s)) {
			t.Errorf("a build without the tag contains %s", s)
		}
	}
}

// And no build recipe or release configuration passes the tag.
func TestNothingThatBuildsAReleasePassesTheVoiceHarnessTag(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{".goreleaser.yaml", "justfile", filepath.Join("scripts", "install.sh"),
		filepath.Join("scripts", "install.ps1"), filepath.Join("scripts", "install-server.sh"),
		filepath.Join(".github", "workflows", "release.yml"), filepath.Join(".github", "workflows", "ci.yml")} {
		body, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // this repository's own files
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(body, []byte("norite_voiceharness")) {
			t.Errorf("%s names the norite_voiceharness tag", name)
		}
	}
}

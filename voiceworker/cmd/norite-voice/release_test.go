// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The worker a release ships carries the WebAssembly codec and the system's devices, and neither of the
// two things that exist for tests (ADR 0036): the test device, which plays a tone instead of the
// microphone, and the native codec, which links libopus through cgo. The first is held off by nothing
// in the program importing it; the second by its build tag, which nothing that builds a release passes,
// and by cgo, which a release build has off.
func TestTheReleasedWorkerHoldsNoTestDeviceAndNoNativeCodec(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	deps := strings.Fields(string(out))
	has := func(pkg string) bool {
		for _, d := range deps {
			if d == pkg {
				return true
			}
		}
		return false
	}
	const mod = "github.com/Alexnex31/Norite/voiceworker/internal/"
	if !has(mod+"opus") || !has(mod+"audio") || !has("github.com/tetratelabs/wazero") {
		t.Fatalf("the worker does not import the codec and the devices, so this test sees nothing: %v", deps)
	}
	if has(mod + "audio/audiotest") {
		t.Error("the released worker imports the test device")
	}
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{".goreleaser.yaml", "justfile", filepath.Join("scripts", "install.sh"),
		filepath.Join("scripts", "install.ps1"), filepath.Join(".github", "workflows", "release.yml"),
		filepath.Join(".github", "workflows", "ci.yml")} {
		body, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // this repository's own files
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(body, []byte("norite_opusnative")) {
			t.Errorf("%s names the norite_opusnative tag", name)
		}
	}
}

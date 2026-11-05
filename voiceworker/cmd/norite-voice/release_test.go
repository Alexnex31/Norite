// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The worker a release ships carries the WebAssembly codec and the system's devices, and none of the three
// things that exist for tests (ADR 0036): the test device, which plays a tone instead of the microphone;
// the native codec, which links libopus through cgo; and a worker with no devices at all. The first is held
// off by nothing in the program importing it, the others by build tags nothing that builds a release
// passes, and the native codec by cgo too, which a release build has off.
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
	// A worker with no devices imports audio too, for the type, so the imports cannot tell it apart: which
	// file the build takes can.
	files := exec.Command("go", "list", "-f", "{{join .GoFiles \" \"}}", ".")
	files.Env = cmd.Env
	got, err := files.Output()
	if err != nil {
		t.Fatal(err)
	}
	if fs := strings.Fields(string(got)); !slices.Contains(fs, "device.go") || slices.Contains(fs, "device_noaudio.go") {
		t.Errorf("the released worker is built from %v", fs)
	}
	// And the worker with no devices still builds: CI runs Linux, where the end-to-end test uses the release's
	// worker, so nothing else there compiles device_noaudio.go.
	if out, err := exec.Command("go", "vet", "-tags", "norite_noaudio", ".").CombinedOutput(); err != nil {
		t.Errorf("the worker with no devices does not build: %v\n%s", err, out)
	}
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{".goreleaser.yaml", "justfile", filepath.Join("scripts", "install.sh"),
		filepath.Join("scripts", "install.ps1"), filepath.Join(".github", "workflows", "release.yml"),
		filepath.Join(".github", "workflows", "ci.yml")} {
		body, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // this repository's own files
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, tag := range []string{"norite_opusnative", "norite_noaudio"} {
			if bytes.Contains(body, []byte(tag)) {
				t.Errorf("%s names the %s tag", name, tag)
			}
		}
	}
}
